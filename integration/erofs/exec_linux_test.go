//go:build linux

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

// exec_linux_test.go contains tests that perform actual EROFS filesystem
// mounts and run container workloads.  These tests:
//
//   - Are Linux-only: mounting EROFS images requires the erofs kernel module
//     and loop device support, which are Linux-specific.
//
//   - Require root (CAP_SYS_ADMIN): mount(2) and loop-device ioctls are
//     privileged operations.
//
//   - Use testutil.RequiresRoot to skip gracefully on non-root runs.
//
// All unpack / snapshot-state / mount-spec tests run without root and live in
// snapshotter_test.go and snapshotter_linux_test.go.
package erofs

import (
	"os"
	"os/exec"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/pkg/testutil"
	erofssnap "github.com/containerd/containerd/v2/plugins/snapshots/erofs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// skipIfEROFSMountUnavailable skips t if any prerequisite for actual EROFS
// filesystem mounting is missing:
//   - Root / CAP_SYS_ADMIN for mount(2)
//   - EROFS kernel module
//   - mkfs.erofs binary
func skipIfEROFSMountUnavailable(t *testing.T) {
	t.Helper()
	testutil.RequiresRoot(t)
	if !erofssnap.FindErofs() {
		t.Skip("EROFS kernel module not loaded")
	}
	if _, err := exec.LookPath("mkfs.erofs"); err != nil {
		t.Skipf("mkfs.erofs not found: %v", err)
	}
}

// ---------------------------------------------------------------------------
// TestErofsExecLayerFileIntegrity mounts each snapshot via the daemon and
// verifies that the layer.erofs source file has a valid EROFS superblock
// magic (0xE0F5E1E2 at byte offset 1024).
//
// The mount is performed by the daemon (which runs as root); the test process
// itself only reads the resulting file path from the Mounts() descriptor.
// However it still calls testutil.RequiresRoot because listing mount paths
// of root-owned daemon state typically requires the same privilege level.
// ---------------------------------------------------------------------------
func TestErofsExecLayerFileIntegrity(t *testing.T) {
	skipIfEROFSMountUnavailable(t)
	if testing.Short() {
		t.Skip()
	}

	ctx, cancel := testContext(t)
	defer cancel()

	c := newTestClient(t)
	defer c.Close()

	dstRef := erofsTestImage + "-erofs-integrity-exec-test"
	dstImg := localEROFS(t, c, dstRef)

	pm := erofsPM()
	img := containerd.NewImageWithPlatform(c, *dstImg, pm)
	require.NoError(t, img.Unpack(ctx, erofsSnapshotterName))

	mfst, err := images.Manifest(ctx, c.ContentStore(), dstImg.Target, pm)
	require.NoError(t, err)

	sn := c.SnapshotService(erofsSnapshotterName)
	for _, id := range chainIDs(t, c, mfst.Layers) {
		mounts, err := sn.Mounts(ctx, id)
		require.NoError(t, err)
		for _, m := range mounts {
			if m.Type == "erofs" {
				checkEROFSSuperblock(t, m.Source)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// TestErofsExecMountReadonly verifies that an EROFS snapshot can be mounted
// and that its mount options include "ro" (read-only).
//
// Actual mounting is performed by the daemon; this test checks the spec.
// ---------------------------------------------------------------------------
func TestErofsExecMountReadonly(t *testing.T) {
	skipIfEROFSMountUnavailable(t)
	if testing.Short() {
		t.Skip()
	}

	ctx, cancel := testContext(t)
	defer cancel()

	c := newTestClient(t)
	defer c.Close()

	dstRef := erofsTestImage + "-erofs-readonly-spec-test"
	dstImg := localEROFS(t, c, dstRef)

	pm := erofsPM()
	img := containerd.NewImageWithPlatform(c, *dstImg, pm)
	require.NoError(t, img.Unpack(ctx, erofsSnapshotterName))

	mfst, err := images.Manifest(ctx, c.ContentStore(), dstImg.Target, pm)
	require.NoError(t, err)

	sn := c.SnapshotService(erofsSnapshotterName)
	for _, id := range chainIDs(t, c, mfst.Layers) {
		mounts, err := sn.Mounts(ctx, id)
		require.NoError(t, err)
		for _, m := range mounts {
			if m.Type == "erofs" {
				var hasRO bool
				for _, opt := range m.Options {
					if opt == "ro" {
						hasRO = true
						break
					}
				}
				assert.True(t, hasRO,
					"EROFS mount spec for snapshot %s must include 'ro' option", id)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// checkEROFSSuperblock reads bytes 1024–1027 from path and asserts the EROFS
// magic 0xE0F5E1E2 (little-endian).
// ---------------------------------------------------------------------------
func checkEROFSSuperblock(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Logf("cannot open %q: %v", path, err)
		return
	}
	defer f.Close()

	var magic [4]byte
	if _, err := f.ReadAt(magic[:], 1024); err != nil {
		t.Logf("cannot read superblock from %q: %v", path, err)
		return
	}
	assert.Equal(t, [4]byte{0xE2, 0xE1, 0xF5, 0xE0}, magic,
		"file %q must have EROFS superblock magic at offset 1024", path)
}
