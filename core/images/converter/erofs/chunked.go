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
	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/containerd/continuity/tarconv"
	"github.com/containerd/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// LayerConvertFuncChunked returns a converter.ConvertFunc that:
//   - Converts a tar or tar+gzip layer to an EROFS image using the pure-Go
//     go-erofs + continuity/tarconv stack.
//   - Splits the EROFS image into zstd-compressed chunks.
//   - Appends a binary chunk index (erofs-image-spec §3.4) as a zstd skippable
//     frame to produce an application/vnd.erofs+zstd blob.
//   - Stores the blob in the regular content store so it can be exported or
//     pushed via the standard OCI pusher.
//
// The idxStore parameter is intentionally ignored and reserved for future use;
// pass nil.  The indexed-content-store step was removed because ctr image
// convert only needs a pushable OCI blob — on-host chunk addressing is set up
// later by the pull path, not at conversion time.
//
// targetFrameSize controls the target compressed output frame size. Pass 0
// to use the default (chunked.TargetFrameSize ≈ 4.5 MiB).
//
// Memory behaviour: no full-image copy is held in RAM.  Decompression is
// streamed directly into tarconv; the EROFS image is written to a temp file;
// chunked.Build reads it in per-chunk windows and writes frames directly to
// the content-store writer.  Peak RAM per layer ≈ one uncompressed chunk
// (≈ 13–15 MiB at the default frame size) plus small metadata.
//
// Disk: no intermediate uncompressed-tar blob is committed to the content
// store.  Only the temp EROFS file (E bytes) and the final +zstd blob in the
// content store (≈ E/3) exist simultaneously.  Set TMPDIR to direct the temp
// file to a filesystem with enough headroom.
func LayerConvertFuncChunked(_ contentindex.Store, targetFrameSize int, opts ...ConvertOpt) converter.ConvertFunc {
	return func(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
		var copts convertOptions
		for _, opt := range opts {
			opt(&copts)
		}
		// Resolve target compressed-frame size with explicit arg
		// taking precedence over WithTargetFrameSize, which in turn
		// takes precedence over the chunked.TargetFrameSize default.
		effectiveFrameSize := targetFrameSize
		if effectiveFrameSize <= 0 {
			effectiveFrameSize = copts.targetFrameSize
		}
		if effectiveFrameSize <= 0 {
			effectiveFrameSize = chunked.TargetFrameSize
		}

		// Skip non-layer and already-EROFS blobs.
		if !images.IsLayerType(desc.MediaType) || erofsutils.IsErofsMediaType(desc.MediaType) {
			return nil, nil
		}
		if images.IsNonDistributable(desc.MediaType) {
			return nil, nil
		}

		// ── Step 1: Get the compressed blob reader ────────────────────────
		ra, err := cs.ReaderAt(ctx, desc)
		if err != nil {
			return nil, fmt.Errorf("chunked converter: open blob reader: %w", err)
		}
		defer ra.Close()

		// ── Step 2: Convert the tar stream to EROFS via tarconv ──────────
		// Stream decompression directly into tarconv — no intermediate
		// uncompressed blob is written to the content store.
		var tarReader io.Reader
		if uncompress.IsUncompressedType(desc.MediaType) {
			tarReader = io.NewSectionReader(ra, 0, ra.Size())
		} else {
			decomp, err := compression.DecompressStream(io.NewSectionReader(ra, 0, ra.Size()))
			if err != nil {
				return nil, fmt.Errorf("chunked converter: decompress stream: %w", err)
			}
			defer decomp.Close()
			tarReader = decomp
		}

		// Write the EROFS filesystem image to a temp file.
		// Use TMPDIR (set via environment) to direct this to a filesystem
		// with sufficient space; the file is removed on return.
		erofsFile, err := os.CreateTemp("", "chunked-erofs-*.erofs")
		if err != nil {
			return nil, fmt.Errorf("chunked converter: create temp file: %w", err)
		}
		defer os.Remove(erofsFile.Name())

		w := goerofs.Create(erofsFile)
		if err := tarconv.Apply(w, tarReader); err != nil {
			erofsFile.Close()
			return nil, fmt.Errorf("chunked converter: tarconv: %w", err)
		}
		if err := w.Close(); err != nil {
			erofsFile.Close()
			return nil, fmt.Errorf("chunked converter: finalize EROFS: %w", err)
		}

		erofsSize, err := erofsFile.Seek(0, io.SeekEnd)
		if err != nil {
			erofsFile.Close()
			return nil, fmt.Errorf("chunked converter: seek end: %w", err)
		}
		if _, err := erofsFile.Seek(0, io.SeekStart); err != nil {
			erofsFile.Close()
			return nil, fmt.Errorf("chunked converter: seek start: %w", err)
		}

		// ── Step 2b: Optionally append dm-verity merkle tree ──────────────
		// When dm-verity is requested, stream the EROFS file through a
		// VerityWriter into a second temp file. The result has layout:
		//   [erofs image][verity SB @ erofsSize][merkle tree]
		// We then force a chunk boundary at the verity hash offset so the
		// verity-tree region and the EROFS-data region never share a frame —
		// this is what lets the lazy mount path pre-fill just the tree.
		//
		// Streaming (rather than reading the whole image into RAM) keeps
		// peak memory at one block; this matches the chunked converter's
		// "no full-image copy" property.
		sourceFile := erofsFile
		sourceSize := erofsSize
		var verity *erofsutils.DmVerityResult
		var forcedBoundaries []int64
		if copts.dmVerity {
			combinedFile, vres, err := streamAppendDmVerity(ctx, erofsFile, erofsSize)
			if err != nil {
				erofsFile.Close()
				return nil, fmt.Errorf("chunked converter: append dm-verity: %w", err)
			}
			// erofsFile has been consumed by streamAppendDmVerity; the
			// returned combinedFile is positioned at end. Close erofsFile
			// (its temp path is registered for removal) and take over the
			// combinedFile lifecycle.
			erofsFile.Close()
			defer os.Remove(combinedFile.Name())
			cfSize, err := combinedFile.Seek(0, io.SeekEnd)
			if err != nil {
				combinedFile.Close()
				return nil, fmt.Errorf("chunked converter: seek combined end: %w", err)
			}
			if _, err := combinedFile.Seek(0, io.SeekStart); err != nil {
				combinedFile.Close()
				return nil, fmt.Errorf("chunked converter: seek combined start: %w", err)
			}
			sourceFile = combinedFile
			sourceSize = cfSize
			verity = vres
			forcedBoundaries = []int64{vres.HashOffset}
		}

		// ── Step 3: Stream EROFS → chunked +zstd blob into content store ─
		// chunked.Build reads the source file in per-chunk windows; no
		// full-image copy is held in RAM.  The content-store writer receives
		// compressed frames as they are produced.  DiffID (SHA-256 of the
		// raw EROFS bytes, including the verity tree when present) is
		// computed in-stream by the builder.
		cw, err := cs.Writer(ctx,
			content.WithRef("convert-chunked-blob-"+desc.Digest.String()),
		)
		if err != nil && !errdefs.IsAlreadyExists(err) {
			sourceFile.Close()
			return nil, fmt.Errorf("chunked converter: open content writer: %w", err)
		}
		if errdefs.IsAlreadyExists(err) {
			// Blob was already converted in a previous run.  Look up the
			// existing descriptor by probing the content store.
			sourceFile.Close()
			return nil, errdefs.ErrAlreadyExists
		}

		result, berr := chunked.Build(sourceFile, sourceSize, cw, contentindex.MediaTypeEROFSZstd, effectiveFrameSize, forcedBoundaries...)
		sourceFile.Close()
		if berr != nil {
			cw.Close()
			return nil, fmt.Errorf("chunked converter: build chunked blob: %w", berr)
		}

		blobDigest := cw.Digest()
		if cerr := cw.Commit(ctx, result.Written, blobDigest); cerr != nil && !errdefs.IsAlreadyExists(cerr) {
			cw.Close()
			return nil, fmt.Errorf("chunked converter: commit blob: %w", cerr)
		}
		cw.Close()

		// ── Step 4: Construct the output descriptor ───────────────────────
		newDesc := result.Descriptor
		newDesc.Digest = blobDigest
		if newDesc.Annotations == nil {
			newDesc.Annotations = make(map[string]string)
		}
		// DiffID is the SHA-256 of the raw EROFS image (uncompressed).
		// Computed in-stream by chunked.Build from the raw bytes it reads.
		newDesc.Annotations[contentindex.AnnotationUncompressedDigest] = result.DiffID.String()
		stampDmVerityAnnotations(newDesc.Annotations, verity)
		return &newDesc, nil
	}
}

