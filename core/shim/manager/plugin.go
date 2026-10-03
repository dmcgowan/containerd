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

package manager

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	bootapi "github.com/containerd/containerd/api/runtime/bootstrap/v1"
	"github.com/containerd/log"

	"github.com/containerd/containerd/v2/pkg/dialer"
	"github.com/containerd/containerd/v2/pkg/namespaces"
)

// PluginNamespace is the reserved namespace under which managed proxy plugin
// shims are bundled. It is kept separate from user namespaces so that plugin
// bundles never collide with task or sandbox bundles.
const PluginNamespace = "plugins"

// PluginShim is a managed proxy plugin shim: a shim the manager starts (or
// adopts) to back a proxy plugin. It is long-lived and survives containerd
// restarts, reconnecting to the running shim rather than starting a new one.
//
// A PluginShim is resilient to the shim process dying: Dial restarts the shim
// once per failed connection attempt, so a gRPC client built with Dial as its
// context dialer transparently reconnects. The plugin instance and its
// grpc.ClientConn never change across a restart, so restarts are invisible to
// callers.
type PluginShim struct {
	mgr     *ShimManager
	name    string
	runtime string
	env     []string

	mu        sync.Mutex
	instance  ShimInstance
	bootstrap *bootapi.BootstrapResult
	// lastRestart guards against restart storms: a dial failure only triggers a
	// restart if the previous restart is old enough.
	lastRestart time.Time
}

// pluginContext returns a context scoped to the reserved plugin namespace.
func pluginContext(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, PluginNamespace)
}

// StartPlugin starts, or adopts an already-running, managed proxy plugin shim
// named name backed by the given runtime (a runtime name or absolute path). env
// is passed to the shim process. The returned PluginShim's Dial should be used
// as the gRPC context dialer for the proxy client.
//
// The shim must speak gRPC; a shim that bootstraps with any other protocol is
// rejected, since proxy plugin clients are gRPC.
func (m *ShimManager) StartPlugin(ctx context.Context, name, runtimeName string, env []string) (*PluginShim, error) {
	if m.state == "" || m.root == "" {
		return nil, errors.New("shim manager has no state/root directory configured for managed plugins")
	}
	ctx = pluginContext(ctx)

	ps := &PluginShim{
		mgr:     m,
		name:    name,
		runtime: runtimeName,
		env:     env,
	}

	inst, boot, err := ps.startOrAdopt(ctx)
	if err != nil {
		return nil, err
	}
	ps.instance = inst
	ps.bootstrap = boot
	return ps, nil
}

// startOrAdopt connects to an existing plugin shim if one is still running,
// otherwise cleans up any stale bundle and starts a fresh shim. It must be
// called with ps.mu held, or during construction before the PluginShim is
// shared.
func (ps *PluginShim) startOrAdopt(ctx context.Context) (ShimInstance, *bootapi.BootstrapResult, error) {
	m := ps.mgr
	name := ps.name

	onClose := func() {
		log.G(ctx).WithField("plugin", name).Info("managed plugin shim disconnected")
	}

	// Try to adopt a shim left running by a previous containerd.
	bundlePath := filepath.Join(m.state, PluginNamespace, name)
	if _, err := os.Stat(filepath.Join(bundlePath, "bootstrap.json")); err == nil {
		bundle, lerr := LoadBundle(ctx, m.state, name)
		if lerr == nil {
			if inst, aerr := loadShim(ctx, bundle, onClose); aerr == nil {
				boot := instanceBootstrap(inst)
				if perr := requireGRPC(boot); perr != nil {
					inst.Close()
					return nil, nil, perr
				}
				if aerr := m.shims.Add(ctx, inst); aerr != nil {
					inst.Close()
					return nil, nil, aerr
				}
				log.G(ctx).WithField("plugin", name).Info("adopted running managed plugin shim")
				return inst, boot, nil
			}
			// Could not connect to the recorded address; fall through to a fresh
			// start, cleaning up the stale bundle first.
			log.G(ctx).WithField("plugin", name).Info("managed plugin shim not reachable, restarting")
		}
	}

	// Best-effort cleanup of any stale bundle before starting fresh.
	if bundle, lerr := LoadBundle(ctx, m.state, name); lerr == nil {
		if _, derr := m.DeleteBundle(ctx, bundle, ps.runtime); derr != nil {
			log.G(ctx).WithField("plugin", name).WithError(derr).Debug("stale managed plugin bundle cleanup")
		}
	}

	bundle, err := NewBundle(ctx, m.root, m.state, name, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create bundle for managed plugin %q: %w", name, err)
	}

	inst, err := m.Start(ctx, name, bundle, StartConfig{
		Runtime: ps.runtime,
		Env:     ps.env,
		OnClose: onClose,
	})
	if err != nil {
		bundle.Delete()
		return nil, nil, fmt.Errorf("failed to start managed plugin shim %q: %w", name, err)
	}

	boot := instanceBootstrap(inst)
	if perr := requireGRPC(boot); perr != nil {
		m.Delete(ctx, name)
		return nil, nil, perr
	}
	return inst, boot, nil
}

