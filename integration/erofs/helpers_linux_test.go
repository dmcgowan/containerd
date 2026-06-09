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

// helpers_linux_test.go provides test helpers that depend on Linux-only
// packages (converter/erofs → erofsutils/dmverity → go-dmverity/keyring).
package erofs

import (
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/converter"
	erofsconv "github.com/containerd/containerd/v2/core/images/converter/erofs"
	"github.com/containerd/platforms"
	"github.com/stretchr/testify/require"
)

// erofsTestImage is the small tar image used as the seed for local conversions.
const erofsTestImage = "ghcr.io/containerd/alpine:3.14.0"

// localEROFS converts erofsTestImage to a single merged EROFS layer under
// dstRef using a local converter.Convert call (no registry push required).
// It registers t.Cleanup to delete both the seed and the converted image.
func localEROFS(t *testing.T, c *containerd.Client, dstRef string) *images.Image {
	t.Helper()
	ctx, cancel := testContext(t)
	defer cancel()

	_, err := c.Fetch(ctx, erofsTestImage,
		containerd.WithPlatform(platforms.DefaultString()))
	require.NoError(t, err, "fetch seed image for local conversion")
	t.Cleanup(func() {
		ctx2, c2 := testContext(t)
		defer c2()
		_ = c.ImageService().Delete(ctx2, erofsTestImage, images.SynchronousDelete())
	})

	opts := []converter.Opt{
		converter.WithUpdateManifest(erofsconv.MergeManifestFunc(
			erofsconv.WithBlobCompression("zstd"))),
		converter.WithPlatform(platforms.DefaultStrict()),
	}
	img, err := converter.Convert(ctx, c, dstRef, erofsTestImage, opts...)
	require.NoError(t, err, "convert to merged EROFS")
	t.Cleanup(func() {
		ctx2, c2 := testContext(t)
		defer c2()
		_ = c.ImageService().Delete(ctx2, dstRef, images.SynchronousDelete())
	})
	return img
}
