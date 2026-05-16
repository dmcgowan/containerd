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

	"github.com/containerd/containerd/v2/core/content"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/converter"
	"github.com/containerd/containerd/v2/core/images/converter/uncompress"
	"github.com/containerd/containerd/v2/core/content/index/chunked"
	"github.com/containerd/containerd/v2/internal/erofsutils"
	"github.com/containerd/errdefs"
	"github.com/google/uuid"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"os"
)

// LayerConvertFuncChunked returns a converter.ConvertFunc that:
//   - Converts a tar or tar+gzip layer to an EROFS image (via mkfs.erofs).
//   - Splits the EROFS image into zstd-compressed chunks.
//   - Appends a binary chunk index (erofs-image-spec §3.4) as a zstd skippable
//     frame to produce a application/vnd.erofs.layer.v1+zstd blob.
//   - Ingests the result into idxStore (chunk-index entry and per-chunk entries
//     in the underlying content store).
//
// The returned descriptor has MediaType = MediaTypeEROFSLayerZstd and carries
// org.erofs.index.* annotations.  The descriptor can be stored in an OCI
// manifest alongside image layers.
//
// chunkSize controls the uncompressed chunk size.  Pass 0 to use the default
// (chunked.DefaultChunkSize = 4 MiB).
func LayerConvertFuncChunked(idxStore contentindex.Store, chunkSize int, opts ...ConvertOpt) converter.ConvertFunc {
	if chunkSize <= 0 {
		chunkSize = chunked.DefaultChunkSize
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

		// ── Step 2: Convert the uncompressed tar to EROFS ────────────────
		ra, err := cs.ReaderAt(ctx, *uncompressedDesc)
		if err != nil {
			return nil, fmt.Errorf("chunked converter: open uncompressed reader: %w", err)
		}
		defer ra.Close()

		blobFile, err := os.CreateTemp("", "chunked-erofs-*.erofs")
		if err != nil {
			return nil, fmt.Errorf("chunked converter: create temp file: %w", err)
		}
		blobPath := blobFile.Name()
		blobFile.Close()
		defer os.Remove(blobPath)

		var mkfsArgs []string
		if copts.compressors != "" {
			mkfsArgs = append(mkfsArgs, "-z"+copts.compressors)
			mkfsArgs = append(mkfsArgs, "-C", "65536")
		}
		mkfsArgs = append(mkfsArgs, copts.mkfsExtraOpts...)
		mkfsArgs = erofsutils.AddDefaultMkfsOpts(mkfsArgs)

		u := uuid.NewSHA1(uuid.NameSpaceURL, []byte("erofs:blobs/"+desc.Digest))
		if err := erofsutils.ConvertTarErofs(ctx, newSectionReadReader(ra, ra.Size()), blobPath, u.String(), mkfsArgs); err != nil {
			return nil, fmt.Errorf("chunked converter: mkfs.erofs: %w", err)
		}

		// ── Step 3: Open the EROFS image and build the chunked blob ──────
		erofsData, err := os.ReadFile(blobPath)
		if err != nil {
			return nil, fmt.Errorf("chunked converter: read erofs file: %w", err)
		}

		result, err := chunked.Build(
			bytes.NewReader(erofsData),
			int64(len(erofsData)),
			contentindex.MediaTypeEROFSLayerZstd,
			chunkSize,
		)
		if err != nil {
			return nil, fmt.Errorf("chunked converter: build chunked blob: %w", err)
		}

		// ── Step 4: Ingest into the indexed content store ─────────────────
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

		// ── Step 5: Also store the whole blob in the regular content store
		// so that it can be pushed to a registry via the normal pusher path.
		// The descriptor returned to the converter framework points at this
		// content-store entry.
		if _, err := cs.Info(ctx, result.Descriptor.Digest); err != nil {
			if !errdefs.IsNotFound(err) {
				return nil, fmt.Errorf("chunked converter: check content store: %w", err)
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

		newDesc := result.Descriptor
		return &newDesc, nil
	}
}

// sectionReadReader adapts a content.ReaderAt to an io.Reader via a
// io.SectionReader, so the existing erofsutils helpers can consume it.
type sectionReadReader struct {
	*bytes.Reader
}

func newSectionReadReader(ra content.ReaderAt, size int64) *bytes.Reader {
	// Read all bytes to memory.  For large images this is not ideal, but for
	// the converter path (which already wrote the file to disk) it's
	// equivalent to re-reading from the temp file.
	buf := make([]byte, size)
	ra.ReadAt(buf, 0)
	return bytes.NewReader(buf)
}
