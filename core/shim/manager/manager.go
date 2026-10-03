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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/containerd/log"
	"github.com/containerd/typeurl/v2"

	bootapi "github.com/containerd/containerd/api/runtime/bootstrap/v1"
	"github.com/containerd/containerd/v2/core/runtime"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	shimbinary "github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/containerd/v2/pkg/timeout"
)

// ShimConfig for the shim
type ShimConfig struct {
	// Env is environment variables added to shim processes
	Env []string `toml:"env"`

	// SocketDir is the directory to place shim sockets. The path must be
	// short enough to fit within the platform's unix socket path limit.
	// Defaults:
	//  Linux (UID 0):  /run/containerd/s
	//  Linux (UID >0): /run/$UID/containerd/s or /tmp/containerd-s-$(UID)
	SocketDir string `toml:"socket_dir"`
}

// MaxSocketDirLen is the maximum length of the socket directory path for the
// current platform.
const MaxSocketDirLen = maxSocketDirLen

// DefaultSocketDir returns the default directory used for shim unix sockets, or
// an empty string if none could be determined.
func DefaultSocketDir() string {
	return defaultSocketDir()
}

type ManagerConfig struct {
	Address      string
	TTRPCAddress string
	SocketDir    string
	ShimEnv      []string
}

// NewShimManager creates a manager for v2 shims
func NewShimManager(config *ManagerConfig) (*ShimManager, error) {
	m := &ShimManager{
		containerdAddress:      config.Address,
		containerdTTRPCAddress: config.TTRPCAddress,
		socketDir:              config.SocketDir,
		shims:                  runtime.NewNSMap[ShimInstance](),
		env:                    config.ShimEnv,
	}

	return m, nil
}

// ShimManager manages currently running shim processes.
// It is mainly responsible for launching new shims and for proper shutdown and cleanup of existing instances.
// The manager is unaware of the underlying services shim provides and lets higher level services consume them,
// but don't care about lifecycle management.
//
// The manager is intentionally generic: it does not know whether a shim runs a
// task, a sandbox, or a long-running plugin service. Callers supply all
// knowledge of that through [StartConfig] and [LoadConfig], including how to
// react when a shim disconnects (OnClose) and, for loads, whether a given shim
// should be kept or reaped (Reap).
type ShimManager struct {
	containerdAddress      string
	containerdTTRPCAddress string
	env                    []string
	shims                  *runtime.NSMap[ShimInstance]
	socketDir              string
	// runtimePaths is a cache of `runtime names` -> `resolved fs path`
	runtimePaths sync.Map
}

// StartConfig configures a shim start. It carries everything the manager needs
// to launch (or join) a shim without knowing what service the shim provides.
type StartConfig struct {
	// Runtime is the runtime name or absolute path to the shim binary.
	Runtime string
	// Options is the runtime/task options passed to the shim binary as a
	// bootstrap extension. May be nil.
	Options typeurl.Any
	// OnClose is invoked when the shim's connection is severed. It must not be
	// nil. The manager removes the shim from its own map before invoking it, so
	// OnClose is free to perform caller-specific cleanup (publishing task
	// events, restarting a plugin, and so on).
	OnClose func()
	// Bootstrap, when set, makes the manager join an already-running shim at the
	// given address instead of invoking the shim binary. It is used to attach a
	// container to an existing sandbox shim. The caller is responsible for
	// resolving these connection details.
	Bootstrap *bootapi.BootstrapResult
	// SandboxID is written into the bundle so the shim knows which sandbox a
	// joining container belongs to. Only consulted when Bootstrap is set.
	SandboxID string
}

// LoadConfig configures reloading a shim from an existing bundle.
type LoadConfig struct {
	// Runtime is the runtime name or absolute path to the shim binary. When
	// empty, the manager reads it from the bundle's shim-binary-path file, then
	// falls back to RuntimeResolver.
	Runtime string
	// RuntimeResolver resolves the runtime name for a shim id when neither
	// Runtime nor the bundle's shim-binary-path file supplies one. It supports
	// very old bundles that predate the shim-binary-path file. May be nil.
	RuntimeResolver func(ctx context.Context, id string) (string, error)
	// OnClose is invoked when the shim's connection is severed. It receives the
	// shim's id and must not be nil. The manager removes the shim from its map
	// before invoking it.
	OnClose func(id string)
	// Reap is an optional policy hook that decides whether a freshly loaded shim
	// should be kept (registered in the manager) or reaped. It is given the
	// loaded instance and must return true to keep it. When nil, the shim is
	// always kept. A Reap that returns false has already cleaned up the shim.
	Reap func(ctx context.Context, shim ShimInstance) (keep bool, err error)
}

// ID of the shim manager. This matches the io.containerd.shim.v1.manager plugin
// ID under which the manager is registered (see plugins/shim).
func (m *ShimManager) ID() string {
	return "io.containerd.shim.v1.manager"
}