// Result returns the bootstrap result the plugin shim reported at startup,
// carrying its address, protocol and any PluginInfo extension.
func (ps *PluginShim) Result() *bootapi.BootstrapResult {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.bootstrap
}

// Name returns the plugin name.
func (ps *PluginShim) Name() string {
	return ps.name
}

// Dial connects to the plugin shim's current gRPC address. It is intended to be
// used as a grpc.WithContextDialer so that a single grpc.ClientConn transparently
// follows the shim across restarts. The address argument from gRPC is ignored:
// the real address comes from the shim's bootstrap result, which Dial refreshes
// when a connection cannot be established.
func (ps *PluginShim) Dial(ctx context.Context, _ string) (net.Conn, error) {
	addr := ps.address()
	conn, err := dialer.ContextDialer(ctx, addr)
	if err == nil {
		return conn, nil
	}

	// The shim may have died. Restart it once (serialized) and retry.
	if rerr := ps.restart(ctx); rerr != nil {
		return nil, fmt.Errorf("plugin %q dial failed and restart failed: %w", ps.name, errors.Join(err, rerr))
	}
	return dialer.ContextDialer(ctx, ps.address())
}

func (ps *PluginShim) address() string {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.bootstrap == nil {
		return ""
	}
	return ps.bootstrap.Address
}

// restart replaces a dead plugin shim with a fresh one. It is serialized and
// rate-limited so concurrent dials do not start a storm of shims.
func (ps *PluginShim) restart(ctx context.Context) error {
	ctx = pluginContext(ctx)
	ps.mu.Lock()
	defer ps.mu.Unlock()

	// If another dial restarted recently, assume the current address is good.
	if time.Since(ps.lastRestart) < time.Second {
		return nil
	}

	m := ps.mgr
	// Drop the dead instance from the manager map and tear it down.
	m.Remove(ctx, ps.name)
	if ps.instance != nil {
		ps.instance.Close()
		ps.instance = nil
	}

	inst, boot, err := ps.startOrAdopt(ctx)
	if err != nil {
		return err
	}
	ps.instance = inst
	ps.bootstrap = boot
	ps.lastRestart = time.Now()
	return nil
}

// Close tears down the plugin shim's client connection and removes it from the
// manager map. It does not stop the shim process: managed plugin shims are
// adopted again on the next containerd start.
func (ps *PluginShim) Close() error {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.instance == nil {
		return nil
	}
	err := ps.instance.Close()
	ps.mgr.Remove(context.Background(), ps.name)
	ps.instance = nil
	return err
}

// CleanupPlugins deletes managed plugin bundles whose names are not in keep.
// It is used at startup to reap plugins removed from configuration. Shims that
// are still configured are left untouched.
func (m *ShimManager) CleanupPlugins(ctx context.Context, keep []string) error {
	if m.state == "" {
		return nil
	}
	ctx = pluginContext(ctx)
	dir := filepath.Join(m.state, PluginNamespace)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	keepSet := make(map[string]struct{}, len(keep))
	for _, k := range keep {
		keepSet[k] = struct{}{}
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if _, ok := keepSet[name]; ok {
			continue
		}
		bundle, lerr := LoadBundle(ctx, m.state, name)
		if lerr != nil {
			continue
		}
		if _, derr := m.DeleteBundle(ctx, bundle, ""); derr != nil {
			log.G(ctx).WithField("plugin", name).WithError(derr).Warn("failed to clean up removed managed plugin")
			// Remove the bundle directory even if the shim delete failed, so a
			// removed plugin does not linger.
			bundle.Delete()
		}
	}
	return nil
}

func instanceBootstrap(inst ShimInstance) *bootapi.BootstrapResult {
	if c, ok := inst.(ShimCapabilities); ok {
		return c.BootstrapResult()
	}
	return nil
}

func requireGRPC(boot *bootapi.BootstrapResult) error {
	if boot == nil {
		return errors.New("managed plugin shim returned no bootstrap result")
	}
	if !strings.EqualFold(boot.Protocol, "grpc") {
		return fmt.Errorf("managed plugin shim must use grpc protocol, got %q", boot.Protocol)
	}
	return nil
}
