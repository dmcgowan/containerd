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

// convert_linux_test.go contains integration tests for the EROFS converter.
// These tests use converter/erofs which has Linux-only transitive dependencies
// via erofsutils/dmverity.go → go-dmverity/pkg/keyring.
//
// Tests here do NOT require root, the EROFS kernel module, or mkfs.erofs.
// They only need a running containerd daemon to call converter.Convert.
package erofs

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/content"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/archive"
	"github.com/containerd/containerd/v2/core/images/converter"
	"github.com/containerd/containerd/v2/core/images/converter/erofs"
	imagelist "github.com/containerd/containerd/v2/integration/images"
	"github.com/containerd/errdefs"
	"github.com/containerd/platforms"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fetchSeed ensures erofsTestImage is present in the content store.
func fetchSeed(t *testing.T, c *containerd.Client) {
	t.Helper()
	ctx, cancel := testContext(t)
	defer cancel()
	_, err := c.Fetch(ctx, erofsTestImage,
		containerd.WithPlatform(platforms.DefaultString()))
	require.NoError(t, err, "fetch seed image %s", erofsTestImage)
	t.Cleanup(func() {
		ctx2, cancel2 := testContext(t)
		defer cancel2()
		_ = c.ImageService().Delete(ctx2, erofsTestImage, images.SynchronousDelete())
	})
}

// ---------------------------------------------------------------------------
// TestErofsImagePull verifies that a dual-format EROFS image index contains
// both plain-linux and EROFS manifests.
// ---------------------------------------------------------------------------
func TestErofsImagePull(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	imageName := imagelist.Get(imagelist.ErofsAlpine)
	ctx, cancel := testContext(t)
	defer cancel()

	c := newTestClient(t)
	defer c.Close()
	_ = c.ImageService().Delete(ctx, imageName, images.SynchronousDelete())

	img, err := c.Pull(ctx, imageName, containerd.WithAllMetadata())
	if err != nil {
		if isNetworkError(err) {
			t.Skipf("EROFS image not reachable: %v", err)
		}
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		ctx2, c2 := testContext(t)
		defer c2()
		_ = c.ImageService().Delete(ctx2, imageName, images.SynchronousDelete())
	})

	plats, err := images.Platforms(ctx, c.ContentStore(), img.Target())
	require.NoError(t, err)

	var hasEROFS, hasPlainLinux bool
	for _, p := range plats {
		for _, f := range p.OSFeatures {
			if f == "erofs" {
				hasEROFS = true
			}
		}
		if p.OS == "linux" && len(p.OSFeatures) == 0 {
			hasPlainLinux = true
		}
	}
	assert.True(t, hasEROFS, "index must contain at least one EROFS manifest")
	assert.True(t, hasPlainLinux, "index must retain at least one plain linux manifest")
}

