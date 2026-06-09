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

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/converter"
	"github.com/containerd/containerd/v2/core/images/converter/uncompress"
	"github.com/containerd/containerd/v2/internal/erofsutils"
	"github.com/containerd/continuity/tarconv"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// LayerConvertFuncSplitData returns a MultiLayerConvertFunc that converts each
// source layer into a pair of regular EROFS layers, per the producer strategy
// described in erofs-image-spec §10.2:
//
//	[0] data-providing layer (application/vnd.erofs[+zstd])
//	    — a regular EROFS image holding file payloads as inlined inodes,
//	    laid out for cross-image deduplication.
//	[1] metadata layer (application/vnd.erofs[+zstd])
//	    — a regular EROFS image whose inodes for files placed in the
//	    data-providing layer reference its blocks via multi-device addressing.
//
// Both outputs use the same regular EROFS media type; no new media type is
// introduced.  The split is a producer strategy, not a format extension.
//
// inlineThreshold (in bytes) controls which files stay inline in the metadata
// layer vs. land in the data-providing layer.  Pass 0 to place all regular
// files in the data-providing layer.
func LayerConvertFuncSplitData(inlineThreshold int, opts ...ConvertOpt) converter.MultiLayerConvertFunc {
	return func(ctx context.Context, cs content.Store, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		var copts convertOptions
		for _, opt := range opts {
			opt(&copts)
		}

		if !images.IsLayerType(desc.MediaType) || erofsutils.IsErofsMediaType(desc.MediaType) {
			return nil, nil
		}
		if images.IsNonDistributable(desc.MediaType) {
			return nil, nil
		}

		// ── Step 1: Decompress the source layer ──────────────────────────
		uncompDesc := &desc
		if !uncompress.IsUncompressedType(desc.MediaType) {
			var err error
			uncompDesc, err = uncompress.LayerConvertFunc(ctx, cs, desc)
			if err != nil {
				return nil, fmt.Errorf("splitdata: decompress: %w", err)
			}
			if uncompDesc == nil {
				return nil, fmt.Errorf("splitdata: unexpectedly same blob after decompress (%s)", desc.Digest)
			}
		}

		ra, err := cs.ReaderAt(ctx, *uncompDesc)
		if err != nil {
			return nil, fmt.Errorf("splitdata: open uncompressed reader: %w", err)
		}
		defer ra.Close()

		// ── Step 2: Build the data-providing EROFS layer via tarconv ─────
		// tarconv.Apply converts the tar stream directly into a go-erofs Writer,
		// producing a complete EROFS image that is independently mountable.
		dataOut, err := os.CreateTemp("", "splitdata-data-*.erofs")
		if err != nil {
			return nil, fmt.Errorf("splitdata: create data temp file: %w", err)
		}
		defer os.Remove(dataOut.Name())
		defer dataOut.Close()

		dw := goerofs.Create(dataOut)
		if err := tarconv.Apply(dw, content.NewReader(ra)); err != nil {
			return nil, fmt.Errorf("splitdata: build data-providing layer: %w", err)
		}
		if err := dw.Close(); err != nil {
			return nil, fmt.Errorf("splitdata: close data writer: %w", err)
		}

		// ── Step 4: Build the metadata EROFS layer ────────────────────────
		// The metadata layer references the data-providing layer as device 1
		// via MetadataOnly() + WithDataFile().  Files that fit inline (per the
		// existing go-erofs heuristics) stay in the metadata image; others get
		// chunk-index extents pointing at the data-providing layer's blocks.
		metaOut, err := os.CreateTemp("", "splitdata-meta-*.erofs")
		if err != nil {
			return nil, fmt.Errorf("splitdata: create meta temp file: %w", err)
		}
		defer os.Remove(metaOut.Name())
		defer metaOut.Close()

		// Reopen the data layer as an EROFS image source for the metadata writer.
		dataIn, err := os.Open(dataOut.Name())
		if err != nil {
			return nil, fmt.Errorf("splitdata: reopen data layer: %w", err)
		}
		defer dataIn.Close()

		dataImg, err := goerofs.Open(dataIn)
		if err != nil {
			return nil, fmt.Errorf("splitdata: open data layer as EROFS: %w", err)
		}

		mw := goerofs.Create(metaOut)
		if err := mw.CopyFrom(dataImg, goerofs.MetadataOnly()); err != nil {
			return nil, fmt.Errorf("splitdata: build metadata layer: %w", err)
		}
		if err := mw.Close(); err != nil {
			return nil, fmt.Errorf("splitdata: close meta writer: %w", err)
		}

		// ── Step 5: Read both blobs and ingest into the content store ────
		if _, err := dataOut.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		dataBytes, err := io.ReadAll(dataOut)
		if err != nil {
			return nil, fmt.Errorf("splitdata: read data layer: %w", err)
		}
		if _, err := metaOut.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		metaBytes, err := io.ReadAll(metaOut)
		if err != nil {
			return nil, fmt.Errorf("splitdata: read meta layer: %w", err)
		}

		// Both outputs use the canonical EROFS media type, per spec §9.1.
		mediaType := images.MediaTypeErofs // application/vnd.erofs

		dataDesc, err := ingestSplitBlob(ctx, cs, dataBytes, mediaType,
			"splitdata-data-"+desc.Digest.String())
		if err != nil {
			return nil, fmt.Errorf("splitdata: ingest data-providing layer: %w", err)
		}
		metaDesc, err := ingestSplitBlob(ctx, cs, metaBytes, mediaType,
			"splitdata-meta-"+desc.Digest.String())
		if err != nil {
			return nil, fmt.Errorf("splitdata: ingest metadata layer: %w", err)
		}

		// Return [data-providing, metadata] in left-to-right layers[] order.
		// The metadata layer references the data-providing layer as device 1
		// via its multi-device chunk-index; the snapshotter at apply time
		// places each layer's layer.erofs file in its own snapshot dir, and
		// the existing mountFsMeta path adds device= options in chain order.
		return []ocispec.Descriptor{dataDesc, metaDesc}, nil
	}
}

// ingestSplitBlob writes data into the content store under its sha256 digest.
// Idempotent: a no-op when the blob is already present.
func ingestSplitBlob(ctx context.Context, cs content.Store, data []byte, mediaType, ref string) (ocispec.Descriptor, error) {
	dgst := digest.FromBytes(data)
	desc := ocispec.Descriptor{
		MediaType: mediaType,
		Digest:    dgst,
		Size:      int64(len(data)),
	}
	if _, err := cs.Info(ctx, dgst); err == nil {
		return desc, nil
	}
	cw, err := cs.Writer(ctx, content.WithRef(ref), content.WithDescriptor(desc))
	if err != nil {
		if errdefs.IsAlreadyExists(err) {
			return desc, nil
		}
		return ocispec.Descriptor{}, err
	}
	if _, err := io.Copy(cw, bytes.NewReader(data)); err != nil {
		cw.Close()
		return ocispec.Descriptor{}, err
	}
	if err := cw.Commit(ctx, int64(len(data)), dgst); err != nil && !errdefs.IsAlreadyExists(err) {
		return ocispec.Descriptor{}, err
	}
	return desc, nil
}
