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

//go:build linux

// daemon_rootless_linux_test.go starts a containerd daemon inside a user
// namespace so that tests can exercise the full daemon plugin stack without
// requiring real root.
//
// # Design
//
// The daemon runs under:
//
//	unshare --user --map-root-user --mount --
//	    <containerd-binary> --root <tmpdir>/root --state <tmpdir>/state \
//	        --address <tmpdir>/containerd.sock --config <cfg>
//
// --user + --map-root-user: maps the current UID/GID to 0/0 inside the
// namespace.  The daemon sees itself as root and can perform file I/O that
// requires o+rw on its own state directories.
//
// --mount: gives the daemon a private mount namespace so EROFS snapshotter
// mounts (if any) don't leak into the host.
//
// The containerd binary must be pre-built by the test runner; pass its path
// via the -containerd-binary flag or the CONTAINERD_BINARY env var.  If
// neither is set, the test looks for a binary built to /tmp/containerd-erofs-test.
//
// # Snapshotter config
//
// The daemon config enables:
//   - plugins.'io.containerd.snapshotter.v1.erofs'   (EROFS snapshotter)
//   - plugins.'io.containerd.content.index.v1.local' (indexed content store)
//   - plugins.'io.containerd.diff.v1.erofs'           (EROFS differ)
//   - plugins.'io.containerd.service.v1.transfer'     (transfer service)
//
// with the diff ordering set to ["erofs", "walking"] so EROFS layers are
// unpacked by the EROFS differ.
//
// # Limitations
//
// Mount(2) for actual EROFS kernel mounts still requires real CAP_SYS_ADMIN
// in the initial user namespace.  Tests that need real mounts must run as
// root (they skip via testutil.RequiresRoot).  Tests that only need pull +
// lazy unpack (writing layer.indexed) work fine rootless.
package erofs

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/testutil"
	"github.com/containerd/log/logtest"
)

var (
	containerdBinary = flag.String("containerd-binary", defaultContainerdBinary(),
		"Path to the containerd binary for rootless daemon tests")
)

func defaultContainerdBinary() string {
	// Prefer a binary built from the current tree.
	if p := os.Getenv("CONTAINERD_BINARY"); p != "" {
		return p
	}
	if _, err := os.Stat("/tmp/containerd-erofs-test"); err == nil {
		return "/tmp/containerd-erofs-test"
	}
	// Fall back to the system containerd.
	if p, err := exec.LookPath("containerd"); err == nil {
		return p
	}
	return "containerd"
}

// rootlessDaemon wraps a containerd daemon started under unshare --user.
type rootlessDaemon struct {
	cmd       *exec.Cmd
	sock      string
	stateDir  string
	rootDir   string
	configPath string
}

// rootlessDaemonOpts configures a rootless daemon.
type rootlessDaemonOpts struct {
	// ExtraConfig is additional TOML appended after the base config.
	ExtraConfig string
	// Namespace for the test context (default: "erofs-rootless-test")
	Namespace string
}

