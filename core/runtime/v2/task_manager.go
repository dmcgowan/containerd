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
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"

	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/platforms"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/typeurl/v2"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-spec/specs-go/features"

	bootapi "github.com/containerd/containerd/api/runtime/bootstrap/v1"
	apitypes "github.com/containerd/containerd/api/types"

	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/events/exchange"
	"github.com/containerd/containerd/v2/core/metadata"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/runtime"
	"github.com/containerd/containerd/v2/core/sandbox"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/protobuf/proto"
	"github.com/containerd/containerd/v2/pkg/timeout"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/containerd/v2/plugins/services/warning"
)

// TaskConfig for the runtime task manager
type TaskConfig struct {
	// Supported platforms
	Platforms []string `toml:"platforms"`
}

func init() {
	registry.Register(&plugin.Registration{
		Type: plugins.RuntimePluginV2,
		ID:   "task",
		Requires: []plugin.Type{
			plugins.ShimPlugin,
			plugins.MountManagerPlugin,
			plugins.WarningPlugin,
			plugins.EventPlugin,
			plugins.MetadataPlugin,
		},
		Config: &TaskConfig{
			Platforms: defaultPlatforms(),
		},
		InitFn: func(ic *plugin.InitContext) (any, error) {
			config := ic.Config.(*TaskConfig)

			supportedPlatforms, err := platforms.ParseAll(config.Platforms)
			if err != nil {
				return nil, err
			}
			ic.Meta.Platforms = supportedPlatforms
			if ic.Meta.Exports == nil {
				ic.Meta.Exports = make(map[string]string, 1)
			}
			ic.Meta.Exports["log-uri-schemes"] = strings.Join(supportedLogURISchemes(), ",")

			shimManagerI, err := ic.GetSingle(plugins.ShimPlugin)
			if err != nil {
				return nil, err
			}
			shimManager := shimManagerI.(*ShimManager)

			md, err := ic.GetSingle(plugins.MetadataPlugin)
			if err != nil {
				return nil, err
			}
			ep, err := ic.GetByID(plugins.EventPlugin, "exchange")
			if err != nil {
				return nil, err
			}
			events := ep.(*exchange.Exchange)
			containerStore := metadata.NewContainerStore(md.(*metadata.DB))
			sandboxStore := metadata.NewSandboxStore(md.(*metadata.DB))

			var mounts mount.Manager
			if mountsI, err := ic.GetSingle(plugins.MountManagerPlugin); err == nil {
				mounts = mountsI.(mount.Manager)
			} else if !errors.Is(err, plugin.ErrPluginNotFound) {
				return nil, err
			}
			root, state := ic.Properties[plugins.PropertyRootDir], ic.Properties[plugins.PropertyStateDir]
			for _, d := range []string{root, state} {
				// root:  the parent of this directory is created as 0o700, not 0o711.
				// state: the parent of this directory is created as 0o711 too, so as to support userns-remapped containers.
				if err := os.MkdirAll(d, 0711); err != nil {
					return nil, err
				}
			}

			m := &TaskManager{
				root:         root,
				state:        state,
				manager:      shimManager,
				events:       events,
				containers:   containerStore,
				sandboxStore: sandboxStore,
				taskMounts: &taskMountController{
					manager: mounts,
					legacy:  newDeprecatedMountCapabilities(shimManager),
				},
			}

			if err := m.loadExistingTasks(ic.Context, state, root); err != nil {
				return nil, fmt.Errorf("failed to load existing shims for task manager: %w", err)
			}

			warningsI, err := ic.GetSingle(plugins.WarningPlugin)
			if err != nil {
				return nil, err
			}
			warnings := warningsI.(warning.Service)
			emitPlatformWarnings(ic.Context, warnings)

			return m, nil
		},
	})
}

// TaskManager wraps task service client on top of shim manager.
type TaskManager struct {
	root         string
	state        string
	manager      *ShimManager
	events       *exchange.Exchange
	containers   containers.Store
	sandboxStore sandbox.Store
	taskMounts   *taskMountController
}

