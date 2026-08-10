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

package sandbox

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/containerd/errdefs/pkg/errgrpc"
	"github.com/containerd/log"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/typeurl/v2"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"

	runtimeAPI "github.com/containerd/containerd/api/runtime/sandbox/v1"
	"github.com/containerd/containerd/api/types"

	"github.com/containerd/containerd/v2/core/events"
	"github.com/containerd/containerd/v2/core/events/exchange"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/runtime"
	v2 "github.com/containerd/containerd/v2/core/runtime/v2"
	"github.com/containerd/containerd/v2/core/sandbox"
	"github.com/containerd/containerd/v2/plugins"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: plugins.SandboxControllerPlugin,
		ID:   "shim",
		Requires: []plugin.Type{
			plugins.ShimPlugin,
			plugins.EventPlugin,
			plugins.MountManagerPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (any, error) {
			shimPlugin, err := ic.GetSingle(plugins.ShimPlugin)
			if err != nil {
				return nil, err
			}

			exchangePlugin, err := ic.GetByID(plugins.EventPlugin, "exchange")
			if err != nil {
				return nil, err
			}

			var (
				shims     = shimPlugin.(*v2.ShimManager)
				publisher = exchangePlugin.(*exchange.Exchange)
			)

			// The mount manager is optional: without it the sandbox
			// rootfs is passed through to the shim untouched.
			var mounts mount.Manager
			if mountsI, err := ic.GetSingle(plugins.MountManagerPlugin); err == nil {
				mounts = mountsI.(mount.Manager)
			} else {
				log.G(ic.Context).WithError(err).Info("mount manager unavailable, sandbox rootfs will be passed through unmodified")
			}
			state := ic.Properties[plugins.PropertyStateDir]
			root := ic.Properties[plugins.PropertyRootDir]
			for _, d := range []string{root, state} {
				if err := os.MkdirAll(d, 0700); err != nil {
					return nil, err
				}
				// chmod is needed for upgrading from an older release that created the dir with 0o711
				if err := os.Chmod(d, 0o700); err != nil {
					return nil, err
				}
			}

			if err := shims.LoadExistingShims(ic.Context, state, root); err != nil {
				return nil, fmt.Errorf("failed to load existing shim sandboxes, %v", err)
			}

			c := &controllerLocal{
				root:      root,
				state:     state,
				shims:     shims,
				publisher: publisher,
				mounts:    mounts,
			}
			return c, nil
		},
	})
}

type controllerLocal struct {
	root      string
	state     string
	shims     *v2.ShimManager
	publisher events.Publisher
	mounts    mount.Manager
}

var _ sandbox.Controller = (*controllerLocal)(nil)

func (c *controllerLocal) cleanupShim(ctx context.Context, sandboxID string, svc runtimeAPI.TTRPCSandboxService) {
	// Let the shim exit, then we can clean up the bundle after.
	if _, sErr := svc.ShutdownSandbox(ctx, &runtimeAPI.ShutdownSandboxRequest{
		SandboxID: sandboxID,
	}); sErr != nil {
		log.G(ctx).WithError(sErr).WithField("sandboxID", sandboxID).
			Error("failed to shutdown sandbox")
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	dErr := c.shims.Delete(ctx, sandboxID)
	if dErr != nil {
		log.G(ctx).WithError(dErr).WithField("sandboxID", sandboxID).
			Error("failed to delete shim")
	}
}

func (c *controllerLocal) Create(ctx context.Context, info sandbox.Sandbox, opts ...sandbox.CreateOpt) (retErr error) {
	var coptions sandbox.CreateOptions
	sandboxID := info.ID
	for _, opt := range opts {
		opt(&coptions)
	}

	if _, err := c.shims.Get(ctx, sandboxID); err == nil {
		return fmt.Errorf("sandbox %s already running: %w", sandboxID, errdefs.ErrAlreadyExists)
	}

	bundle, err := v2.NewBundle(ctx, c.root, c.state, sandboxID, info.Spec)
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			bundle.Delete()
		}
	}()

	shim, err := c.shims.Start(ctx, sandboxID, bundle, runtime.CreateOpts{
		Spec:           info.Spec,
		RuntimeOptions: info.Runtime.Options,
		Runtime:        info.Runtime.Name,
		TaskOptions:    nil,
	})
	if err != nil {
		return fmt.Errorf("failed to start new shim for sandbox %s: %w", sandboxID, err)
	}

	svc, err := sandbox.NewClient(shim.Client())
	if err != nil {
		return err
	}

	rootfs, err := c.activateRootfs(ctx, info, coptions.Rootfs)
	if err != nil {
		c.cleanupShim(ctx, sandboxID, svc)
		return err
	}
	defer func() {
		if retErr != nil {
			c.deactivateRootfs(ctx, sandboxID)
		}
	}()

	if _, err := svc.CreateSandbox(ctx, &runtimeAPI.CreateSandboxRequest{
		SandboxID:   sandboxID,
		BundlePath:  shim.Bundle(),
		Rootfs:      mount.ToProto(rootfs),
		Options:     typeurl.MarshalProto(coptions.Options),
		NetnsPath:   coptions.NetNSPath,
		Annotations: coptions.Annotations,
	}); err != nil {
		c.cleanupShim(ctx, sandboxID, svc)
		return fmt.Errorf("failed to create sandbox %s: %w", sandboxID, errgrpc.ToNative(err))
	}

	return nil
}

