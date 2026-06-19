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

// snapshotter_test.go contains cross-platform integration tests for the EROFS
// snapshotter that run on Linux, macOS, and Windows without requiring root.
//
// # Why these tests do not require root
//
// The EROFS unpack pipeline (Prepare → Apply → Commit) issues zero mount(2)
// syscalls and does not need the EROFS kernel module:
//
//   - Prepare()  creates directories and writes a boltdb record.
//   - Apply()    (EROFS+zstd path) decompresses zstd and writes layer.erofs.
//   - Commit()   detects layer.erofs exists and writes a boltdb record.
//   - Stat() / Mounts() are pure boltdb reads.
//
// Root and the EROFS kernel module are only needed when snapshots are
// *actually mounted* for container execution.  Those tests live in
// exec_linux_test.go.
//
// # Image source
//
// Tests in this file use pre-converted EROFS images from the registry
// (docker.io/dmcgowan/ by default, overridable via -image-list).
// Tests that build images via local conversion are in snapshotter_linux_test.go.
//
// # Platform availability
//
// skipIfErofsSnapshotterUnavailable() skips gracefully on macOS/Windows where
// the daemon's EROFS snapshotter plugin will not be loaded.
package erofs

import (
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/snapshots"
	imagelist "github.com/containerd/containerd/v2/integration/images"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// erofsSnapshotterName is the containerd snapshotter name for EROFS.
const erofsSnapshotterName = "erofs"

// skipIfErofsSnapshotterUnavailable skips t when the running containerd daemon
// does not expose the "erofs" snapshotter plugin.  This occurs on macOS and
// Windows, and on Linux when the EROFS kernel module is not loaded.
// No root is required for this check.
func skipIfErofsSnapshotterUnavailable(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("short mode")
	}
	c := newTestClient(t)
	defer c.Close()

	ctx, cancel := testContext(t)
	defer cancel()

	_, err := c.GetSnapshotterCapabilities(ctx, erofsSnapshotterName)
	if err != nil {
		t.Skipf("erofs snapshotter not available: %v", err)
	}
}

// ---------------------------------------------------------------------------
// TestErofsSnapshotterPullAndUnpackMerged pulls a pre-converted merged EROFS
// image from the configured registry, unpacks it with the EROFS snapshotter,
// and verifies the snapshot is in KindCommitted state.
//
// No root required: the daemon-side unpack pipeline (Prepare→Apply→Commit)
// issues no mount(2) syscalls.
// ---------------------------------------------------------------------------
func TestErofsSnapshotterPullAndUnpackMerged(t *testing.T) {
	skipIfErofsSnapshotterUnavailable(t)

	imageName := imagelist.Get(imagelist.ErofsAlpineMerge)
	require.NotEmpty(t, imageName)

	ctx, cancel := testContext(t)
	defer cancel()

	c := newTestClient(t)
	defer c.Close()
	_ = c.ImageService().Delete(ctx, imageName, images.SynchronousDelete())

	pm := erofsPM()
	img, err := c.Pull(ctx, imageName,
		containerd.WithPlatformMatcher(pm),
		containerd.WithPullSnapshotter(erofsSnapshotterName),
		containerd.WithPullUnpack,
	)
	if err != nil {
		if isNetworkError(err) {
			t.Skipf("image %q not reachable: %v", imageName, err)
		}
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		ctx2, c2 := testContext(t)
		defer c2()
		_ = c.ImageService().Delete(ctx2, imageName, images.SynchronousDelete())
	})

	// Verify IsUnpacked — pure boltdb + chain ID check, no root.
	unpacked, err := img.IsUnpacked(ctx, erofsSnapshotterName)
	require.NoError(t, err)
	assert.True(t, unpacked,
		"IsUnpacked must return true: unpack only writes layer.erofs, no mount(2)")

	// Verify all snapshots are committed — Stat() is a boltdb read.
	mfst, err := images.Manifest(ctx, c.ContentStore(), img.Target(), pm)
	require.NoError(t, err)
	require.Len(t, mfst.Layers, 1, "merged EROFS must have exactly one layer")
	assert.Equal(t, "application/vnd.erofs+zstd", mfst.Layers[0].MediaType)

	sn := c.SnapshotService(erofsSnapshotterName)
	for _, id := range chainIDs(t, c, mfst.Layers) {
		info, err := sn.Stat(ctx, id)
		require.NoError(t, err, "snapshot %s must exist after unpack", id)
		assert.Equal(t, snapshots.KindCommitted, info.Kind,
			"snapshot %s must be committed", id)
	}
}