// NewTaskManager creates a new task manager instance.
// root is the rootDir of TaskManager plugin to store persistent data
// state is the stateDir of TaskManager plugin to store transient data
// shims is  ShimManager for TaskManager to create/delete shims
func NewTaskManager(ctx context.Context, root, state string, shims *ShimManager, events *exchange.Exchange, containerStore containers.Store, sandboxStore sandbox.Store) (*TaskManager, error) {
	m := &TaskManager{
		root:         root,
		state:        state,
		manager:      shims,
		events:       events,
		containers:   containerStore,
		sandboxStore: sandboxStore,
		taskMounts: &taskMountController{
			legacy: newDeprecatedMountCapabilities(shims),
		},
	}
	if err := m.loadExistingTasks(ctx, state, root); err != nil {
		return nil, fmt.Errorf("failed to load existing shims for task manager: %w", err)
	}
	return m, nil
}

// loadExistingTasks reloads task and sandbox shims from disk, applying the task
// reap policy to each. It is the task-side wrapper around
// [ShimManager.LoadExistingShims].
func (m *TaskManager) loadExistingTasks(ctx context.Context, state, root string) error {
	return m.manager.LoadExistingShims(ctx, state, root, LoadConfig{
		// Runtime is read per-shim from the bundle's shim-binary-path file, or
		// for very old bundles from the container record.
		RuntimeResolver: m.resolveTaskRuntime,
		OnClose:         func(id string) { m.onTaskShimClose(ctx, id) },
		Reap:            m.reapTaskShim,
	})
}

// onTaskShimClose handles a task shim disconnect: it reaps the dead shim and
// publishes the task exit/delete events the shim could no longer deliver. The
// binary is reconstructed from the bundle on disk, since the shim has already
// been removed from the manager's map.
func (m *TaskManager) onTaskShimClose(ctx context.Context, id string) {
	ctx = context.WithoutCancel(ctx)
	bundle, err := LoadBundle(ctx, m.state, id)
	if err != nil {
		log.G(ctx).WithField("id", id).WithError(err).Error("failed to load bundle to clean up dead task shim")
		return
	}
	runtimeName, err := m.bundleRuntime(ctx, bundle)
	if err != nil {
		log.G(ctx).WithField("id", id).WithError(err).Error("failed to resolve runtime to clean up dead task shim")
		return
	}
	runtimePath, err := m.manager.resolveRuntimePath(runtimeName)
	if err != nil {
		log.G(ctx).WithField("id", id).WithError(err).Error("failed to resolve runtime path to clean up dead task shim")
		return
	}
	binaryCall := shimBinary(bundle, shimBinaryConfig{
		runtime:      runtimePath,
		address:      m.manager.containerdAddress,
		ttrpcAddress: m.manager.containerdTTRPCAddress,
		socketDir:    m.manager.socketDir,
		env:          m.manager.env,
	})
	cleanupAfterDeadShim(ctx, id, m.manager.shims, m.events, binaryCall)
}

// bundleRuntime resolves a shim's runtime name from its bundle, falling back to
// the container record for very old bundles.
func (m *TaskManager) bundleRuntime(ctx context.Context, bundle *Bundle) (string, error) {
	if data, err := os.ReadFile(filepath.Join(bundle.Path, "shim-binary-path")); err == nil {
		return string(data), nil
	} else if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	return m.resolveTaskRuntime(ctx, bundle.ID)
}

// resolveTaskRuntime reads a shim's runtime name from the container record, for
// very old bundles that predate the shim-binary-path file. On failure it also
// unmounts the bundle rootfs so a broken bundle does not leak a mount.
func (m *TaskManager) resolveTaskRuntime(ctx context.Context, id string) (string, error) {
	container, err := m.containers.Get(ctx, id)
	if err != nil {
		log.G(ctx).WithError(err).Errorf("loading container %s", id)
		bundlePath := filepath.Join(m.state, id)
		if ns, ok := namespaces.Namespace(ctx); ok {
			bundlePath = filepath.Join(m.state, ns, id)
		}
		if uerr := mount.UnmountRecursive(filepath.Join(bundlePath, "rootfs"), 0); uerr != nil {
			log.G(ctx).WithError(uerr).Errorf("failed to unmount of rootfs %s", id)
		}
		return "", err
	}
	return container.Runtime.Name, nil
}