// ---------------------------------------------------------------------------
// TestErofsImagePullMerged pulls a merged EROFS image and checks that the
// manifest has exactly one layer with the canonical EROFS+zstd media type
// and the required uncompressed-digest annotation.
// ---------------------------------------------------------------------------
func TestErofsImagePullMerged(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	imageName := imagelist.Get(imagelist.ErofsAlpineMerge)
	ctx, cancel := testContext(t)
	defer cancel()

	c := newTestClient(t)
	defer c.Close()
	_ = c.ImageService().Delete(ctx, imageName, images.SynchronousDelete())

	pm := erofsPM()
	img, err := c.Pull(ctx, imageName, containerd.WithPlatformMatcher(pm))
	if err != nil {
		if isNetworkError(err) {
			t.Skipf("merged EROFS image not reachable: %v", err)
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

	require.Len(t, mfst.Layers, 1, "merged EROFS must have exactly one layer")
	assert.Equal(t, contentindex.MediaTypeEROFSZstd, mfst.Layers[0].MediaType)
	_, hasAnnot := mfst.Layers[0].Annotations[contentindex.AnnotationUncompressedDigest]
	assert.True(t, hasAnnot, "merged layer must carry org.erofs.uncompressed-digest")
}

// ---------------------------------------------------------------------------
// TestErofsConvertPerLayer converts a tar image to a dual-format EROFS+zstd
// index (per-layer, append mode) and verifies layer media types/annotations.
// ---------------------------------------------------------------------------
func TestErofsConvertPerLayer(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	ctx, cancel := testContext(t)
	defer cancel()

	c := newTestClient(t)
	defer c.Close()
	fetchSeed(t, c)

	dstRef := erofsTestImage + "-erofs-perlay-test"
	t.Cleanup(func() {
		ctx2, c2 := testContext(t)
		defer c2()
		_ = c.ImageService().Delete(ctx2, dstRef, images.SynchronousDelete())
	})

	opts := []converter.Opt{
		converter.WithLayerConvertFunc(erofs.LayerConvertFunc(erofs.WithBlobCompression("zstd"))),
		converter.WithUpdateManifest(erofs.UpdateManifestPlatform),
		converter.WithPlatform(platforms.DefaultStrict()),
		converter.WithAppendToIndex(),
	}
	dstImg, err := converter.Convert(ctx, c, dstRef, erofsTestImage, opts...)
	require.NoError(t, err)

	cs := c.ContentStore()
	pm := erofsPM()

	mfst, err := images.Manifest(ctx, cs, dstImg.Target, pm)
	require.NoError(t, err)

	for i, l := range mfst.Layers {
		assert.Equal(t, contentindex.MediaTypeEROFSZstd, l.MediaType,
			"layer %d must use EROFS+zstd", i)
		_, ok := l.Annotations[contentindex.AnnotationUncompressedDigest]
		assert.True(t, ok, "layer %d must carry org.erofs.uncompressed-digest", i)
	}

	// The original tar manifest must still be present (dual-format ordering).
	tarMfst, err := images.Manifest(ctx, cs, dstImg.Target, platforms.DefaultStrict())
	require.NoError(t, err)
	for i, l := range tarMfst.Layers {
		assert.NotEqual(t, contentindex.MediaTypeEROFSZstd, l.MediaType,
			"tar manifest layer %d must not be EROFS+zstd", i)
	}
}

// ---------------------------------------------------------------------------
// TestErofsConvertMerge converts a tar image to a single merged EROFS layer
// and verifies the resulting manifest.
// ---------------------------------------------------------------------------
func TestErofsConvertMerge(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	ctx, cancel := testContext(t)
	defer cancel()

	c := newTestClient(t)
	defer c.Close()
	fetchSeed(t, c)

	dstRef := erofsTestImage + "-erofs-merge-test"
	t.Cleanup(func() {
		ctx2, c2 := testContext(t)
		defer c2()
		_ = c.ImageService().Delete(ctx2, dstRef, images.SynchronousDelete())
	})

	opts := []converter.Opt{
		converter.WithUpdateManifest(erofs.MergeManifestFunc(erofs.WithBlobCompression("zstd"))),
		converter.WithPlatform(platforms.DefaultStrict()),
	}
	dstImg, err := converter.Convert(ctx, c, dstRef, erofsTestImage, opts...)
	require.NoError(t, err)

	mfst, err := images.Manifest(ctx, c.ContentStore(), dstImg.Target, erofsPM())
	require.NoError(t, err)

	require.Len(t, mfst.Layers, 1, "merged EROFS must have exactly one layer")
	assert.Equal(t, contentindex.MediaTypeEROFSZstd, mfst.Layers[0].MediaType)
	_, ok := mfst.Layers[0].Annotations[contentindex.AnnotationUncompressedDigest]
	assert.True(t, ok)
}

// ---------------------------------------------------------------------------
// TestErofsOCIArchiveRoundtrip exports a locally converted EROFS image to an
// OCI tar archive, imports it back, and verifies that the EROFS layer media
// type and the os.features=["erofs"] config survive the round-trip.
// ---------------------------------------------------------------------------
func TestErofsOCIArchiveRoundtrip(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	ctx, cancel := testContext(t)
	defer cancel()

	c := newTestClient(t)
	defer c.Close()
	fetchSeed(t, c)

	srcRef := erofsTestImage + "-erofs-rt-src"
	t.Cleanup(func() {
		ctx2, c2 := testContext(t)
		defer c2()
		_ = c.ImageService().Delete(ctx2, srcRef, images.SynchronousDelete())
	})

	// Convert to merged EROFS.
	_, err := converter.Convert(ctx, c, srcRef, erofsTestImage, []converter.Opt{
		converter.WithUpdateManifest(erofs.MergeManifestFunc(erofs.WithBlobCompression("zstd"))),
		converter.WithPlatform(platforms.DefaultStrict()),
	}...)
	require.NoError(t, err)

	pm := erofsPM()

	// Export to an in-memory buffer.
	var buf bytes.Buffer
	err = c.Export(ctx, &buf,
		archive.WithImage(c.ImageService(), srcRef),
		archive.WithPlatform(pm),
		archive.WithSkipDockerManifest(),
	)
	require.NoError(t, err)
	require.Greater(t, buf.Len(), 1024, "OCI archive must be non-trivial")

	// Delete the source so the import is truly a fresh load.
	require.NoError(t, c.ImageService().Delete(ctx, srcRef, images.SynchronousDelete()))

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

	cs := c.ContentStore()
	mfst, err := images.Manifest(ctx, cs, importedImgs[0].Target, pm)
	require.NoError(t, err)

	require.Len(t, mfst.Layers, 1)
	assert.Equal(t, contentindex.MediaTypeEROFSZstd, mfst.Layers[0].MediaType,
		"EROFS+zstd media type must survive OCI archive round-trip")
	_, hasUncomp := mfst.Layers[0].Annotations[contentindex.AnnotationUncompressedDigest]
	assert.True(t, hasUncomp, "uncompressed-digest annotation must survive round-trip")

	cfgPlat, err := images.ConfigPlatform(ctx, cs, mfst.Config)
	require.NoError(t, err)
	var hasEROFSFeature bool
	for _, f := range cfgPlat.OSFeatures {
		if f == "erofs" {
			hasEROFSFeature = true
			break
		}
	}
	assert.True(t, hasEROFSFeature, "os.features=[erofs] must survive round-trip")
}

// ---------------------------------------------------------------------------
// TestErofsDualFormatIndexOrdering verifies spec §3.1: original tar manifests
// appear first in the index, EROFS variants are appended after them.
// ---------------------------------------------------------------------------
func TestErofsDualFormatIndexOrdering(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	ctx, cancel := testContext(t)
	defer cancel()

	c := newTestClient(t)
	defer c.Close()
	fetchSeed(t, c)

	dstRef := erofsTestImage + "-erofs-ordering-test"
	t.Cleanup(func() {
		ctx2, c2 := testContext(t)
		defer c2()
		_ = c.ImageService().Delete(ctx2, dstRef, images.SynchronousDelete())
	})

	opts := []converter.Opt{
		converter.WithLayerConvertFunc(erofs.LayerConvertFunc(erofs.WithBlobCompression("zstd"))),
		converter.WithUpdateManifest(erofs.UpdateManifestPlatform),
		converter.WithPlatform(platforms.DefaultStrict()),
		converter.WithAppendToIndex(),
	}
	dstImg, err := converter.Convert(ctx, c, dstRef, erofsTestImage, opts...)
	require.NoError(t, err)

	// Read the raw index to inspect manifest order.
	ra, err := c.ContentStore().ReaderAt(ctx, dstImg.Target)
	require.NoError(t, err)
	defer ra.Close()
	data, err := io.ReadAll(content.NewReader(ra))
	require.NoError(t, err)

	var idx ocispec.Index
	require.NoError(t, json.Unmarshal(data, &idx))
	require.True(t, len(idx.Manifests) >= 2,
		"dual-format index must have at least two manifests")

	// The first half should be plain tar; the second half EROFS.
	mid := len(idx.Manifests) / 2
	for i, m := range idx.Manifests {
		isEROFS := false
		if m.Platform != nil {
			for _, f := range m.Platform.OSFeatures {
				if f == "erofs" {
					isEROFS = true
				}
			}
		}
		if i < mid {
			assert.False(t, isEROFS,
				"manifest %d (first half) must be a plain tar manifest", i)
		} else {
			assert.True(t, isEROFS,
				"manifest %d (second half) must be an EROFS manifest", i)
		}
	}
}

// ---------------------------------------------------------------------------
// TestErofsConvertLayerAnnotations verifies that per-layer EROFS descriptors
// carry the mandatory uncompressed-digest annotation and, when chunked, the
// full chunk-index annotation set.
// ---------------------------------------------------------------------------
func TestErofsConvertLayerAnnotations(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	ctx, cancel := testContext(t)
	defer cancel()

	c := newTestClient(t)
	defer c.Close()
	fetchSeed(t, c)

	dstRef := erofsTestImage + "-erofs-annot-test"
	t.Cleanup(func() {
		ctx2, c2 := testContext(t)
		defer c2()
		_ = c.ImageService().Delete(ctx2, dstRef, images.SynchronousDelete())
	})

	opts := []converter.Opt{
		converter.WithLayerConvertFunc(erofs.LayerConvertFunc(erofs.WithBlobCompression("zstd"))),
		converter.WithUpdateManifest(erofs.UpdateManifestPlatform),
		converter.WithPlatform(platforms.DefaultStrict()),
		converter.WithAppendToIndex(),
	}
	dstImg, err := converter.Convert(ctx, c, dstRef, erofsTestImage, opts...)
	require.NoError(t, err)

	mfst, err := images.Manifest(ctx, c.ContentStore(), dstImg.Target, erofsPM())
	require.NoError(t, err)

	for i, l := range mfst.Layers {
		require.Equal(t, contentindex.MediaTypeEROFSZstd, l.MediaType,
			"layer %d must be EROFS+zstd", i)

		uncompDigest, ok := l.Annotations[contentindex.AnnotationUncompressedDigest]
		assert.True(t, ok, "layer %d: org.erofs.uncompressed-digest must be present", i)
		assert.NotEmpty(t, uncompDigest, "layer %d: uncompressed-digest must be non-empty", i)

		if _, hasRange := l.Annotations[contentindex.AnnotationChunkIndexRange]; hasRange {
			assert.Contains(t, l.Annotations, contentindex.AnnotationChunkIndexDigest,
				"layer %d: chunk-index.digest must accompany chunk-index.range", i)
			assert.Contains(t, l.Annotations, contentindex.AnnotationChunkIndexMediaType,
				"layer %d: chunk-index.mediaType must accompany chunk-index.range", i)
		}
	}
}

// ---------------------------------------------------------------------------
// TestErofsConvertDmVerityAnnotations verifies that layers converted with
// WithDmVerity() carry the dm-verity root digest and hash-offset annotations.
// ---------------------------------------------------------------------------
func TestErofsConvertDmVerityAnnotations(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	ctx, cancel := testContext(t)
	defer cancel()

	c := newTestClient(t)
	defer c.Close()
	fetchSeed(t, c)

	dstRef := erofsTestImage + "-erofs-dmverity-annot"
	t.Cleanup(func() {
		ctx2, c2 := testContext(t)
		defer c2()
		_ = c.ImageService().Delete(ctx2, dstRef, images.SynchronousDelete())
	})

	opts := []converter.Opt{
		converter.WithLayerConvertFunc(erofs.LayerConvertFunc(
			erofs.WithBlobCompression("zstd"),
			erofs.WithDmVerity(),
		)),
		converter.WithUpdateManifest(erofs.UpdateManifestPlatform),
		converter.WithPlatform(platforms.DefaultStrict()),
		converter.WithAppendToIndex(),
	}
	dstImg, err := converter.Convert(ctx, c, dstRef, erofsTestImage, opts...)
	require.NoError(t, err)

	mfst, err := images.Manifest(ctx, c.ContentStore(), dstImg.Target, erofsPM())
	require.NoError(t, err)

	for i, l := range mfst.Layers {
		require.Equal(t, contentindex.MediaTypeEROFSZstd, l.MediaType)
		_, hasRoot := l.Annotations[contentindex.AnnotationDmVerityRootDigest]
		_, hasOff := l.Annotations[contentindex.AnnotationDmVerityHashOffset]
		assert.True(t, hasRoot,
			"layer %d must carry org.erofs.dmverity.root_digest", i)
		assert.True(t, hasOff,
			"layer %d must carry org.erofs.dmverity.hash_offset", i)
	}
}

// ---------------------------------------------------------------------------
// TestErofsConvertOSGuardSkipsWindows verifies that Windows manifests in a
// multi-platform image do NOT receive EROFS conversions.
// ---------------------------------------------------------------------------
func TestErofsConvertOSGuardSkipsWindows(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	ctx, cancel := testContext(t)
	defer cancel()

	c := newTestClient(t)
	defer c.Close()

	// python:latest has linux/* + windows/amd64.
	const multiImg = "docker.io/library/python:latest"
	_, err := c.Fetch(ctx, multiImg,
		containerd.WithPlatform(platforms.DefaultString()))
	if err != nil {
		if isNetworkError(err) {
			t.Skipf("multi-platform image not reachable: %v", err)
		}
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		ctx2, c2 := testContext(t)
		defer c2()
		_ = c.ImageService().Delete(ctx2, multiImg, images.SynchronousDelete())
	})

	dstRef := multiImg + "-erofs-os-guard-test"
	t.Cleanup(func() {
		ctx2, c2 := testContext(t)
		defer c2()
		_ = c.ImageService().Delete(ctx2, dstRef, images.SynchronousDelete())
	})

	opts := []converter.Opt{
		converter.WithLayerConvertFunc(erofs.LayerConvertFunc(erofs.WithBlobCompression("zstd"))),
		converter.WithUpdateManifest(erofs.UpdateManifestPlatform),
		converter.WithAppendToIndex(),
	}
	dstImg, err := converter.Convert(ctx, c, dstRef, multiImg, opts...)
	require.NoError(t, err)

	plats, err := images.Platforms(ctx, c.ContentStore(), dstImg.Target)
	require.NoError(t, err)

	for _, p := range plats {
		if p.OS == "windows" {
			for _, f := range p.OSFeatures {
				assert.NotEqual(t, "erofs", f,
					"windows manifest must NOT receive an EROFS conversion")
			}
		}
	}
}

// ---------------------------------------------------------------------------
// TestErofsRegistryImagePull exercises pulling a pre-converted EROFS image
// from the registry configured via -image-list (defaults to dmcgowan/).
// ---------------------------------------------------------------------------
func TestErofsRegistryImagePull(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	imageName := imagelist.Get(imagelist.ErofsAlpine)
	require.NotEmpty(t, imageName)

	ctx, cancel := testContext(t)
	defer cancel()

	c := newTestClient(t)
	defer c.Close()
	_ = c.ImageService().Delete(ctx, imageName, images.SynchronousDelete())

	img, err := c.Pull(ctx, imageName, containerd.WithAllMetadata())
	if err != nil {
		if errdefs.IsNotFound(err) || isNetworkError(err) {
			t.Skipf("EROFS image %q not reachable: %v", imageName, err)
		}
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		ctx2, c2 := testContext(t)
		defer c2()
		_ = c.ImageService().Delete(ctx2, imageName, images.SynchronousDelete())
	})

	plats, err := images.Platforms(ctx, c.ContentStore(), img.Target())
	require.NoError(t, err)

	var erofsCount int
	for _, p := range plats {
		for _, f := range p.OSFeatures {
			if f == "erofs" {
				erofsCount++
			}
		}
	}
	assert.Greater(t, erofsCount, 0,
		"at least one EROFS manifest must be present in %q", imageName)
}
