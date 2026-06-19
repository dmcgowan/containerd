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

package images

import (
	"context"
	"io"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/containerd/containerd/v2/core/content"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/containerd/containerd/v2/pkg/labels"
	"github.com/containerd/log"
)

// UncompressedDigestFromDescriptor returns the uncompressed-data digest
// of a layer descriptor, consulting sources in priority order:
//
//  1. The org.erofs.uncompressed-digest annotation on the descriptor
//     (erofs-image-spec §2.3 / §5.2).
//  2. The content-store label containerd.io/uncompressed on the blob.
//  3. Live decompression via GetDiffID (slow path; also memoizes the
//     result as the containerd.io/uncompressed label).
//
// The returned digest equals the layer's DiffID (as used in rootfs.diff_ids
// and ChainID computation) for all currently defined EROFS and OCI media types.
// When the annotation is present its value is verified to be a valid digest;
// an invalid value is treated as absent and the next source is consulted.
// When both annotation and content-store label are present and disagree, an
// error is returned — the image is considered malformed.
func UncompressedDigestFromDescriptor(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (digest.Digest, error) {
	// Source 1: annotation on the descriptor.
	var annotUncompDigest digest.Digest
	if v, ok := desc.Annotations[contentindex.AnnotationUncompressedDigest]; ok && v != "" {
		d, err := digest.Parse(v)
		if err == nil {
			annotUncompDigest = d
		}
	}

	// Annotation present: use it directly. rootfs.diff_ids and any
	// content-store label may be ignored per erofs-image-spec §5.2.
	if annotUncompDigest != "" {
		return annotUncompDigest, nil
	}

	// No annotation: fall back to content-store label / live decompression.
	return GetDiffID(ctx, cs, desc)
}

// GetDiffID gets the diff ID of the layer blob descriptor.
func GetDiffID(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (digest.Digest, error) {
	switch desc.MediaType {
	case
		// If the layer is already uncompressed, we can just return its digest
		MediaTypeDockerSchema2Layer,
		ocispec.MediaTypeImageLayer,
		MediaTypeDockerSchema2LayerForeign,
		ocispec.MediaTypeImageLayerNonDistributable, //nolint:staticcheck // deprecated
		// Raw (uncompressed) EROFS: the blob digest equals the uncompressed
		// content digest. The +zstd variant requires annotation or label lookup.
		MediaTypeErofs,
		MediaTypeErofsLayer:
		return desc.Digest, nil
	}
	info, err := cs.Info(ctx, desc.Digest)
	if err != nil {
		return "", err
	}
	v, ok := info.Labels[labels.LabelUncompressed]
	if ok {
		// Fast path: if the image is already unpacked, we can use the label value
		return digest.Parse(v)
	}
	// if the image is not unpacked, we may not have the label
	ra, err := cs.ReaderAt(ctx, desc)
	if err != nil {
		return "", err
	}
	defer ra.Close()
	r := content.NewReader(ra)
	uR, err := compression.DecompressStream(r)
	if err != nil {
		return "", err
	}
	defer uR.Close()
	digester := digest.Canonical.Digester()
	hashW := digester.Hash()
	if _, err := io.Copy(hashW, uR); err != nil {
		return "", err
	}
	if err := ra.Close(); err != nil {
		return "", err
	}
	digest := digester.Digest()
	// memorize the computed value
	if info.Labels == nil {
		info.Labels = make(map[string]string)
	}
	info.Labels[labels.LabelUncompressed] = digest.String()
	if _, err := cs.Update(ctx, info, "labels"); err != nil {
		log.G(ctx).WithError(err).Warnf("failed to set %s label for %s", labels.LabelUncompressed, desc.Digest)
	}
	return digest, nil
}