// reapTaskShim is the task-manager reap policy applied when loading a shim from
// disk. It decides whether a loaded shim should be kept (it is still running a
// task or sandbox) or reaped (a leaked shim from a crash). It returns keep=true
// when the shim should remain registered.
//
// There are 3 possibilities for the loaded shim here:
//  1. It could be a shim that is running a task.
//  2. It could be a sandbox shim.
//  3. Or it could be a shim that was created for running a task but something
//     happened (probably a containerd crash) and the task was never created.
//     This shim process should be cleaned up here. See
//     containerd/containerd#6860 for further details.
func (m *TaskManager) reapTaskShim(ctx context.Context, shim ShimInstance) (bool, error) {
	id := shim.ID()

	// Check connectivity. TaskService is the only required service, so create a
	// temp one to check the connection, downgrading the client if the shim only
	// speaks an older task API.
	s, pidErr := probeTaskShim(ctx, shim)
	var pInfo []runtime.ProcessInfo
	if pidErr == nil {
		pInfo, pidErr = s.Pids(ctx)
	}

	_, sgetErr := m.sandboxStore.Get(ctx, id)
	if shouldCleanupShim(sgetErr, pidErr, pInfo) {
		logEntry := log.G(ctx).WithField("id", id)
		if pidErr != nil {
			logEntry = logEntry.WithError(pidErr)
		}
		logEntry.Info("cleaning leaked shim process")
		if s == nil {
			// Could not even create a task client; fall back to closing it and
			// letting the bundle be removed by the loader.
			shim.Close()
			return false, fmt.Errorf("failed to create task client for leaked shim %q", id)
		}
		if err := cleanupShimTask(ctx, s); err != nil && !errdefs.IsNotFound(err) {
			return false, fmt.Errorf("failed to clean up leaked shim %q: %w", id, err)
		}
		return false, nil
	}

	if pidErr != nil {
		log.G(ctx).WithField("id", id).WithError(pidErr).Warn("failed to query shim pids, keeping shim registered")
	}
	return true, nil
}

// probeTaskShim wraps shim in a shimTask and verifies connectivity by calling
// PID, downgrading the task client version if the shim only speaks an older API.
func probeTaskShim(ctx context.Context, shim ShimInstance) (*shimTask, error) {
	s, err := newShimTask(shim)
	if err != nil {
		return nil, err
	}

	if _, err := s.PID(ctx); err != nil {
		if !errdefs.IsNotImplemented(err) {
			return s, err
		}

		downgrader, ok := shim.(clientVersionDowngrader)
		if ok {
			if derr := downgrader.Downgrade(); derr == nil {
				log.G(ctx).WithError(err).WithField("id", shim.ID()).
					Warning("failed to call task.PID, downgrading client API version to try again")

				s, err = newShimTask(shim)
				if err != nil {
					return nil, fmt.Errorf("failed to create shim task after downgrading: %w", err)
				}
				_, err = s.PID(ctx)
			}
		}
		if err != nil {
			return s, err
		}
	}
	return s, nil
}

// shouldCleanupShim determines whether or not a shim is in such a state that
// we should reap it. To be reapable we confirm that it is not a sandbox shim
// and it has no pids running
func shouldCleanupShim(sgetErr, pidErr error, pInfo []runtime.ProcessInfo) bool {
	return errors.Is(sgetErr, errdefs.ErrNotFound) &&
		(errors.Is(pidErr, errdefs.ErrNotFound) ||
			(pidErr == nil && len(pInfo) == 0))
}

// ID of the task manager
func (m *TaskManager) ID() string {
	return plugins.RuntimePluginV2.String() + ".task"
}