// streamAppendDmVerity reads src (an EROFS image of exactly srcSize bytes,
// positioned at offset 0) and produces a fresh temp file containing:
//
//	[erofs image][verity superblock @ srcSize][merkle tree]
//
// Returned file is positioned at end (after the tree). Caller must Close+Remove.
//
// Memory: O(leafHashes); for a 2 GiB image at 4096-byte blocks ≈ 16 MiB.
// Disk: srcSize bytes are copied once into dst, then re-read once for hashing.
func streamAppendDmVerity(ctx context.Context, src *os.File, srcSize int64) (*os.File, *erofsutils.DmVerityResult, error) {
	dst, err := os.CreateTemp("", "chunked-erofs-verity-*.bin")
	if err != nil {
		return nil, nil, fmt.Errorf("create verity temp file: %w", err)
	}
	// Copy EROFS image bytes → dst at offsets [0, srcSize).
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		dst.Close()
		os.Remove(dst.Name())
		return nil, nil, fmt.Errorf("seek source: %w", err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(dst.Name())
		return nil, nil, fmt.Errorf("copy erofs to verity temp: %w", err)
	}
	// Append verity SB and merkle tree at offset srcSize, re-reading the
	// data bytes from dst for hashing.
	res, err := erofsutils.AppendDmVerityStream(ctx, dst, dst, srcSize, 0)
	if err != nil {
		dst.Close()
		os.Remove(dst.Name())
		return nil, nil, fmt.Errorf("append dm-verity: %w", err)
	}
	return dst, res, nil
}