// activateRootfs prepares the sandbox rootfs through the mount
// manager, returning the mounts to hand to the shim.
//
// This runs any mount transforms the snapshotter asked for, such as
// creating and formatting a block image, and mounts anything the shim
// has not declared it handles itself. A sandbox which attaches a block
// device to a VM therefore receives a plain, correctly sized device
// rather than having to create one itself.
//
// The mounts are returned unchanged if there is no mount manager or
// nothing in the chain needs it.
func (c *controllerLocal) activateRootfs(ctx context.Context, info sandbox.Sandbox, rootfs []mount.Mount) ([]mount.Mount, error) {
	if c.mounts == nil || len(rootfs) == 0 {
		return rootfs, nil
	}

	activateOpts := []mount.ActivateOpt{
		mount.WithLabels(sandboxMountLabels(info)),
	}
	if types, err := c.shims.AllowedMountTypes(ctx, info.Runtime.Name); err == nil {
		for _, t := range types {
			activateOpts = append(activateOpts, mount.WithAllowMountType(t))
		}
	} else {
		log.G(ctx).WithError(err).WithField("runtime", info.Runtime.Name).Error("failed to load runtime info")
	}

	ai, err := c.mounts.Activate(ctx, info.ID, rootfs, activateOpts...)
	if err == nil {
		return ai.System, nil
	}
	if errdefs.IsAlreadyExists(err) {
		// Reuse the existing activation rather than tearing it down,
		// the sandbox with this id still exists.
		ai, err = c.mounts.Info(ctx, info.ID)
		if err != nil {
			return nil, fmt.Errorf("failed to get info on already active sandbox mount: %w", err)
		}
		return ai.System, nil
	}
	if errdefs.IsNotImplemented(err) {
		// Nothing in the chain needs the mount manager.
		return rootfs, nil
	}
	return nil, fmt.Errorf("failed to activate sandbox rootfs: %w", err)
}

// deactivateRootfs releases the sandbox rootfs activation. Mounts
// shared with other activations, such as a block image backing every
// container in the sandbox, are only unmounted once nothing else
// references them.
func (c *controllerLocal) deactivateRootfs(ctx context.Context, sandboxID string) {
	if c.mounts == nil {
		return
	}
	if err := c.mounts.Deactivate(ctx, sandboxID); err != nil && !errdefs.IsNotFound(err) && !errdefs.IsNotImplemented(err) {
		log.G(ctx).WithError(err).WithField("sandboxID", sandboxID).Error("failed to deactivate sandbox mounts")
	}
}

// gcBackRefPrefix marks sandbox labels which describe what the
// sandbox rootfs is derived from.
const gcBackRefPrefix = "containerd.io/gc.bref."

// sandboxMountLabels forwards the sandbox's garbage collection back
// references to its mount activation, so that the activation is
// collected along with the resource it was built from if the sandbox
// is never cleanly shut down.
func sandboxMountLabels(info sandbox.Sandbox) map[string]string {
	labels := map[string]string{}
	for k, v := range info.Labels {
		if strings.HasPrefix(k, gcBackRefPrefix) {
			labels[k] = v
		}
	}
	return labels
}

func (c *controllerLocal) Start(ctx context.Context, sandboxID string) (sandbox.ControllerInstance, error) {
	shim, err := c.shims.Get(ctx, sandboxID)
	if err != nil {
		return sandbox.ControllerInstance{}, fmt.Errorf("unable to find sandbox %q", sandboxID)
	}

	svc, err := sandbox.NewClient(shim.Client())
	if err != nil {
		return sandbox.ControllerInstance{}, err
	}

	resp, err := svc.StartSandbox(ctx, &runtimeAPI.StartSandboxRequest{SandboxID: sandboxID})
	if err != nil {
		c.cleanupShim(ctx, sandboxID, svc)
		return sandbox.ControllerInstance{}, fmt.Errorf("failed to start sandbox %s: %w", sandboxID, errgrpc.ToNative(err))
	}
	address, version := shim.Endpoint()
	return sandbox.ControllerInstance{
		SandboxID: sandboxID,
		Pid:       resp.GetPid(),
		Address:   address,
		Version:   uint32(version),
		CreatedAt: resp.GetCreatedAt().AsTime(),
		Spec:      resp.GetSpec(),
	}, nil
}

