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

// snapshotter_linux_test.go contains Linux-specific snapshotter tests that
// use local image conversion (which depends on Linux-only packages) instead
// of pulling pre-converted images from a remote registry.
//
// These tests still do NOT require root: the EROFS differ writes layer.erofs
// via plain file I/O with no mount(2) syscall.
package erofs

import (
	"bytes"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/archive"
	"github.com/containerd/containerd/v2/core/images/converter"
	erofsconv "github.com/containerd/containerd/v2/core/images/converter/erofs"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/platforms"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// TestErofsSnapshotterLocalConvertAndUnpack converts a local tar image to
// EROFS (no registry needed after the initial fetch) and unpacks it.
//
// Does NOT require root: Prepare→Apply→Commit uses no mount(2) calls.
// ---------------------------------------------------------------------------
func TestErofsSnapshotterLocalConvertAndUnpack(t *testing.T) {
	skipIfErofsSnapshotterUnavailable(t)

	ctx, cancel := testContext(t)
	defer cancel()

	c := newTestClient(t)
	defer c.Close()

	dstRef := erofsTestImage + "-erofs-local-unpack-test"
	dstImg := localEROFS(t, c, dstRef)

	pm := erofsPM()
	img := containerd.NewImageWithPlatform(c, *dstImg, pm)

	require.NoError(t, img.Unpack(ctx, erofsSnapshotterName),
		"Unpack must succeed: EROFS differ writes layer.erofs via file I/O, "+
			"no mount(2) syscall is issued")

	unpacked, err := img.IsUnpacked(ctx, erofsSnapshotterName)
	require.NoError(t, err)
	assert.True(t, unpacked)

	mfst, err := images.Manifest(ctx, c.ContentStore(), dstImg.Target, pm)
	require.NoError(t, err)
	sn := c.SnapshotService(erofsSnapshotterName)
	for _, id := range chainIDs(t, c, mfst.Layers) {
		info, err := sn.Stat(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, snapshots.KindCommitted, info.Kind)
	}
}

// ---------------------------------------------------------------------------
// TestErofsSnapshotterLocalSnapshotState verifies KindCommitted for all
// snapshots after local conversion and unpack.
// ---------------------------------------------------------------------------
func TestErofsSnapshotterLocalSnapshotState(t *testing.T) {
	skipIfErofsSnapshotterUnavailable(t)

	ctx, cancel := testContext(t)
	defer cancel()

	c := newTestClient(t)
	defer c.Close()

	dstRef := erofsTestImage + "-erofs-snap-state-test"
	dstImg := localEROFS(t, c, dstRef)

	pm := erofsPM()
	img := containerd.NewImageWithPlatform(c, *dstImg, pm)
	require.NoError(t, img.Unpack(ctx, erofsSnapshotterName))

	mfst, err := images.Manifest(ctx, c.ContentStore(), dstImg.Target, pm)
	require.NoError(t, err)

	sn := c.SnapshotService(erofsSnapshotterName)
	for _, id := range chainIDs(t, c, mfst.Layers) {
		info, err := sn.Stat(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, snapshots.KindCommitted, info.Kind)
	}
}

// ---------------------------------------------------------------------------
// TestErofsSnapshotterLocalMountsSpec verifies that Mounts() returns valid
// mount descriptors after a local-conversion unpack.  Mounts() issues no
// mount(2) syscall — root not required.
// ---------------------------------------------------------------------------
func TestErofsSnapshotterLocalMountsSpec(t *testing.T) {
	skipIfErofsSnapshotterUnavailable(t)

	ctx, cancel := testContext(t)
	defer cancel()

	c := newTestClient(t)
	defer c.Close()

	dstRef := erofsTestImage + "-erofs-mounts-test"
	dstImg := localEROFS(t, c, dstRef)

	pm := erofsPM()
	img := containerd.NewImageWithPlatform(c, *dstImg, pm)
	require.NoError(t, img.Unpack(ctx, erofsSnapshotterName))

	mfst, err := images.Manifest(ctx, c.ContentStore(), dstImg.Target, pm)
	require.NoError(t, err)

	sn := c.SnapshotService(erofsSnapshotterName)
	for _, id := range chainIDs(t, c, mfst.Layers) {
		mounts, err := sn.Mounts(ctx, id)
		require.NoError(t, err,
			"Mounts() is metadata-only, must not require root")
		assert.NotEmpty(t, mounts)
		for _, m := range mounts {
			assert.NotEmpty(t, m.Source)
		}
	}
}

// ---------------------------------------------------------------------------
// TestErofsSnapshotterLocalUnpackParallel converts and unpacks the same image
// in three concurrent goroutines to verify thread-safety in the snapshotter's
// metadata layer.  No root required.
// ---------------------------------------------------------------------------
func TestErofsSnapshotterLocalUnpackParallel(t *testing.T) {
	skipIfErofsSnapshotterUnavailable(t)

	ctx, cancel := testContext(t)
	defer cancel()

	c := newTestClient(t)
	defer c.Close()

	// Pre-fetch the seed so all goroutines share the same content blobs.
	_, err := c.Fetch(ctx, erofsTestImage,
		containerd.WithPlatform(platforms.DefaultString()))
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx2, c2 := testContext(t)
		defer c2()
		_ = c.ImageService().Delete(ctx2, erofsTestImage, images.SynchronousDelete())
	})

	const parallelism = 3
	errs := make(chan error, parallelism)

	for i := 0; i < parallelism; i++ {
		idx := i
		go func() {
			ref := erofsTestImage + "-erofs-par-" + string(rune('a'+idx))
			t.Cleanup(func() {
				ctx2, c2 := testContext(t)
				defer c2()
				_ = c.ImageService().Delete(ctx2, ref, images.SynchronousDelete())
			})
			ctx2, cancel2 := testContext(t)
			defer cancel2()

			converted, convertErr := converter.Convert(ctx2, c, ref, erofsTestImage,
				converter.WithUpdateManifest(erofsconv.MergeManifestFunc(
					erofsconv.WithBlobCompression("zstd"))),
				converter.WithPlatform(platforms.DefaultStrict()),
			)
			if convertErr != nil {
				errs <- convertErr
				return
			}
			wrapped := containerd.NewImageWithPlatform(c, *converted, erofsPM())
			errs <- wrapped.Unpack(ctx2, erofsSnapshotterName)
		}()
	}

	timer := time.NewTimer(120 * time.Second)
	defer timer.Stop()
	for i := 0; i < parallelism; i++ {
		select {
		case unpackErr := <-errs:
			assert.NoError(t, unpackErr, "parallel goroutine %d failed", i)
		case <-timer.C:
			t.Fatal("timeout waiting for parallel EROFS unpacks")
		}
	}
}

