/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package v2

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/containerd/ttrpc"
	"github.com/containerd/typeurl/v2"

	eventstypes "github.com/containerd/containerd/api/events"
	task "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/api/types"
	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"

	"github.com/containerd/containerd/v2/core/events/exchange"
	"github.com/containerd/containerd/v2/core/runtime"
	shimmanager "github.com/containerd/containerd/v2/core/shim/manager"
	"github.com/containerd/containerd/v2/pkg/archive"
	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/containerd/containerd/v2/pkg/identifiers"
	"github.com/containerd/containerd/v2/pkg/protobuf"
	ptypes "github.com/containerd/containerd/v2/pkg/protobuf/types"
)

// rootFsDiffTar is the name of the rootfs diff archive written next to a
// checkpoint. It is part of the checkpoint layout produced by CRIU tooling,
// so it must stay in sync with github.com/checkpoint-restore/checkpointctl/lib.RootFsDiffTar.
const rootFsDiffTar = "rootfs-diff.tar"

// taskDeleteState caches the result of a successful task delete on the shim
// instance, so that a Delete retried after a failed shutdown can still return
// the original exit. It is satisfied by the shim instances the manager creates.
type taskDeleteState = shimmanager.DeleteResultRecorder

func cleanupAfterDeadShim(ctx context.Context, id string, mgr *shimmanager.ShimManager, events *exchange.Exchange, bundle *shimmanager.Bundle, runtimeName string) {
	ctx, cancel := shimmanager.CleanupTimeout(ctx)
	defer cancel()

	log.G(ctx).WithField("id", id).Info("cleaning up after shim disconnected")
	response, err := mgr.DeleteBundle(ctx, bundle, runtimeName)
	if err != nil {
		log.G(ctx).WithError(err).WithField("id", id).Warn("failed to clean up after shim disconnected")
	}

	s, err := mgr.Get(ctx, id)
	if err != nil {
		// Task was never started, or its record has already been removed.
		// No need to publish events.
		return
	}

	// If the task delete already succeeded, the shim itself has delivered the
	// exit and delete events. No need to publish duplicates.
	if dr, ok := s.(taskDeleteState); ok && dr.DeleteResult() != nil {
		return
	}

	var (
		pid        uint32
		exitStatus uint32
		exitedAt   time.Time
	)
	if response != nil {
		pid = response.Pid
		exitStatus = response.Status
		exitedAt = response.Timestamp
	} else {
		exitStatus = 255
		exitedAt = time.Now()
	}
	events.Publish(ctx, runtime.TaskExitEventTopic, &eventstypes.TaskExit{
		ContainerID: id,
		ID:          id,
		Pid:         pid,
		ExitStatus:  exitStatus,
		ExitedAt:    protobuf.ToTimestamp(exitedAt),
	})

	events.Publish(ctx, runtime.TaskDeleteEventTopic, &eventstypes.TaskDelete{
		ContainerID: id,
		Pid:         pid,
		ExitStatus:  exitStatus,
		ExitedAt:    protobuf.ToTimestamp(exitedAt),
	})
}

// cleanupShimTask reaps a shim task we have given up on, after a failed start or
// when loading a bundle left behind by a previous containerd. An unresponsive
// shim must not block the caller — on the load path that would stall containerd
// startup — so each call is bounded, and detached from the caller's context,
// which by then may be cancelled or out of budget.
//
// A failed delete returns before shutting the shim down and closing its client,
// so both are done here. It also leaves the bundle in place (only a successful
// delete removes it), so callers that own one must remove it on error. The shim
// map is untouched: callers reach this having already removed the task, or never
// added it.
func cleanupShimTask(ctx context.Context, st *shimTask) error {
	dctx, cancel := shimmanager.CleanupTimeout(context.WithoutCancel(ctx))
	defer cancel()

	_, err := st.delete(dctx, func(context.Context, string) {})
	if err == nil {
		return nil
	}

	// Shutting down needs a context with time left on it. Check the deadline
	// rather than the error: a timeout only survives as context.DeadlineExceeded
	// over GRPC. Over TTRPC it arrives as the raw context error, which carries no
	// GRPC status, so errgrpc.ToNative flattens it into errdefs.ErrUnknown.
	if dctx.Err() != nil {
		dctx, cancel = shimmanager.CleanupTimeout(context.WithoutCancel(ctx))
		defer cancel()
	}

	st.Shutdown(dctx)
	st.Close()

	return err
}