// startRootlessDaemon starts a containerd daemon under an unprivileged user
// namespace and returns a *rootlessDaemon.  The daemon is stopped and its
// state is cleaned up via t.Cleanup.
//
// Skips the test if:
//   - The containerd binary is not found.
//   - unshare is not available.
//   - User namespaces are not permitted (kernel.unprivileged_userns_clone != 1).
func startRootlessDaemon(t *testing.T, opts rootlessDaemonOpts) (*rootlessDaemon, *containerd.Client) {
	t.Helper()

	if testing.Short() {
		t.Skip("rootless daemon tests require daemon lifecycle; skipped in -short mode")
	}

	bin := *containerdBinary
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("containerd binary not found at %s (set -containerd-binary or build with: go build -o /tmp/containerd-erofs-test ./cmd/containerd/): %v", bin, err)
	}
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skipf("unshare not in PATH: %v", err)
	}

	// Check unprivileged user namespace support.
	data, err := os.ReadFile("/proc/sys/kernel/unprivileged_userns_clone")
	if err == nil && len(data) > 0 && data[0] == '0' {
		t.Skip("unprivileged user namespaces disabled (kernel.unprivileged_userns_clone=0)")
	}

	ns := opts.Namespace
	if ns == "" {
		ns = "erofs-rootless-test"
	}

	// Create isolated state directories under TempDir.
	stateBase := t.TempDir()
	rootDir := filepath.Join(stateBase, "root")
	stateDir := filepath.Join(stateBase, "state")
	sock := filepath.Join(stateBase, "containerd.sock")
	logFile := filepath.Join(stateBase, "containerd.log")

	for _, d := range []string{rootDir, stateDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	cfg := rootlessDaemonConfig(rootDir, stateDir, sock, opts.ExtraConfig)
	cfgPath := filepath.Join(stateBase, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	logF, err := os.Create(logFile)
	if err != nil {
		t.Fatalf("create log file: %v", err)
	}
	t.Cleanup(func() { logF.Close() })

	// Start the daemon under a user namespace.
	cmd := exec.Command("unshare",
		"--user", "--map-root-user",
		"--mount",
		"--",
		bin,
		"--root", rootDir,
		"--state", stateDir,
		"--address", sock,
		"--log-level", "debug",
		"--config", cfgPath,
	)
	cmd.Stdout = logF
	cmd.Stderr = logF
	if err := cmd.Start(); err != nil {
		t.Fatalf("start rootless containerd: %v", err)
	}

	d := &rootlessDaemon{
		cmd:        cmd,
		sock:       sock,
		stateDir:   stateDir,
		rootDir:    rootDir,
		configPath: cfgPath,
	}

	t.Cleanup(func() {
		_ = d.stop()
		if t.Failed() {
			// Dump the daemon log on failure for debugging.
			if data, err := os.ReadFile(logFile); err == nil {
				lines := string(data)
				// Print last 100 lines.
				if len(lines) > 8192 {
					lines = "...\n" + lines[len(lines)-8192:]
				}
				t.Logf("=== containerd log ===\n%s\n=== end log ===", lines)
			}
		}
		_ = os.RemoveAll(stateBase)
	})

	// Wait for the daemon to be ready.
	waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var client *containerd.Client
	for {
		select {
		case <-waitCtx.Done():
			d.stop() //nolint:errcheck
			t.Fatalf("rootless containerd did not become ready within 30s; log: %s", logFile)
		case <-time.After(200 * time.Millisecond):
		}
		c, err := containerd.New(sock)
		if err != nil {
			continue
		}
		ok, err := c.IsServing(waitCtx)
		if err != nil || !ok {
			c.Close()
			continue
		}
		// Check plugins loaded OK.
		resp, err := c.IntrospectionService().Plugins(waitCtx)
		if err != nil {
			c.Close()
			continue
		}
		allOK := true
		for _, p := range resp.Plugins {
			if p.InitErr != nil {
				t.Logf("plugin %s.%s init error: %s", p.Type, p.ID, p.InitErr.Message)
				// Don't treat non-fatal plugin failures as daemon failures.
				// Some plugins (e.g. overlayfs snapshotter) legitimately fail
				// in user namespaces.
			}
		}
		if allOK {
			client = c
			break
		}
		c.Close()
	}

	t.Logf("rootless containerd ready at %s", sock)
	return d, client
}

// stop terminates the daemon gracefully, then forcibly if needed.
func (d *rootlessDaemon) stop() error {
	if d.cmd == nil || d.cmd.Process == nil {
		return nil
	}
	_ = d.cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- d.cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		_ = d.cmd.Process.Kill()
		return <-done
	}
}

// rootlessTestContext returns a context with the given namespace and a per-test
// logger, for use with the rootless daemon.
func rootlessTestContext(t *testing.T, ns string) context.Context {
	ctx := context.Background() //nolint:all
	ctx = namespaces.WithNamespace(ctx, ns)
	ctx = logtest.WithT(ctx, t)
	return ctx
}

// skipIfNoRootlessDaemon skips the test if the containerd binary is not
// available for rootless testing.
func skipIfNoRootlessDaemon(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("rootless daemon test skipped in -short mode")
	}
	bin := *containerdBinary
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("containerd binary not available for rootless testing: %v", err)
	}
	if _, err := exec.LookPath("unshare"); err != nil {
		t.Skipf("unshare not found: %v", err)
	}
}

// skipIfNeedsRootForMount skips when the test requires real CAP_SYS_ADMIN for
// mount(2) (i.e. kernel EROFS mounts over loop devices).
func skipIfNeedsRootForMount(t *testing.T) {
	t.Helper()
	testutil.RequiresRoot(t)
	if !findErofsKernel() {
		t.Skip("EROFS kernel module not loaded")
	}
}

// rootlessDaemonConfig generates a containerd TOML config for the rootless
// EROFS test daemon.
func rootlessDaemonConfig(rootDir, stateDir, sock, extraConfig string) string {
	return fmt.Sprintf(`version = 4
root   = %q
state  = %q

[debug]
  level = "debug"

# Disable plugins that fail in user namespaces or that we don't need.
disabled_plugins = [
  "io.containerd.snapshotter.v1.overlayfs",
  "io.containerd.snapshotter.v1.native",
  "io.containerd.snapshotter.v1.btrfs",
  "io.containerd.snapshotter.v1.zfs",
  "io.containerd.tracing.processor.v1.otlp",
  "io.containerd.internal.v1.tracing",
  "io.containerd.nri.v1.nri",
  "io.containerd.grpc.v1.cri",
]

[plugins]
  # EROFS snapshotter
  [plugins.'io.containerd.snapshotter.v1.erofs']
    # No special options needed for lazy tests.

  # EROFS differ with no dmverity (may not be supported in user-ns)
  [plugins.'io.containerd.diff.v1.erofs']
    enable_dmverity = false

  # Diff ordering: try EROFS differ first, fall back to walking.
  [plugins.'io.containerd.service.v1.diff']
    default_differ  = "erofs"

%s
`, rootDir, stateDir, extraConfig)
}

// containerdSockPath returns the unix socket path for the rootless daemon.
func (d *rootlessDaemon) sockPath() string { return d.sock }
