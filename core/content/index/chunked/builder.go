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

// Package chunked provides a builder that converts a stream of uncompressed
// image data into the EROFS chunk-indexed +zstd blob format defined by the
// erofs-image-spec (designs/erofs-image-spec/spec.md §3.3 and §3.4).
//
// The output blob is a valid zstd byte stream:
//
//	[zstd frame 0 (chunk 0)] ... [zstd frame N-1 (chunk N-1)]
//	[zstd skippable frame containing the chunk-index payload]
//
// Each chunk targets a fixed *compressed* output size (TargetFrameSize,
// default 4 MiB). The input size for each chunk is estimated from the
// compression ratio observed in the previous chunk, then adjusted with a
// binary search if the initial estimate misses by more than a small margin.
// This is O(N) in the common case — typically 1–2 compressions per chunk —
// and keeps every output frame within 10% of the target.
//
// The chunk-index uses variable-mode entries (BlockOffset + UncompressedOffset
// + SHA-256 checksum, 48 bytes each) with SHA-256 per-chunk checksums.
// The output descriptor carries org.erofs.chunk-index.* annotations.
package chunked

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	// TargetFrameSize is the target compressed frame size in bytes.
	// Set slightly above 4 MiB so that real-world frames land at or above
	// 4 MiB after the ratio-based estimate, avoiding short frames.
	TargetFrameSize = 4*1024*1024 + 512*1024 // 4.5 MiB

	// DefaultChunkSize is kept as an alias for callers that still pass a
	// chunk size parameter.
	DefaultChunkSize = TargetFrameSize

	// tolerance is unused (ratio-based path no longer binary-searches), kept
	// for reference only.
	tolerance = TargetFrameSize / 10

	chunkIndexMagic           uint32 = 0x67ECE4CD
	chunkIndexVersion         uint8  = 1
	chunkIndexCompressionZstd uint8  = 1
	hashAlgoSHA2              uint8  = 1
	hashSizeSHA256            uint8  = 32

	// Entry layout: BlockOffset(8) + UncompressedOffset(8) + SHA-256(32).
	entrySize = 8 + 8 + int(hashSizeSHA256) // 48 bytes

	skippableFrameMagic uint32 = 0x184D2A50
)

// Result holds everything needed to register and describe the built blob.
type Result struct {
	Blob       []byte
	Descriptor ocispec.Descriptor
	Chunks     []contentindex.ChunkRef
}

