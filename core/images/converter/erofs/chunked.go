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

package erofs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"

	goerofs "github.com/erofs/go-erofs"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/content/index/chunked"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/converter"
	"github.com/containerd/containerd/v2/core/images/converter/uncompress"
	"github.com/containerd/containerd/v2/internal/erofsutils"
	"github.com/containerd/continuity/tarconv"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// LayerConvertFuncChunked returns a converter.ConvertFunc that:
//   - Converts a tar or tar+gzip layer to an EROFS image (via mkfs.erofs).
//   - Splits the EROFS image into zstd-compressed chunks.
//   - Appends a binary chunk index (erofs-image-spec §3.4) as a zstd skippable
//     frame to produce a application/vnd.erofs+zstd blob.
//   - Stores the whole blob in the regular content store so it can be exported
//     or pushed via the standard OCI pusher.
//   - When idxStore is non-nil, also ingests the blob into the indexed content
//     store so per-chunk addressing and lazy loading are available.
//
// Passing idxStore = nil is valid and skips the indexed-store step.  This is
// the correct choice for the ctr image convert workflow where the goal is
// producing a redistributable OCI blob, not enabling on-host chunk addressing.
//
// targetFrameSize controls the target compressed output frame size. Pass 0
// to use the default (chunked.TargetFrameSize = 4 MiB).
func LayerConvertFuncChunked(idxStore contentindex.Store, targetFrameSize int, opts ...ConvertOpt) converter.ConvertFunc {
	if targetFrameSize <= 0 {
		targetFrameSize = chunked.TargetFrameSize
	}
	return func(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
		var copts convertOptions
		for _, opt := range opts {
			opt(&copts)
		}

		// Skip non-layer and already-EROFS blobs.
		if !images.IsLayerType(desc.MediaType) || erofsutils.IsErofsMediaType(desc.MediaType) {
			return nil, nil
		}
		if images.IsNonDistributable(desc.MediaType) {
			return nil, nil
		}

		// ── Step 1: Decompress if necessary ──────────────────────────────
		uncompressedDesc := &desc
		if !uncompress.IsUncompressedType(desc.MediaType) {
			var err error
			uncompressedDesc, err = uncompress.LayerConvertFunc(ctx, cs, desc)
			if err != nil {
				return nil, fmt.Errorf("chunked converter: decompress: %w", err)
			}
			if uncompressedDesc == nil {
				return nil, fmt.Errorf("chunked converter: unexpectedly got same blob after decompression (%s)", desc.Digest)
			}
		}

		// ── Step 2: Convert the uncompressed tar to EROFS via tarconv ────
		ra, err := cs.ReaderAt(ctx, *uncompressedDesc)
		if err != nil {
			return nil, fmt.Errorf("chunked converter: open uncompressed reader: %w", err)
		}
		defer ra.Close()

		erofsFile, err := os.CreateTemp("", "chunked-erofs-*.erofs")
		if err != nil {
			return nil, fmt.Errorf("chunked converter: create temp file: %w", err)
		}
		defer os.Remove(erofsFile.Name())

		w := goerofs.Create(erofsFile)
		if err := tarconv.Apply(w, newSectionReadReader(ra, ra.Size())); err != nil {
			erofsFile.Close()
			return nil, fmt.Errorf("chunked converter: tarconv: %w", err)
		}
		if err := w.Close(); err != nil {
			erofsFile.Close()
			return nil, fmt.Errorf("chunked converter: finalize EROFS: %w", err)
		}
		if _, err := erofsFile.Seek(0, io.SeekStart); err != nil {
			erofsFile.Close()
			return nil, fmt.Errorf("chunked converter: seek: %w", err)
		}

		// ── Step 3: Read EROFS bytes and build the chunked blob ───────────
		erofsData, err := io.ReadAll(erofsFile)
		erofsFile.Close()
		if err != nil {
			return nil, fmt.Errorf("chunked converter: read erofs file: %w", err)
		}

		result, err := chunked.Build(
			bytes.NewReader(erofsData),
			int64(len(erofsData)),
			contentindex.MediaTypeEROFSZstd,
			targetFrameSize,
		)
		if err != nil {
			return nil, fmt.Errorf("chunked converter: build chunked blob: %w", err)
		}

		// ── Step 4: Optionally ingest into the indexed content store ──────
		// When idxStore is non-nil, the blob is split into chunks and ingested
		// for per-chunk addressing and lazy loading.  When idxStore is nil
		// (e.g. ctr image convert without a running indexed store) this step
		// is skipped; the whole blob is still stored in the regular content
		// store below so it remains exportable and pushable.
		if idxStore != nil {
			w, err := idxStore.Writer(ctx,
				content.WithRef("convert-chunked-"+desc.Digest.String()),
				content.WithDescriptor(result.Descriptor),
			)
			if err != nil && !errdefs.IsAlreadyExists(err) {
				return nil, fmt.Errorf("chunked converter: open indexed writer: %w", err)
			}
			if err == nil {
				if _, werr := w.Write(result.Blob); werr != nil {
					w.Close()
					return nil, fmt.Errorf("chunked converter: write to indexed store: %w", werr)
				}
				if cerr := w.Commit(ctx, int64(len(result.Blob)), result.Descriptor.Digest); cerr != nil && !errdefs.IsAlreadyExists(cerr) {
					return nil, fmt.Errorf("chunked converter: commit to indexed store: %w", cerr)
				}
			}
		}

		// ── Step 5: Store the whole blob in the regular content store ─────
		// This entry is what the OCI pusher and image exporter read.  Whether
		// the indexed store was used above is irrelevant to this step.
		if _, cerr := cs.Info(ctx, result.Descriptor.Digest); cerr != nil {
			if !errdefs.IsNotFound(cerr) {
				return nil, fmt.Errorf("chunked converter: check content store: %w", cerr)
			}
			cw, err := cs.Writer(ctx,
				content.WithRef("convert-chunked-blob-"+desc.Digest.String()),
				content.WithDescriptor(result.Descriptor),
			)
			if err != nil && !errdefs.IsAlreadyExists(err) {
				return nil, fmt.Errorf("chunked converter: open content writer: %w", err)
			}
			if err == nil {
				if _, werr := cw.Write(result.Blob); werr != nil {
					cw.Close()
					return nil, fmt.Errorf("chunked converter: write blob to content store: %w", werr)
				}
				if cerr := cw.Commit(ctx, int64(len(result.Blob)), result.Descriptor.Digest); cerr != nil && !errdefs.IsAlreadyExists(cerr) {
					return nil, fmt.Errorf("chunked converter: commit blob to content store: %w", cerr)
				}
			}
		}

		// Annotate the descriptor with the DiffID of the layer —
		// the digest of the raw EROFS image bytes (erofsData), which is
		// what the +zstd layer decompresses to.  This allows consumers to
		// derive ChainIDs without rootfs.diff_ids in the image config.
		diffID := digest.FromBytes(erofsData)
		newDesc := result.Descriptor
		if newDesc.Annotations == nil {
			newDesc.Annotations = make(map[string]string)
		}
		newDesc.Annotations[contentindex.AnnotationUncompressedDigest] = diffID.String()
		return &newDesc, nil
	}
}

// newSectionReadReader reads all bytes from ra and returns a *bytes.Reader.
// Used to feed the EROFS image bytes into mkfs.erofs helpers that need an
// io.Reader.
func newSectionReadReader(ra content.ReaderAt, size int64) io.Reader {
	buf := make([]byte, size)
	ra.ReadAt(buf, 0)
	return bytes.NewReader(buf)
}