var _ runtime.Task = &shimTask{}

// shimTask wraps shim process and adds task service client for compatibility with existing shim manager.
type shimTask struct {
	shimmanager.ShimInstance
	task TaskServiceClient
}

func newShimTask(shim shimmanager.ShimInstance) (*shimTask, error) {
	_, version := shim.Endpoint()
	taskClient, err := NewTaskClient(shim.Client(), version)
	if err != nil {
		return nil, err
	}

	return &shimTask{
		ShimInstance: shim,
		task:         taskClient,
	}, nil
}

func (s *shimTask) Shutdown(ctx context.Context) error {
	_, err := s.task.Shutdown(ctx, &task.ShutdownRequest{
		ID: s.ID(),
	})
	if err != nil && !errors.Is(err, ttrpc.ErrClosed) {
		return errgrpc.ToNative(err)
	}
	return nil
}

func (s *shimTask) waitShutdown(ctx context.Context) error {
	ctx, cancel := shimmanager.ShutdownTimeout(ctx)
	defer cancel()
	return s.Shutdown(ctx)
}

// PID of the task
func (s *shimTask) PID(ctx context.Context) (uint32, error) {
	response, err := s.task.Connect(ctx, &task.ConnectRequest{
		ID: s.ID(),
	})
	if err != nil {
		return 0, errgrpc.ToNative(err)
	}

	return response.TaskPid, nil
}

func (s *shimTask) delete(ctx context.Context, removeTask func(ctx context.Context, id string)) (*runtime.Exit, error) {
	response, shimErr := s.task.Delete(ctx, &task.DeleteRequest{
		ID: s.ID(),
	})
	if shimErr != nil {
		log.G(ctx).WithField("id", s.ID()).WithError(shimErr).Error("failed to delete task")
		if !errors.Is(shimErr, ttrpc.ErrClosed) {
			shimErr = errgrpc.ToNative(shimErr)
			if !errdefs.IsNotFound(shimErr) {
				return nil, shimErr
			}
		}
	}

	deleteState, _ := s.ShimInstance.(taskDeleteState)

	// NOTE: If the shim has been killed and ttrpc connection has been
	// closed, the shimErr will not be nil. For this case, the event
	// subscriber, like moby/moby, might have received the exit or delete
	// events. Just in case, we should allow ttrpc-callback-on-close to
	// send the exit and delete events again. And the exit status will
	// depend on result of shimV2.Delete.
	//
	// If not, the shim has delivered the exit and delete events. Cache the
	// delete result so a retry can return it and prevent duplicate events from
	// ttrpc-callback-on-close.
	//
	// TODO: It's hard to guarantee that the event is unique and sent only
	// once. The moby/moby should not rely on that assumption that there is
	// only one exit event. The moby/moby should handle the duplicate events.
	//
	// REF: https://github.com/containerd/containerd/issues/4769
	var exit *runtime.Exit
	if shimErr == nil {
		exit = &runtime.Exit{
			Status:    response.ExitStatus,
			Timestamp: protobuf.FromTimestamp(response.ExitedAt),
			Pid:       response.Pid,
		}
		if deleteState != nil {
			deleteState.RecordDeleteResult(exit)
		}
	}

	// NOTE: Returning here deliberately leaves the shim record, the ttrpc
	// client and the bundle in place, so that the caller can retry Delete.
	// The result cached above lets that retry return the original exit, even
	// though the shim reports NotFound for the task by then.
	if err := s.waitShutdown(ctx); err != nil {
		return nil, fmt.Errorf("failed to invoke shutdown: %w", err)
	}

	if err := s.ShimInstance.Delete(ctx); err != nil {
		log.G(ctx).WithField("id", s.ID()).WithError(err).Error("failed to delete shim")
	}

	// remove self from the runtime task list
	// this seems dirty but it cleans up the API across runtimes, tasks, and the service
	removeTask(ctx, s.ID())

	if exit == nil && deleteState != nil {
		// An earlier attempt already deleted the task in the shim, so shimErr
		// is NotFound. Return the exit recorded by that attempt instead.
		exit = deleteState.DeleteResult()
	}
	if exit == nil {
		return nil, shimErr
	}
	return exit, nil
}

