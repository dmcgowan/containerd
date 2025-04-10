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

package task

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	eventstypes "github.com/containerd/containerd/api/events"
	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/api/types/task"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"
	"github.com/containerd/ttrpc"
	"github.com/containerd/typeurl/v2"

	"github.com/containerd/containerd/v2/core/runtime"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/protobuf"
	ptypes "github.com/containerd/containerd/v2/pkg/protobuf/types"
	"github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/containerd/v2/pkg/shutdown"
	"github.com/containerd/containerd/v2/pkg/stdio"
)

var (
	_     = shim.TTRPCService(&service{})
	empty = &ptypes.Empty{}
)

// NewTaskService creates a new instance of a task service
func NewTaskService(ctx context.Context, publisher shim.Publisher, sd shutdown.Service) (taskAPI.TTRPCTaskService, error) {
	s := &service{
		context:  ctx,
		events:   make(chan interface{}, 128),
		shutdown: sd,
	}
	// TODO: initialize stdio
	go s.forward(ctx, publisher)
	sd.RegisterCallback(func(context.Context) error {
		close(s.events)
		return nil
	})

	if address, err := shim.ReadAddress("address"); err == nil {
		sd.RegisterCallback(func(context.Context) error {
			return shim.RemoveSocket(address)
		})
	}
	return s, nil
}

// service is the shim implementation of a remote shim over GRPC
type service struct {
	mu sync.Mutex

	context  context.Context
	events   chan interface{}
	platform stdio.Platform

	// TODO: keep map of containers

	lifecycleMu sync.Mutex

	shutdown shutdown.Service
}

// Create a new initial process and container
func (s *service) Create(ctx context.Context, r *taskAPI.CreateTaskRequest) (_ *taskAPI.CreateTaskResponse, err error) {
	log.G(ctx).WithFields(log.Fields{
		"container": r.ID,
		"bundle":    r.Bundle,
		"rootfs":    r.Rootfs,
		"stdin":     r.Stdin,
		"stdout":    r.Stdout,
		"stderr":    r.Stderr,
	}).Info("create task")

	b, err := os.ReadFile(filepath.Join(r.Bundle, "config.json"))
	if err != nil {
		return nil, errgrpc.ToGRPC(err)
	}
	fmt.Fprintln(os.Stderr, string(b))

	s.mu.Lock()
	defer s.mu.Unlock()

	// TODO: Create an init process and get pid
	pid := 0

	// TODO: Store config and bundle path

	s.send(&eventstypes.TaskCreate{
		ContainerID: r.ID,
		Bundle:      r.Bundle,
		Rootfs:      r.Rootfs,
		IO: &eventstypes.TaskIO{
			Stdin:    r.Stdin,
			Stdout:   r.Stdout,
			Stderr:   r.Stderr,
			Terminal: r.Terminal,
		},
		Checkpoint: r.Checkpoint,
		Pid:        uint32(pid),
	})

	return &taskAPI.CreateTaskResponse{
		Pid: uint32(pid),
	}, nil
}

func (s *service) RegisterTTRPC(server *ttrpc.Server) error {
	taskAPI.RegisterTTRPCTaskService(server, s)
	return nil
}

// Start a process
func (s *service) Start(ctx context.Context, r *taskAPI.StartRequest) (*taskAPI.StartResponse, error) {
	log.G(ctx).WithField("container", r.ID).Info("start task")

	// TODO: load config
	pid := 0

	switch r.ExecID {
	case "":
		s.send(&eventstypes.TaskStart{
			ContainerID: r.ID,
			Pid:         uint32(pid),
		})
	default:
		s.send(&eventstypes.TaskExecStarted{
			ContainerID: r.ID,
			ExecID:      r.ExecID,
			Pid:         uint32(pid),
		})
	}

	return &taskAPI.StartResponse{
		Pid: uint32(pid),
	}, nil
}

// Delete the initial process and container
func (s *service) Delete(ctx context.Context, r *taskAPI.DeleteRequest) (*taskAPI.DeleteResponse, error) {
	log.G(ctx).WithField("container", r.ID).Info("delete task")

	// TODO: load config
	pid := 0
	exitedAt := time.Now()

	// if we deleted an init task, send the task delete event
	if r.ExecID == "" {
		// TODO: delete container
		s.send(&eventstypes.TaskDelete{
			ContainerID: r.ID,
			Pid:         uint32(pid),
			ExitStatus:  uint32(0),
			ExitedAt:    protobuf.ToTimestamp(exitedAt),
		})
	}
	return &taskAPI.DeleteResponse{
		ExitStatus: uint32(0),
		ExitedAt:   protobuf.ToTimestamp(exitedAt),
		Pid:        uint32(pid),
	}, nil
}

// Exec an additional process inside the container
func (s *service) Exec(ctx context.Context, r *taskAPI.ExecProcessRequest) (*ptypes.Empty, error) {
	// TODO: Handle exec if supported

	s.send(&eventstypes.TaskExecAdded{
		ContainerID: r.ID,
		ExecID:      r.ExecID,
	})
	return empty, nil
}