// Env returns the environment configured for the shim manager.
func (m *ShimManager) Env() []string {
	if m.env == nil {
		return nil
	}
	cp := make([]string, len(m.env))
	copy(cp, m.env)
	return cp
}

// Start launches a new shim instance, or joins an already-running one when
// cfg.Bootstrap is set. It is generic: the caller decides, through cfg, what
// the shim is for and how to react when it disconnects.
func (m *ShimManager) Start(ctx context.Context, id string, bundle *Bundle, cfg StartConfig) (_ ShimInstance, retErr error) {
	if cfg.OnClose == nil {
		return nil, errors.New("StartConfig.OnClose must not be nil")
	}

	onClose := func() {
		log.G(ctx).WithField("id", id).Info("shim disconnected")
		// Remove self from the shim list first: the caller's OnClose may
		// publish events or otherwise assume the shim is already gone.
		m.shims.Delete(ctx, id)
		cfg.OnClose()
	}

	// Join an already-running shim rather than invoking the shim binary.
	if cfg.Bootstrap != nil {
		// Write sandbox ID this task belongs to.
		if err := os.WriteFile(filepath.Join(bundle.Path, "sandbox"), []byte(cfg.SandboxID), 0600); err != nil {
			return nil, err
		}

		if err := writeBootstrapParams(filepath.Join(bundle.Path, "bootstrap.json"), cfg.Bootstrap); err != nil {
			return nil, fmt.Errorf("failed to write bootstrap.json for bundle %s: %w", bundle.Path, err)
		}

		shim, err := loadShim(ctx, bundle, onClose)
		if err != nil {
			return nil, fmt.Errorf("failed to join shim for sandbox %q: %w", cfg.SandboxID, err)
		}

		if err := m.shims.Add(ctx, shim); err != nil {
			return nil, err
		}

		return shim, nil
	}

	shim, err := m.startShim(ctx, bundle, id, cfg, onClose)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			m.cleanupShim(ctx, shim)
		}
	}()

	if err := m.shims.Add(ctx, shim); err != nil {
		return nil, fmt.Errorf("failed to add shim: %w", err)
	}

	return shim, nil
}

func (m *ShimManager) startShim(ctx context.Context, bundle *Bundle, id string, cfg StartConfig, onClose func()) (*shim, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}
	ctx = log.WithLogger(ctx, log.G(ctx).WithField("namespace", ns))

	runtimePath, err := m.resolveRuntimePath(cfg.Runtime)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve runtime path: %w", err)
	}

	b := shimBinary(bundle, shimBinaryConfig{
		runtime:      runtimePath,
		address:      m.containerdAddress,
		ttrpcAddress: m.containerdTTRPCAddress,
		socketDir:    m.socketDir,
		env:          m.env,
	})
	shim, err := b.Start(ctx, typeurl.MarshalProto(cfg.Options), onClose)
	if err != nil {
		return nil, fmt.Errorf("start failed: %w", err)
	}

	return shim, nil
}