func (s *shimTask) Create(ctx context.Context, opts runtime.CreateOpts) (runtime.Task, error) {
	topts := opts.TaskOptions
	if topts == nil || topts.GetValue() == nil {
		topts = opts.RuntimeOptions
	}
	request := &task.CreateTaskRequest{
		ID:         s.ID(),
		Bundle:     s.Bundle(),
		Stdin:      opts.IO.Stdin,
		Stdout:     opts.IO.Stdout,
		Stderr:     opts.IO.Stderr,
		Terminal:   opts.IO.Terminal,
		Checkpoint: opts.Checkpoint,
		Options:    typeurl.MarshalProto(topts),
	}
	for _, m := range opts.Rootfs {
		request.Rootfs = append(request.Rootfs, &types.Mount{
			Type:    m.Type,
			Source:  m.Source,
			Target:  m.Target,
			Options: m.Options,
		})
	}

	_, err := s.task.Create(ctx, request)
	if err != nil {
		return nil, errgrpc.ToNative(err)
	}

	if opts.RestoreFromPath {
		// Unpack rootfs-diff.tar if it exists.
		// This needs to happen between the 'Create()' from above and before the 'Start()' from below.
		rootfsDiff := filepath.Join(opts.Checkpoint, "..", rootFsDiffTar)

		_, err = os.Stat(rootfsDiff)
		if err == nil {
			rootfsDiffTar, err := os.Open(rootfsDiff)
			if err != nil {
				return nil, fmt.Errorf("failed to open rootfs-diff archive %s for import: %w", rootfsDiffTar.Name(), err)
			}
			defer func(f *os.File) {
				if err := f.Close(); err != nil {
					log.G(ctx).Errorf("Unable to close file %s: %q", f.Name(), err)
				}
			}(rootfsDiffTar)

			decompressed, err := compression.DecompressStream(rootfsDiffTar)
			if err != nil {
				return nil, fmt.Errorf("failed to decompress archive %s for import: %w", rootfsDiffTar.Name(), err)
			}

			rootfs := filepath.Join(s.Bundle(), "rootfs")
			_, err = archive.Apply(
				ctx,
				rootfs,
				decompressed,
			)

			if err != nil {
				return nil, fmt.Errorf("unpacking of rootfs-diff archive %s into %s failed: %w", rootfsDiffTar.Name(), rootfs, err)
			}
			log.G(ctx).Debugf("Unpacked checkpoint in %s", rootfs)
		}
		// (adrianreber): This is unclear to me. But it works (and it is necessary).
		// This is probably connected to my misunderstanding why
		// restoring a container goes through Create().
		log.G(ctx).Infof("About to start with opts.Checkpoint %s", opts.Checkpoint)
		err = s.Start(ctx)
		if err != nil {
			return nil, err
		}
	}

	return s, nil
}

func (s *shimTask) Pause(ctx context.Context) error {
	if _, err := s.task.Pause(ctx, &task.PauseRequest{
		ID: s.ID(),
	}); err != nil {
		return errgrpc.ToNative(err)
	}
	return nil
}

func (s *shimTask) Resume(ctx context.Context) error {
	if _, err := s.task.Resume(ctx, &task.ResumeRequest{
		ID: s.ID(),
	}); err != nil {
		return errgrpc.ToNative(err)
	}
	return nil
}

func (s *shimTask) Start(ctx context.Context) error {
	_, err := s.task.Start(ctx, &task.StartRequest{
		ID: s.ID(),
	})
	if err != nil {
		return errgrpc.ToNative(err)
	}
	return nil
}

func (s *shimTask) Kill(ctx context.Context, signal uint32, all bool) error {
	if _, err := s.task.Kill(ctx, &task.KillRequest{
		ID:     s.ID(),
		Signal: signal,
		All:    all,
	}); err != nil {
		return errgrpc.ToNative(err)
	}
	return nil
}