// ResizePty of a process
func (s *service) ResizePty(ctx context.Context, r *taskAPI.ResizePtyRequest) (*ptypes.Empty, error) {
	// TODO: if supported

	return empty, nil
}

// State returns runtime state information for a process
func (s *service) State(ctx context.Context, r *taskAPI.StateRequest) (*taskAPI.StateResponse, error) {
	log.G(ctx).WithField("container", r.ID).Info("task state")

	// TODO: get process info
	pid := 0

	return &taskAPI.StateResponse{
		ID: r.ID,
		//Bundle:     container.Bundle,
		Pid:    uint32(pid),
		Status: task.Status_RUNNING,
		//Stdin:      path,
		//Stdout:     path,
		//Stderr:     path,
		//Terminal:   sio.Terminal,
		//ExitStatus: uint32(0),
		//ExitedAt:   protobuf.ToTimestamp(time.Now()),
	}, nil
}

// Pause the container
func (s *service) Pause(ctx context.Context, r *taskAPI.PauseRequest) (*ptypes.Empty, error) {
	// TODO: if supported
	s.send(&eventstypes.TaskPaused{
		ContainerID: r.ID,
	})
	return empty, nil
}

// Resume the container
func (s *service) Resume(ctx context.Context, r *taskAPI.ResumeRequest) (*ptypes.Empty, error) {
	// TODO: if supported
	s.send(&eventstypes.TaskResumed{
		ContainerID: r.ID,
	})
	return empty, nil
}

// Kill a process with the provided signal
func (s *service) Kill(ctx context.Context, r *taskAPI.KillRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithField("container", r.ID).WithField("signal", r.Signal).Info("kill task")
	// TODO: end the process

	return empty, nil
}

// Pids returns all pids inside the container
func (s *service) Pids(ctx context.Context, r *taskAPI.PidsRequest) (*taskAPI.PidsResponse, error) {
	var processes []*task.ProcessInfo

	// TODO: Get all process info

	return &taskAPI.PidsResponse{
		Processes: processes,
	}, nil
}

// CloseIO of a process
func (s *service) CloseIO(ctx context.Context, r *taskAPI.CloseIORequest) (*ptypes.Empty, error) {
	// TODO: if supported

	return empty, nil
}

// Checkpoint the container
func (s *service) Checkpoint(ctx context.Context, r *taskAPI.CheckpointTaskRequest) (*ptypes.Empty, error) {
	// TODO: if supported

	return empty, nil
}

// Update a running container
func (s *service) Update(ctx context.Context, r *taskAPI.UpdateTaskRequest) (*ptypes.Empty, error) {
	// TODO: if supported

	return empty, nil
}

// Wait for a process to exit
func (s *service) Wait(ctx context.Context, r *taskAPI.WaitRequest) (*taskAPI.WaitResponse, error) {
	log.G(ctx).WithField("container", r.ID).Info("wait task")
	// TODO: get process info and wait for exit

	return &taskAPI.WaitResponse{
		ExitStatus: uint32(0),
		ExitedAt:   protobuf.ToTimestamp(time.Now()),
	}, nil
}

// Connect returns shim information such as the shim's pid
func (s *service) Connect(ctx context.Context, r *taskAPI.ConnectRequest) (*taskAPI.ConnectResponse, error) {
	// TODO: get pid info
	pid := 0

	return &taskAPI.ConnectResponse{
		ShimPid: uint32(os.Getpid()),
		TaskPid: uint32(pid),
	}, nil
}

func (s *service) Shutdown(ctx context.Context, r *taskAPI.ShutdownRequest) (*ptypes.Empty, error) {
	log.G(ctx).WithField("container", r.ID).Info("shutdown task")

	s.mu.Lock()
	defer s.mu.Unlock()

	// TODO: return empty if the shim is still servicing containers

	// please make sure that temporary resource has been cleanup or registered
	// for cleanup before calling shutdown
	s.shutdown.Shutdown()

	return empty, nil
}

func (s *service) Stats(ctx context.Context, r *taskAPI.StatsRequest) (*taskAPI.StatsResponse, error) {
	log.G(ctx).WithField("container", r.ID).Info("task stats")

	// Implement any stats logic here
	data, err := typeurl.MarshalAny(struct{}{})
	if err != nil {
		return nil, err
	}
	return &taskAPI.StatsResponse{
		Stats: typeurl.MarshalProto(data),
	}, nil
}

func (s *service) send(evt interface{}) {
	s.events <- evt
}

func (s *service) forward(ctx context.Context, publisher shim.Publisher) {
	ns, _ := namespaces.Namespace(ctx)
	ctx = namespaces.WithNamespace(context.Background(), ns)
	for e := range s.events {
		err := publisher.Publish(ctx, runtime.GetTopic(e), e)
		if err != nil {
			log.G(ctx).WithError(err).Error("post event")
		}
	}
	publisher.Close()
}