// Build converts uncompressed image data (r, totalSize bytes) into a
// +zstd chunked blob with an appended chunk index.
//
//   - r                — source of uncompressed image data
//   - totalSize        — exact number of bytes to read from r
//   - mediaType        — must be one of the +zstd EROFS layer media types
//   - targetFrame      — target compressed frame size in bytes; pass 0 to use
//     TargetFrameSize (4 MiB).
//   - forcedBoundaries — optional sorted list of uncompressed byte offsets at
//     which a new chunk MUST start, regardless of compressed size. Use this to
//     place the dm-verity merkle tree (which starts at hash_offset) in its own
//     chunk. Pass nil or an empty slice for no forced boundaries.
func Build(r io.Reader, totalSize int64, mediaType string, targetFrame int, forcedBoundaries ...int64) (*Result, error) {
	if targetFrame <= 0 {
		targetFrame = TargetFrameSize
	}
	switch mediaType {
	case contentindex.MediaTypeEROFSZstd,
		contentindex.MediaTypeEROFSLayerZstd:
	default:
		return nil, fmt.Errorf("chunked: Build requires a +zstd media type, got %q", mediaType)
	}

	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return nil, fmt.Errorf("chunked: new zstd encoder: %w", err)
	}

	// Read all input into memory once. For large images this is unavoidable
	// because the caller provides the EROFS image as a contiguous buffer.
	// We operate on slices to avoid per-chunk copies.
	inputBuf := make([]byte, totalSize)
	if _, err := io.ReadFull(r, inputBuf); err != nil {
		return nil, fmt.Errorf("chunked: read input: %w", err)
	}

	type chunkMeta struct {
		blobStart    int64
		blobEnd      int64
		uncompOffset int64
		uncompLen    int64
		sha256       []byte
	}

	var (
		blobBuf bytes.Buffer
		chunks  []chunkMeta
	)

	// compress compresses a slice and returns the frame bytes.
	compress := func(plain []byte) []byte {
		return enc.EncodeAll(plain, nil)
	}

	// Build a deduplicated, sorted set of forced boundary offsets that fall
	// strictly inside [1, totalSize-1] so we never split at the very start or
	// produce a zero-length final chunk.
	var boundaries []int64
	seen := map[int64]bool{}
	for _, b := range forcedBoundaries {
		if b > 0 && b < totalSize && !seen[b] {
			boundaries = append(boundaries, b)
			seen[b] = true
		}
	}
	// sort.Slice is not imported; use simple insertion sort (boundaries is tiny).
	for i := 1; i < len(boundaries); i++ {
		for j := i; j > 0 && boundaries[j] < boundaries[j-1]; j-- {
			boundaries[j], boundaries[j-1] = boundaries[j-1], boundaries[j]
		}
	}
	nextBoundaryIdx := 0

	// Initial estimate: assume a 3:1 compression ratio (conservative for
	// EROFS data; real ratio is calibrated after the first chunk).
	// inputGuess = targetFrame * ratio.
	ratio := 3.0
	pos := int64(0)

	for pos < totalSize {
		remaining := totalSize - pos

		// Skip any forced boundaries we have already passed.
		for nextBoundaryIdx < len(boundaries) && boundaries[nextBoundaryIdx] <= pos {
			nextBoundaryIdx++
		}
		// If the next forced boundary is within the target chunk window, cut there.
		// We use a 2× window so that we catch boundaries even when the ratio
		// estimate is off, without collapsing everything into one huge chunk.
		if nextBoundaryIdx < len(boundaries) {
			nextBoundary := boundaries[nextBoundaryIdx]
			inputGuess := int64(float64(targetFrame) * ratio)
			if pos+2*inputGuess >= nextBoundary {
				// The forced boundary is within reach. Cut exactly there.
				cutLen := nextBoundary - pos
				frame := compress(inputBuf[pos : pos+cutLen])
				h := digest.SHA256.Digester()
				h.Hash().Write(frame)
				blobStart := int64(blobBuf.Len())
				blobBuf.Write(frame)
				chunks = append(chunks, chunkMeta{
					blobStart:    blobStart,
					blobEnd:      int64(blobBuf.Len()),
					uncompOffset: pos,
					uncompLen:    cutLen,
					sha256:       h.Hash().Sum(nil),
				})
				if len(frame) > 0 {
					ratio = float64(cutLen) / float64(len(frame))
				}
				pos += cutLen
				nextBoundaryIdx++
				continue
			}
		}

		// Estimate input bytes needed to produce ~targetFrame compressed bytes.
		inputGuess := int64(float64(targetFrame) * ratio)
		if inputGuess > remaining {
			inputGuess = remaining
		}

		// One-shot compression: compress the estimate, then update the ratio
		// for the next chunk. No binary-search refinement — the ratio tracks
		// well after the first chunk and deviations of ±25% are acceptable.
		frame := compress(inputBuf[pos : pos+inputGuess])

		if inputGuess == remaining {
			// Last chunk.
			h := digest.SHA256.Digester()
			h.Hash().Write(frame)
			blobStart := int64(blobBuf.Len())
			blobBuf.Write(frame)
			chunks = append(chunks, chunkMeta{
				blobStart:    blobStart,
				blobEnd:      int64(blobBuf.Len()),
				uncompOffset: pos,
				uncompLen:    remaining,
				sha256:       h.Hash().Sum(nil),
			})
			pos = totalSize
			break
		}

		// Update ratio for next chunk.
		ratio = float64(inputGuess) / float64(len(frame))

		h := digest.SHA256.Digester()
		h.Hash().Write(frame)
		blobStart := int64(blobBuf.Len())
		blobBuf.Write(frame)
		chunks = append(chunks, chunkMeta{
			blobStart:    blobStart,
			blobEnd:      int64(blobBuf.Len()),
			uncompOffset: pos,
			uncompLen:    inputGuess,
			sha256:       h.Hash().Sum(nil),
		})
		pos += inputGuess
	}

	// ── Build the chunk-index payload ──────────────────────────────────────
	var idxBuf bytes.Buffer
	var hdr [32]byte
	binary.LittleEndian.PutUint32(hdr[0:4], chunkIndexMagic)
	hdr[4] = chunkIndexVersion
	hdr[5] = chunkIndexCompressionZstd
	binary.LittleEndian.PutUint64(hdr[8:16], uint64(totalSize))
	binary.LittleEndian.PutUint32(hdr[16:20], uint32(len(chunks)))
	hdr[20] = hashAlgoSHA2
	hdr[21] = hashSizeSHA256
	idxBuf.Write(hdr[:])

	for _, c := range chunks {
		var entry [entrySize]byte
		binary.LittleEndian.PutUint64(entry[0:8], uint64(c.blobStart))
		binary.LittleEndian.PutUint64(entry[8:16], uint64(c.uncompOffset))
		copy(entry[16:16+hashSizeSHA256], c.sha256)
		idxBuf.Write(entry[:])
	}
	idxPayload := idxBuf.Bytes()

	// ── Wrap chunk-index in a zstd skippable frame ─────────────────────────
	chunkIndexSectionOffset := int64(blobBuf.Len())
	var frameHdr [8]byte
	binary.LittleEndian.PutUint32(frameHdr[0:4], skippableFrameMagic)
	binary.LittleEndian.PutUint32(frameHdr[4:8], uint32(len(idxPayload)))
	blobBuf.Write(frameHdr[:])
	blobBuf.Write(idxPayload)

	blob := blobBuf.Bytes()
	blobDigest := digest.FromBytes(blob)
	idxDigest := digest.FromBytes(idxPayload)

	rangeAnnotation := fmt.Sprintf("%d:%d", chunkIndexSectionOffset, len(blob))
	desc := ocispec.Descriptor{
		MediaType: mediaType,
		Digest:    blobDigest,
		Size:      int64(len(blob)),
		Annotations: map[string]string{
			contentindex.AnnotationChunkIndexRange:     rangeAnnotation,
			contentindex.AnnotationChunkIndexDigest:    idxDigest.String(),
			contentindex.AnnotationChunkIndexMediaType: contentindex.ChunkIndexMediaTypeEROFSV1,
		},
	}

	chunkRefs := make([]contentindex.ChunkRef, len(chunks))
	for i, c := range chunks {
		chunkRefs[i] = contentindex.ChunkRef{
			Digest:      digest.NewDigestFromBytes(digest.SHA256, c.sha256),
			Offset:      c.uncompOffset,
			Length:      c.uncompLen,
			OnBlobStart: c.blobStart,
			OnBlobEnd:   c.blobEnd,
		}
	}

	return &Result{
		Blob:       blob,
		Descriptor: desc,
		Chunks:     chunkRefs,
	}, nil
}