// Create launches new shim instance and creates new task
func (m *TaskManager) Create(ctx context.Context, taskID string, opts runtime.CreateOpts) (_ runtime.Task, retErr error) {
	bundle, err := NewBundle(ctx, m.root, m.state, taskID, opts.Spec)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			bundle.Delete()
		}
	}()

	log.G(ctx).WithFields(log.Fields{
		"id":      taskID,
		"runtime": opts.Runtime,
	}).Debug("creating task")

	// Registered before the shim is started so that it runs after the shim
	// cleanup below: the shim may still be using these mounts.
	var activation mountActivation
	defer func() {
		if retErr != nil && activation.owned {
			dctx, cancel := timeout.WithContext(context.WithoutCancel(ctx), cleanupTimeout)
			defer cancel()
			if err := m.taskMounts.Deactivate(dctx, taskID); err != nil {
				log.G(ctx).WithError(err).WithField("task", taskID).Errorf("failed to deactivate mounts")
			}
		}
	}()

	topts := opts.TaskOptions
	if topts == nil || topts.GetValue() == nil {
		topts = opts.RuntimeOptions
	}

	startCfg := StartConfig{
		Runtime: opts.Runtime,
		Options: topts,
		OnClose: func() { m.onTaskShimClose(ctx, taskID) },
	}

	// Resolve whether this task should join an existing sandbox shim rather than
	// invoke the shim binary. This is task-specific knowledge, so it lives here
	// rather than in the generic shim manager.
	bootstrapParams, sandboxID, err := m.resolveSandboxJoin(ctx, taskID, opts)
	if err != nil {
		return nil, err
	}
	startCfg.Bootstrap = bootstrapParams
	startCfg.SandboxID = sandboxID

	// The shim is started before its mounts are activated so that it can report
	// which mount types and transforms it performs itself, which decides what
	// the mount manager must do on its behalf. Starting the shim does not
	// require the rootfs; only the task.Create call below consumes opts.Rootfs.
	shim, err := m.manager.Start(ctx, taskID, bundle, startCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to start shim: %w", err)
	}
	defer func() {
		if retErr != nil {
			m.cleanupStartedShim(ctx, taskID, shim)
		}
	}()

	var bootstrap *bootapi.BootstrapResult
	if sc, ok := shim.(shimCapabilities); ok {
		bootstrap = sc.BootstrapResult()
	}
	activation, err = m.taskMounts.Activate(ctx, taskID, opts.Runtime, bootstrap, opts.Rootfs)
	if err != nil {
		return nil, err
	}
	opts.Rootfs = activation.rootfs

	// Cast to shim task and call task service to create a new container task instance.
	// This will not be required once shim service / client implemented.
	shimTask, err := newShimTask(shim)
	if err != nil {
		return nil, err
	}

	// runc ignores silently features it doesn't know about, so for things that this is
	// problematic let's check if this runc version supports them.
	if err := m.validateRuntimeFeatures(ctx, opts); err != nil {
		return nil, fmt.Errorf("failed to validate OCI runtime features: %w", err)
	}

	t, err := func() (runtime.Task, error) {
		t, err := shimTask.Create(ctx, opts)
		if err == nil || !errdefs.IsNotImplemented(err) {
			return t, err
		}

		downgrader, ok := shim.(clientVersionDowngrader)
		if ok {
			if derr := downgrader.Downgrade(); derr == nil {
				log.G(ctx).WithError(err).WithField("id", taskID).
					Warning("failed to call task.Create, downgrading client API version to try again")

				shimTask, err = newShimTask(shim)
				if err != nil {
					return nil, fmt.Errorf("failed to create shim task after downgrading: %w", err)
				}
				return shimTask.Create(ctx, opts)
			}
		}
		return t, err
	}()
	if err != nil {
		// The shim is torn down, including removing it from m.manager.shims,
		// by the deferred cleanupStartedShim above.
		return nil, fmt.Errorf("failed to create shim task: %w", err)
	}

	return t, nil
}

// resolveSandboxJoin decides whether a task should join an already-running
// sandbox shim instead of invoking the shim binary. It returns non-nil
// bootstrap params (and the sandbox id) when the task should join; nil bootstrap
// means the shim binary should be invoked.
//
// Even though one shim can group multiple containers, that does not mean it
// supports the sandbox API. The old shim implementation still requires
// containerd to invoke `shim delete` to clean up each container's resource when
// it exits. So if the shim version is not higher than 3, we fall back to
// invoking the shim binary. The shim version also indicates streaming I/O
// support, rolled out together with the sandbox API.
func (m *TaskManager) resolveSandboxJoin(ctx context.Context, id string, opts runtime.CreateOpts) (*bootapi.BootstrapResult, string, error) {
	if opts.SandboxID == "" {
		return nil, "", nil
	}

	const supportSandboxAPIVersion = 3

	_, sbErr := m.sandboxStore.Get(ctx, opts.SandboxID)
	if sbErr != nil {
		if !errors.Is(sbErr, errdefs.ErrNotFound) {
			return nil, "", sbErr
		}
		// NOTE: If a sandbox container, like pause, is created by v1.6.x or
		// v1.7.x, the shim may not be able to group multiple containers. We
		// should invoke the shim binary and establish a new connection based on
		// the returned address.
		log.G(ctx).WithField("id", id).Warningf("sandbox (id=%s) not found, maybe created from v1.x", opts.SandboxID)
		return nil, opts.SandboxID, nil
	}

	var params *bootapi.BootstrapResult
	if opts.Address != "" {
		// The address returned from the sandbox controller should be in the form
		// like ttrpc+unix://<uds-path> or grpc+vsock://<cid>:<port>; split off
		// the protocol first.
		protocol, address, ok := strings.Cut(opts.Address, "+")
		if !ok {
			return nil, "", errors.New("the scheme of sandbox address should be in the form of <protocol>+<unix|vsock|tcp>, i.e. ttrpc+unix or grpc+vsock")
		}
		params = &bootapi.BootstrapResult{
			Version:  int32(opts.Version),
			Protocol: protocol,
			Address:  address,
		}

		// The sandbox controller only returns connection details, not what its
		// shim advertised at startup. Recover that from the shim instance
		// containerd already has in memory for this sandbox, so a container
		// joining it is not treated as if the shim advertised nothing.
		if process, err := m.manager.Get(ctx, opts.SandboxID); err == nil {
			params.Extensions = sandboxShimExtensions(process)
		}
	} else {
		process, err := m.manager.Get(ctx, opts.SandboxID)
		if err != nil {
			return nil, "", fmt.Errorf("can't find shim for sandbox %s: %w", opts.SandboxID, err)
		}

		p, err := restoreBootstrapParams(process.Bundle())
		if err != nil {
			return nil, "", fmt.Errorf("failed to get bootstrap params of sandbox %s: %w", opts.SandboxID, err)
		}
		params = p
	}

	if params.Version < supportSandboxAPIVersion {
		// Fall back to invoking the shim binary.
		return nil, opts.SandboxID, nil
	}

	return params, opts.SandboxID, nil
}