func (c *controllerLocal) Platform(ctx context.Context, sandboxID string) (imagespec.Platform, error) {
	svc, err := c.getSandbox(ctx, sandboxID)
	if err != nil {
		return imagespec.Platform{}, err
	}

	response, err := svc.Platform(ctx, &runtimeAPI.PlatformRequest{SandboxID: sandboxID})
	if err != nil {
		return imagespec.Platform{}, fmt.Errorf("failed to get sandbox platform: %w", errgrpc.ToNative(err))
	}

	var platform imagespec.Platform
	if p := response.GetPlatform(); p != nil {
		platform.OS = p.OS
		platform.Architecture = p.Architecture
		platform.Variant = p.Variant
	}
	return platform, nil
}

func (c *controllerLocal) Stop(ctx context.Context, sandboxID string, opts ...sandbox.StopOpt) error {
	var soptions sandbox.StopOptions
	for _, opt := range opts {
		opt(&soptions)
	}
	req := &runtimeAPI.StopSandboxRequest{SandboxID: sandboxID}
	if soptions.Timeout != nil {
		req.TimeoutSecs = uint32(soptions.Timeout.Seconds())
	}

	svc, err := c.getSandbox(ctx, sandboxID)
	if errdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	if _, err := svc.StopSandbox(ctx, req); err != nil {
		err = errgrpc.ToNative(err)
		if !errdefs.IsNotFound(err) && !errdefs.IsUnavailable(err) {
			return fmt.Errorf("failed to stop sandbox: %w", err)
		}
	}

	return nil
}

func (c *controllerLocal) Shutdown(ctx context.Context, sandboxID string) error {
	svc, err := c.getSandbox(ctx, sandboxID)
	if err != nil {
		return err
	}

	_, err = svc.ShutdownSandbox(ctx, &runtimeAPI.ShutdownSandboxRequest{SandboxID: sandboxID})
	if err != nil {
		return fmt.Errorf("failed to shutdown sandbox: %w", errgrpc.ToNative(err))
	}

	if err := c.shims.Delete(ctx, sandboxID); err != nil {
		return fmt.Errorf("failed to delete sandbox shim: %w", err)
	}

	c.deactivateRootfs(ctx, sandboxID)

	return nil
}

func (c *controllerLocal) Wait(ctx context.Context, sandboxID string) (sandbox.ExitStatus, error) {
	svc, err := c.getSandbox(ctx, sandboxID)
	if err != nil {
		return sandbox.ExitStatus{}, err
	}

	resp, err := svc.WaitSandbox(ctx, &runtimeAPI.WaitSandboxRequest{
		SandboxID: sandboxID,
	})

	if err != nil {
		return sandbox.ExitStatus{}, fmt.Errorf("failed to wait sandbox %s: %w", sandboxID, errgrpc.ToNative(err))
	}

	return sandbox.ExitStatus{
		ExitStatus: resp.GetExitStatus(),
		ExitedAt:   resp.GetExitedAt().AsTime(),
	}, nil
}

func (c *controllerLocal) Status(ctx context.Context, sandboxID string, verbose bool) (sandbox.ControllerStatus, error) {
	svc, err := c.getSandbox(ctx, sandboxID)
	if errdefs.IsNotFound(err) {
		return sandbox.ControllerStatus{
			SandboxID: sandboxID,
			ExitedAt:  time.Now(),
		}, nil
	}
	if err != nil {
		return sandbox.ControllerStatus{}, err
	}

	resp, err := svc.SandboxStatus(ctx, &runtimeAPI.SandboxStatusRequest{
		SandboxID: sandboxID,
		Verbose:   verbose,
	})
	if err != nil {
		return sandbox.ControllerStatus{}, fmt.Errorf("failed to query sandbox %s status: %w", sandboxID, err)
	}

	shim, err := c.shims.Get(ctx, sandboxID)
	if err != nil {
		return sandbox.ControllerStatus{}, fmt.Errorf("unable to find sandbox %q", sandboxID)
	}
	address, version := shim.Endpoint()

	return sandbox.ControllerStatus{
		SandboxID: resp.GetSandboxID(),
		Pid:       resp.GetPid(),
		State:     resp.GetState(),
		Info:      resp.GetInfo(),
		CreatedAt: resp.GetCreatedAt().AsTime(),
		ExitedAt:  resp.GetExitedAt().AsTime(),
		Extra:     resp.GetExtra(),
		Address:   address,
		Version:   uint32(version),
	}, nil
}

func (c *controllerLocal) Metrics(ctx context.Context, sandboxID string) (*types.Metric, error) {
	sb, err := c.getSandbox(ctx, sandboxID)
	if err != nil {
		return nil, err
	}
	req := &runtimeAPI.SandboxMetricsRequest{SandboxID: sandboxID}
	resp, err := sb.SandboxMetrics(ctx, req)
	if err != nil {
		return nil, err
	}
	return resp.Metrics, nil
}

func (c *controllerLocal) Update(
	ctx context.Context,
	sandboxID string,
	sandbox sandbox.Sandbox,
	fields ...string) error {
	return nil
}

func (c *controllerLocal) getSandbox(ctx context.Context, id string) (runtimeAPI.TTRPCSandboxService, error) {
	shim, err := c.shims.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	return sandbox.NewClient(shim.Client())
}