// ---------------------------------------------------------------------------
// TestErofsSnapshotterPullAndUnpackPerLayer pulls a pre-converted per-layer
// EROFS image and verifies each layer's snapshot.
// ---------------------------------------------------------------------------
func TestErofsSnapshotterPullAndUnpackPerLayer(t *testing.T) {
	skipIfErofsSnapshotterUnavailable(t)

	imageName := imagelist.Get(imagelist.ErofsAlpine)
	require.NotEmpty(t, imageName)

	ctx, cancel := testContext(t)
	defer cancel()

	c := newTestClient(t)
	defer c.Close()
	_ = c.ImageService().Delete(ctx, imageName, images.SynchronousDelete())

	pm := erofsPM()
	img, err := c.Pull(ctx, imageName,
		containerd.WithPlatformMatcher(pm),
		containerd.WithPullSnapshotter(erofsSnapshotterName),
		containerd.WithPullUnpack,
	)
	if err != nil {
		if isNetworkError(err) {
			t.Skipf("image %q not reachable: %v", imageName, err)
		}
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		ctx2, c2 := testContext(t)
		defer c2()
		_ = c.ImageService().Delete(ctx2, imageName, images.SynchronousDelete())
	})

	unpacked, err := img.IsUnpacked(ctx, erofsSnapshotterName)
	require.NoError(t, err)
	assert.True(t, unpacked)

	mfst, err := images.Manifest(ctx, c.ContentStore(), img.Target(), pm)
	require.NoError(t, err)

	sn := c.SnapshotService(erofsSnapshotterName)
	for _, id := range chainIDs(t, c, mfst.Layers) {
		info, err := sn.Stat(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, snapshots.KindCommitted, info.Kind)
	}
}

// ---------------------------------------------------------------------------
// TestErofsSnapshotterMountsSpec verifies that Mounts() returns non-empty
// mount descriptors for a committed EROFS snapshot.
// Mounts() issues no mount(2) syscall — it returns descriptor structs only.
// ---------------------------------------------------------------------------
func TestErofsSnapshotterMountsSpec(t *testing.T) {
	skipIfErofsSnapshotterUnavailable(t)

	imageName := imagelist.Get(imagelist.ErofsAlpineMerge)
	require.NotEmpty(t, imageName)

	ctx, cancel := testContext(t)
	defer cancel()

	c := newTestClient(t)
	defer c.Close()
	_ = c.ImageService().Delete(ctx, imageName, images.SynchronousDelete())

	pm := erofsPM()
	img, err := c.Pull(ctx, imageName,
		containerd.WithPlatformMatcher(pm),
		containerd.WithPullSnapshotter(erofsSnapshotterName),
		containerd.WithPullUnpack,
	)
	if err != nil {
		if isNetworkError(err) {
			t.Skipf("image %q not reachable: %v", imageName, err)
		}
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		ctx2, c2 := testContext(t)
		defer c2()
		_ = c.ImageService().Delete(ctx2, imageName, images.SynchronousDelete())
	})

	mfst, err := images.Manifest(ctx, c.ContentStore(), img.Target(), pm)
	require.NoError(t, err)

	sn := c.SnapshotService(erofsSnapshotterName)
	for _, id := range chainIDs(t, c, mfst.Layers) {
		mounts, err := sn.Mounts(ctx, id)
		require.NoError(t, err, "Mounts() is a metadata-only call, must not require root")
		assert.NotEmpty(t, mounts,
			"Mounts() must return at least one mount descriptor")
		for _, m := range mounts {
			assert.NotEmpty(t, m.Source,
				"mount descriptor source path must be non-empty")
			t.Logf("snapshot %s mount: type=%s source=%s", id, m.Type, m.Source)
		}
	}
}

// ---------------------------------------------------------------------------
// TestErofsSnapshotterIsUnpackedFalseForTarImage verifies that an image
// unpacked with the default (overlayfs) snapshotter is NOT reported as
// unpacked by the EROFS snapshotter — namespace isolation check.
// ---------------------------------------------------------------------------
func TestErofsSnapshotterIsUnpackedFalseForTarImage(t *testing.T) {
	skipIfErofsSnapshotterUnavailable(t)

	ctx, cancel := testContext(t)
	defer cancel()

	c := newTestClient(t)
	defer c.Close()

	// Pull and unpack with the default snapshotter (tar path, may need root
	// on some setups — skip gracefully).
	const seedImage = "ghcr.io/containerd/alpine:3.14.0"
	img, err := c.Pull(ctx, seedImage,
		containerd.WithPlatformMatcher(platforms.DefaultStrict()),
		containerd.WithPullSnapshotter(testSnapshotter),
		containerd.WithPullUnpack,
	)
	if err != nil {
		if isNetworkError(err) {
			t.Skipf("seed image not reachable: %v", err)
		}
		// On some platforms unpacking tar may need elevated privileges.
		t.Skipf("could not unpack tar image (may need elevated privileges): %v", err)
	}
	t.Cleanup(func() {
		ctx2, c2 := testContext(t)
		defer c2()
		_ = c.ImageService().Delete(ctx2, seedImage, images.SynchronousDelete())
	})

	unpackedDefault, err := img.IsUnpacked(ctx, testSnapshotter)
	require.NoError(t, err)
	assert.True(t, unpackedDefault)

	// EROFS snapshotter must NOT see this as unpacked (different namespace).
	unpackedErofs, err := img.IsUnpacked(ctx, erofsSnapshotterName)
	if err != nil && !errdefs.IsNotFound(err) {
		t.Logf("IsUnpacked(erofs) error (acceptable for this test): %v", err)
	}
	assert.False(t, unpackedErofs,
		"image unpacked with overlayfs snapshotter must NOT be reported "+
			"as unpacked by the erofs snapshotter")
}