// sandboxShimExtensions returns the extensions process's shim advertised when
// it started, or nil if process does not retain that (for example, an external
// sandboxer's shim instance that predates capability extensions). This is best
// effort: a container joining a sandbox whose shim instance cannot be asked
// degrades to no extensions rather than failing to start.
func sandboxShimExtensions(process ShimInstance) []*bootapi.Extension {
	sc, ok := process.(shimCapabilities)
	if !ok {
		return nil
	}
	return sc.BootstrapResult().GetExtensions()
}

// cleanupStartedShim tears down a shim that was started for a task which then
// failed to be created. It may be called before a *shimTask exists for shim,
// since it also covers the window between a successful shim start and
// taskMounts.Activate/newShimTask succeeding.
func (m *TaskManager) cleanupStartedShim(ctx context.Context, taskID string, shim ShimInstance) {
	// NOTE: ctx contains required namespace information.
	m.manager.Remove(ctx, taskID)

	shimTask, err := newShimTask(shim)
	if err != nil {
		log.G(ctx).WithError(err).WithField("id", taskID).
			Error("failed to create shim task to clean up shim")
		shim.Close()
		return
	}

	if err := cleanupShimTask(ctx, shimTask); err != nil && !errdefs.IsNotFound(err) {
		log.G(ctx).WithError(err).WithField("id", taskID).Error("failed to clean up shim")
	}
}

// Get a specific task
func (m *TaskManager) Get(ctx context.Context, id string) (runtime.Task, error) {
	shim, err := m.manager.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return newShimTask(shim)
}

// Tasks lists all tasks
func (m *TaskManager) Tasks(ctx context.Context, all bool) ([]runtime.Task, error) {
	shims, err := m.manager.GetAll(ctx, all)
	if err != nil {
		return nil, err
	}
	out := make([]runtime.Task, len(shims))
	for i := range shims {
		newClient, err := newShimTask(shims[i])
		if err != nil {
			return nil, err
		}
		out[i] = newClient
	}
	return out, nil
}

// Delete deletes the task and shim instance
func (m *TaskManager) Delete(ctx context.Context, taskID string) (*runtime.Exit, error) {
	shim, err := m.manager.Get(ctx, taskID)
	if err != nil {
		return nil, err
	}

	_, err = m.containers.Get(ctx, taskID)
	if err != nil {
		return nil, err
	}

	shimTask, err := newShimTask(shim)
	if err != nil {
		return nil, err
	}

	exit, err := shimTask.delete(ctx, func(ctx context.Context, id string) {
		m.manager.Remove(ctx, id)
	})

	// An ErrNotFound here means the shim has no record of the task and there
	// was no cached delete result to fall back to. For example, the task was
	// never created successfully in the shim, or a previous containerd process
	// deleted it. The runtime side has still been cleaned up, so we should
	// deactivate the mounts before returning the error.
	if err != nil && !errdefs.IsNotFound(err) {
		return nil, fmt.Errorf("failed to delete task: %w", err)
	}

	// FIXME(fuweid): It seems that cleaning this up is best-effort because
	// GC can guarantee that the mount is deleted when the container is deleted.
	// What if we reuse the container and restart the task?
	if merr := m.taskMounts.Deactivate(ctx, taskID); merr != nil && !errdefs.IsNotFound(merr) {
		log.G(ctx).WithError(merr).WithField("task", taskID).Errorf("failed to deactivate mounts")
	}

	if err != nil {
		return nil, fmt.Errorf("failed to delete task: %w", err)
	}
	return exit, nil
}