func (s *shimTask) Exec(ctx context.Context, id string, opts runtime.ExecOpts) (runtime.ExecProcess, error) {
	if err := identifiers.Validate(id); err != nil {
		return nil, fmt.Errorf("invalid exec id %s: %w", id, err)
	}
	request := &task.ExecProcessRequest{
		ID:       s.ID(),
		ExecID:   id,
		Stdin:    opts.IO.Stdin,
		Stdout:   opts.IO.Stdout,
		Stderr:   opts.IO.Stderr,
		Terminal: opts.IO.Terminal,
		Spec:     opts.Spec,
	}
	if _, err := s.task.Exec(ctx, request); err != nil {
		return nil, errgrpc.ToNative(err)
	}
	return &process{
		id:   id,
		shim: s,
	}, nil
}

func (s *shimTask) Pids(ctx context.Context) ([]runtime.ProcessInfo, error) {
	resp, err := s.task.Pids(ctx, &task.PidsRequest{
		ID: s.ID(),
	})
	if err != nil {
		return nil, errgrpc.ToNative(err)
	}
	var processList []runtime.ProcessInfo
	for _, p := range resp.Processes {
		processList = append(processList, runtime.ProcessInfo{
			Pid:  p.Pid,
			Info: p.Info,
		})
	}
	return processList, nil
}

func (s *shimTask) ResizePty(ctx context.Context, size runtime.ConsoleSize) error {
	_, err := s.task.ResizePty(ctx, &task.ResizePtyRequest{
		ID:     s.ID(),
		Width:  size.Width,
		Height: size.Height,
	})
	if err != nil {
		return errgrpc.ToNative(err)
	}
	return nil
}

func (s *shimTask) CloseIO(ctx context.Context) error {
	_, err := s.task.CloseIO(ctx, &task.CloseIORequest{
		ID:    s.ID(),
		Stdin: true,
	})
	if err != nil {
		return errgrpc.ToNative(err)
	}
	return nil
}

func (s *shimTask) Wait(ctx context.Context) (*runtime.Exit, error) {
	taskPid, err := s.PID(ctx)
	if err != nil {
		return nil, err
	}
	response, err := s.task.Wait(ctx, &task.WaitRequest{
		ID: s.ID(),
	})
	if err != nil {
		return nil, errgrpc.ToNative(err)
	}
	return &runtime.Exit{
		Pid:       taskPid,
		Timestamp: protobuf.FromTimestamp(response.ExitedAt),
		Status:    response.ExitStatus,
	}, nil
}

func (s *shimTask) Checkpoint(ctx context.Context, path string, options *ptypes.Any) error {
	request := &task.CheckpointTaskRequest{
		ID:      s.ID(),
		Path:    path,
		Options: options,
	}
	if _, err := s.task.Checkpoint(ctx, request); err != nil {
		return errgrpc.ToNative(err)
	}
	return nil
}

func (s *shimTask) Update(ctx context.Context, resources *ptypes.Any, annotations map[string]string) error {
	if _, err := s.task.Update(ctx, &task.UpdateTaskRequest{
		ID:          s.ID(),
		Resources:   resources,
		Annotations: annotations,
	}); err != nil {
		return errgrpc.ToNative(err)
	}
	return nil
}

func (s *shimTask) Stats(ctx context.Context) (*ptypes.Any, error) {
	response, err := s.task.Stats(ctx, &task.StatsRequest{
		ID: s.ID(),
	})
	if err != nil {
		return nil, errgrpc.ToNative(err)
	}
	return response.Stats, nil
}

func (s *shimTask) Process(ctx context.Context, id string) (runtime.ExecProcess, error) {
	p := &process{
		id:   id,
		shim: s,
	}
	if _, err := p.State(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

func (s *shimTask) State(ctx context.Context) (runtime.State, error) {
	response, err := s.task.State(ctx, &task.StateRequest{
		ID: s.ID(),
	})
	if err != nil {
		if errdefs.IsDeadlineExceeded(err) {
			return runtime.State{}, err
		}
		if !errors.Is(err, ttrpc.ErrClosed) {
			return runtime.State{}, errgrpc.ToNative(err)
		}
		return runtime.State{}, errdefs.ErrNotFound
	}
	return runtime.State{
		Pid:        response.Pid,
		Status:     statusFromProto(response.Status),
		Stdin:      response.Stdin,
		Stdout:     response.Stdout,
		Stderr:     response.Stderr,
		Terminal:   response.Terminal,
		ExitStatus: response.ExitStatus,
		ExitedAt:   protobuf.FromTimestamp(response.ExitedAt),
	}, nil
}