// ---------------------------------------------------------------------------
// TestErofsSnapshotterOCIArchiveRoundtrip exercises the full workflow using
// locally converted images:
//  1. Convert tar → EROFS
//  2. Unpack with EROFS snapshotter  (no root: file I/O only)
//  3. Export to OCI archive          (no root: content store reads)
//  4. Import archive                 (no root: content store writes)
//  5. Re-unpack the imported image   (no root: file I/O only)
// ---------------------------------------------------------------------------
func TestErofsSnapshotterOCIArchiveRoundtrip(t *testing.T) {
	skipIfErofsSnapshotterUnavailable(t)

	ctx, cancel := testContext(t)
	defer cancel()

	c := newTestClient(t)
	defer c.Close()

	srcRef := erofsTestImage + "-erofs-rt-src"
	srcDst := localEROFS(t, c, srcRef)

	pm := erofsPM()
	srcImg := containerd.NewImageWithPlatform(c, *srcDst, pm)
	require.NoError(t, srcImg.Unpack(ctx, erofsSnapshotterName))

	// Export to in-memory buffer (no root needed).
	var buf bytes.Buffer
	err := c.Export(ctx, &buf,
		archive.WithImage(c.ImageService(), srcRef),
		archive.WithPlatform(pm),
		archive.WithSkipDockerManifest(),
	)
	require.NoError(t, err)
	require.Greater(t, buf.Len(), 1024)

	// Import.
	importedRef := erofsTestImage + "-erofs-rt-imported"
	t.Cleanup(func() {
		ctx2, c2 := testContext(t)
		defer c2()
		_ = c.ImageService().Delete(ctx2, importedRef, images.SynchronousDelete())
	})

	importedImgs, err := c.Import(ctx, &buf,
		containerd.WithImageRefTranslator(func(string) string { return importedRef }),
	)
	require.NoError(t, err)
	require.NotEmpty(t, importedImgs)

	// Re-unpack.
	imported := containerd.NewImageWithPlatform(c, importedImgs[0], pm)
	require.NoError(t, imported.Unpack(ctx, erofsSnapshotterName),
		"EROFS image must be re-unpackable after OCI archive round-trip")

	unpacked, err := imported.IsUnpacked(ctx, erofsSnapshotterName)
	require.NoError(t, err)
	assert.True(t, unpacked)
}