func supportedLogURISchemes() []string {
	switch goruntime.GOOS {
	case "windows":
		return []string{"binary", "binary-v2", "file", "npipe"}
	default:
		return []string{"fifo", "binary", "binary-v2", "file"}
	}
}

func getRuntimeInfo(ctx context.Context, shims *ShimManager, req *apitypes.RuntimeRequest) (*apitypes.RuntimeInfo, error) {
	runtimePath, err := shims.ResolveRuntimePath(req.RuntimePath)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve runtime path: %w", err)
	}
	var optsB []byte
	if req.Options != nil {
		optsB, err = proto.Marshal(req.Options)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal %s: %w", req.Options.TypeUrl, err)
		}
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, runtimePath, "-info")
	cmd.Stdin = bytes.NewReader(optsB)
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to run %v: %w (stderr: %q)", cmd.Args, err, stderr.String())
	}
	var info apitypes.RuntimeInfo
	if err = proto.Unmarshal(stdout, &info); err != nil {
		return nil, fmt.Errorf("failed to unmarshal stdout from %v into %T: %w", cmd.Args, &info, err)
	}
	return &info, nil
}

func (m *TaskManager) PluginInfo(ctx context.Context, request any) (any, error) {
	req, ok := request.(*apitypes.RuntimeRequest)
	if !ok {
		return nil, fmt.Errorf("unknown request type %T: %w", request, errdefs.ErrNotImplemented)
	}

	return getRuntimeInfo(ctx, m.manager, req)
}

func (m *TaskManager) validateRuntimeFeatures(ctx context.Context, opts runtime.CreateOpts) error {
	var spec specs.Spec
	if err := typeurl.UnmarshalTo(opts.Spec, &spec); err != nil {
		return fmt.Errorf("unmarshal spec: %w", err)
	}

	// Only ask for the PluginInfo if idmap mounts are used.
	if !usesIDMapMounts(spec) {
		return nil
	}

	topts := opts.TaskOptions
	if topts == nil || topts.GetValue() == nil {
		topts = opts.RuntimeOptions
	}

	pInfo, err := m.PluginInfo(ctx, &apitypes.RuntimeRequest{RuntimePath: opts.Runtime, Options: typeurl.MarshalProto(topts)})
	if err != nil {
		return fmt.Errorf("runtime info: %w", err)
	}

	pluginInfo, ok := pInfo.(*apitypes.RuntimeInfo)
	if !ok {
		return fmt.Errorf("invalid runtime info type: %T", pInfo)
	}

	feat, err := typeurl.UnmarshalAny(pluginInfo.Features)
	if err != nil {
		return fmt.Errorf("unmarshal runtime features: %w", err)
	}

	// runc-compatible runtimes silently ignores features it doesn't know about. But ignoring
	// our request to use idmap mounts can break permissions in the volume, so let's make sure
	// it supports it. For more info, see:
	//	https://github.com/opencontainers/runtime-spec/pull/1219
	//
	features, ok := feat.(*features.Features)
	if !ok {
		// Leave alone non runc-compatible runtimes that don't provide the features info,
		// they might not be affected by this.
		return nil
	}

	if err := supportsIDMapMounts(features); err != nil {
		return fmt.Errorf("idmap mounts not supported: %w", err)
	}

	return nil
}

func usesIDMapMounts(spec specs.Spec) bool {
	for _, m := range spec.Mounts {
		if m.UIDMappings != nil || m.GIDMappings != nil {
			return true
		}
		if slices.Contains(m.Options, "idmap") || slices.Contains(m.Options, "ridmap") {
			return true
		}

	}
	return false
}

func supportsIDMapMounts(features *features.Features) error {
	if features.Linux.MountExtensions == nil || features.Linux.MountExtensions.IDMap == nil {
		return errors.New("missing `mountExtensions.idmap` entry in `features` command")
	}
	if enabled := features.Linux.MountExtensions.IDMap.Enabled; enabled == nil || !*enabled {
		return errors.New("entry `mountExtensions.idmap.Enabled` in `features` command not present or disabled")
	}
	return nil
}