// restoreBootstrapParams reads bootstrap.json to restore shim configuration.
// If its an old shim, this will perform migration - read address file and write default bootstrap
// configuration (version = 2, protocol = ttrpc, and address).
func restoreBootstrapParams(bundlePath string) (*bootapi.BootstrapResult, error) {
	filePath := filepath.Join(bundlePath, "bootstrap.json")

	// Read bootstrap.json if exists
	if _, err := os.Stat(filePath); err == nil {
		return readBootstrapParams(filePath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("failed to stat %s: %w", filePath, err)
	}

	// File not found, likely its an older shim. Try migrate.

	address, err := shimbinary.ReadAddress(filepath.Join(bundlePath, "address"))
	if err != nil {
		return nil, fmt.Errorf("unable to migrate shim: failed to get socket address for bundle %s: %w", bundlePath, err)
	}

	params := bootapi.BootstrapResult{
		Version:  2,
		Address:  address,
		Protocol: "ttrpc",
	}

	if err := writeBootstrapParams(filePath, &params); err != nil {
		return nil, fmt.Errorf("unable to migrate: failed to write bootstrap.json file: %w", err)
	}

	return &params, nil
}

func (m *ShimManager) resolveRuntimePath(runtime string) (string, error) {
	if runtime == "" {
		return "", errors.New("no runtime name")
	}

	// Custom path to runtime binary
	if filepath.IsAbs(runtime) {
		// Make sure it exists before returning ok
		if _, err := os.Stat(runtime); err != nil {
			return "", fmt.Errorf("invalid custom binary path: %w", err)
		}

		return runtime, nil
	}

	// Check if relative path to runtime binary provided
	if strings.Contains(runtime, "/") {
		return "", fmt.Errorf("invalid runtime name %s, correct runtime name should be either format like `io.containerd.runc.v2` or a full path to the binary", runtime)
	}

	// Preserve existing logic and resolve runtime path from runtime name.

	name := shimbinary.BinaryName(runtime)
	if name == "" {
		return "", fmt.Errorf("invalid runtime name %s, correct runtime name should be either format like `io.containerd.runc.v2` or a full path to the binary", runtime)
	}

	if path, ok := m.runtimePaths.Load(name); ok {
		return path.(string), nil
	}

	var (
		cmdPath string
		lerr    error
	)

	binaryPath := shimbinary.BinaryPath(runtime)
	if _, serr := os.Stat(binaryPath); serr == nil {
		cmdPath = binaryPath
	}

	if cmdPath == "" {
		if cmdPath, lerr = exec.LookPath(name); lerr != nil {
			if eerr, ok := lerr.(*exec.Error); ok {
				if eerr.Err == exec.ErrNotFound {
					self, err := os.Executable()
					if err != nil {
						return "", err
					}

					// Match the calling binaries (containerd) path and see
					// if they are side by side. If so, execute the shim
					// found there.
					testPath := filepath.Join(filepath.Dir(self), name)
					if _, serr := os.Stat(testPath); serr == nil {
						cmdPath = testPath
					}
					if cmdPath == "" {
						return "", fmt.Errorf("runtime %q binary not installed %q: %w", runtime, name, os.ErrNotExist)
					}
				}
			}
		}
	}

	cmdPath, err := filepath.Abs(cmdPath)
	if err != nil {
		return "", err
	}

	if path, ok := m.runtimePaths.LoadOrStore(name, cmdPath); ok {
		// We didn't store cmdPath we loaded an already cached value. Use it.
		cmdPath = path.(string)
	}

	return cmdPath, nil
}

// cleanupShim attempts to properly delete and cleanup shim after error
func (m *ShimManager) cleanupShim(ctx context.Context, shim *shim) {
	dctx, cancel := timeout.WithContext(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()

	_ = shim.Delete(dctx)
	m.shims.Delete(dctx, shim.ID())
}

func (m *ShimManager) Get(ctx context.Context, id string) (ShimInstance, error) {
	return m.shims.Get(ctx, id)
}

// GetAll returns all shims registered with the manager. When all is false, only
// shims in the caller's namespace are returned.
func (m *ShimManager) GetAll(ctx context.Context, all bool) ([]ShimInstance, error) {
	return m.shims.GetAll(ctx, all)
}

// Remove drops a shim from the manager's map without deleting the shim itself.
// It is used by callers that perform their own shim teardown and only need the
// manager to forget the instance.
func (m *ShimManager) Remove(ctx context.Context, id string) {
	m.shims.Delete(ctx, id)
}

// ResolveRuntimePath resolves a runtime name or path to the shim binary path.
func (m *ShimManager) ResolveRuntimePath(runtime string) (string, error) {
	return m.resolveRuntimePath(runtime)
}

// Delete closes a shim's connection and removes its bundle from disk.
func (m *ShimManager) Delete(ctx context.Context, id string) error {
	shim, err := m.shims.Get(ctx, id)
	if err != nil {
		return err
	}

	err = shim.Delete(ctx)
	m.shims.Delete(ctx, id)

	return err
}

// CleanupTimeout returns the configured per-call budget for cleaning up a dead
// shim. Callers that drive their own cleanup use it to bound those calls.
func CleanupTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return timeout.WithContext(ctx, cleanupTimeout)
}

// ShutdownTimeout returns the configured budget for shutting a shim down.
func ShutdownTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return timeout.WithContext(ctx, shutdownTimeout)
}

// RestoreBootstrapParams reads a bundle's bootstrap.json (migrating an old
// address file if needed) to recover how to connect to its shim.
func RestoreBootstrapParams(bundlePath string) (*bootapi.BootstrapResult, error) {
	return restoreBootstrapParams(bundlePath)
}

// DeleteBundle invokes the shim binary's delete action for a dead shim's bundle,
// cleaning up its on-disk state and returning the exit it reports. It is used by
// callers reacting to a shim disconnect, where the in-memory client is already
// gone and only the bundle remains. runtime may be empty, in which case it is
// read from the bundle's shim-binary-path file.
func (m *ShimManager) DeleteBundle(ctx context.Context, bundle *Bundle, runtimeName string) (*runtime.Exit, error) {
	if runtimeName == "" {
		if data, err := os.ReadFile(filepath.Join(bundle.Path, "shim-binary-path")); err == nil {
			runtimeName = string(data)
		} else if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	if runtimeName == "" {
		return nil, fmt.Errorf("no runtime for bundle %q: unable to read shim-binary-path", bundle.ID)
	}
	runtimePath, err := m.resolveRuntimePath(runtimeName)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve runtime path: %w", err)
	}
	b := shimBinary(bundle, shimBinaryConfig{
		runtime:      runtimePath,
		address:      m.containerdAddress,
		ttrpcAddress: m.containerdTTRPCAddress,
		socketDir:    m.socketDir,
		env:          m.env,
	})
	return b.Delete(ctx)
}
