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

// suite_test.go contains platform-independent tests for the EROFS integration
// suite.  Tests here compile and run on Linux, macOS, and Windows.  They
// depend only on packages without Linux-only transitive dependencies:
//   - contentindex (media-type constants)
//   - images      (IsLayerType, Manifest, Platforms, ConfigPlatform)
//   - integration/images (image-list constants)
package erofs

import (
	"testing"

	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/core/images"
	imagelist "github.com/containerd/containerd/v2/integration/images"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// TestErofsMediaTypes verifies that all EROFS media type constants are
// recognized by images.IsLayerType and follow the "application/vnd.erofs"
// naming scheme.
// ---------------------------------------------------------------------------
func TestErofsMediaTypes(t *testing.T) {
	canonical := []string{
		contentindex.MediaTypeEROFS,
		contentindex.MediaTypeEROFSZstd,
	}
	legacy := []string{
		contentindex.MediaTypeEROFSLayer,
		contentindex.MediaTypeEROFSLayerZstd,
	}
	for _, mt := range append(canonical, legacy...) {
		mt := mt
		t.Run(mt, func(t *testing.T) {
			assert.True(t, images.IsLayerType(mt),
				"images.IsLayerType must recognise EROFS media type %q", mt)
			assert.True(t, isErofsMediaTypePrefix(mt),
				"EROFS media type %q must start with 'application/vnd.erofs'", mt)
		})
	}

	// Chunk-index type is NOT a layer.
	assert.False(t, images.IsLayerType(contentindex.ChunkIndexMediaTypeEROFSV1),
		"chunk-index media type must not be a layer type")

	// OCI tar types must NOT match.
	for _, mt := range []string{
		ocispec.MediaTypeImageLayerGzip,
		ocispec.MediaTypeImageLayer,
		"application/vnd.docker.image.rootfs.diff.tar.gzip",
	} {
		assert.False(t, isErofsMediaTypePrefix(mt),
			"tar media type %q must not be recognised as EROFS", mt)
	}
}

// ---------------------------------------------------------------------------
// TestErofsMediaTypeConstants verifies the canonical EROFS media type strings
// against the values mandated by erofs-image-spec.
// ---------------------------------------------------------------------------
func TestErofsMediaTypeConstants(t *testing.T) {
	cases := []struct{ name, got, want string }{
		{"canonical raw", contentindex.MediaTypeEROFS, "application/vnd.erofs"},
		{"canonical zstd", contentindex.MediaTypeEROFSZstd, "application/vnd.erofs+zstd"},
		{"legacy layer", contentindex.MediaTypeEROFSLayer, "application/vnd.erofs.layer.v1"},
		{"legacy layer+zstd", contentindex.MediaTypeEROFSLayerZstd, "application/vnd.erofs.layer.v1+zstd"},
		{"chunk-index", contentindex.ChunkIndexMediaTypeEROFSV1, "application/vnd.erofs.chunk-index.v1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, c.got)
		})
	}
}

// ---------------------------------------------------------------------------
// TestErofsAnnotationConstants verifies that all erofs-image-spec annotation
// keys follow the "org.erofs.*" naming scheme.
// ---------------------------------------------------------------------------
func TestErofsAnnotationConstants(t *testing.T) {
	const prefix = "org.erofs."
	for name, value := range map[string]string{
		"AnnotationUncompressedDigest":  contentindex.AnnotationUncompressedDigest,
		"AnnotationDmVerityRootDigest":  contentindex.AnnotationDmVerityRootDigest,
		"AnnotationDmVerityHashOffset":  contentindex.AnnotationDmVerityHashOffset,
		"AnnotationDmVerityBlockSize":   contentindex.AnnotationDmVerityBlockSize,
		"AnnotationChunkIndexRange":     contentindex.AnnotationChunkIndexRange,
		"AnnotationChunkIndexDigest":    contentindex.AnnotationChunkIndexDigest,
		"AnnotationChunkIndexMediaType": contentindex.AnnotationChunkIndexMediaType,
		"AnnotationRole":                contentindex.AnnotationRole,
	} {
		name, value := name, value
		t.Run(name, func(t *testing.T) {
			require.True(t,
				len(value) > len(prefix) && value[:len(prefix)] == prefix,
				"annotation %s = %q must start with %q", name, value, prefix)
		})
	}
}

// ---------------------------------------------------------------------------
// TestErofsImageListConfigured verifies that the image list has EROFS entries
// configured (either defaults or overridden via -image-list).
// ---------------------------------------------------------------------------
func TestErofsImageListConfigured(t *testing.T) {
	entries := map[string]int{
		"ErofsAlpine":      imagelist.ErofsAlpine,
		"ErofsBusyBox":     imagelist.ErofsBusyBox,
		"ErofsPause":       imagelist.ErofsPause,
		"ErofsAlpineMerge": imagelist.ErofsAlpineMerge,
	}
	for name, key := range entries {
		ref := imagelist.Get(key)
		require.NotEmpty(t, ref,
			"image list entry %s must have a non-empty reference", name)
		t.Logf("%-20s = %s", name, ref)
	}
}

// ---------------------------------------------------------------------------
// TestErofsRoleAnnotationValues verifies the defined role constant values.
// ---------------------------------------------------------------------------
func TestErofsRoleAnnotationValues(t *testing.T) {
	assert.Equal(t, "device", contentindex.RoleDevice)
	assert.Equal(t, "overlay-lower", contentindex.RoleOverlayLower)
	assert.Equal(t, "overlay-data", contentindex.RoleOverlayData)
}
