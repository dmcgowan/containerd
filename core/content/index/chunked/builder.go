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

// Package chunked provides a builder that converts a raw EROFS image into the
// EROFS chunk-indexed +zstd blob format defined by the erofs-image-spec
// (designs/erofs-image-spec/spec.md §3.3 and §3.4).
//
// The output blob is a valid zstd byte stream:
//
//	[zstd frame 0 (chunk 0)] ... [zstd frame N-1 (chunk N-1)]
//	[zstd skippable frame containing the chunk-index payload]
//
// Each chunk targets a fixed *compressed* output size (TargetFrameSize,
// default 4 MiB). The input size for each chunk is estimated from the
// compression ratio observed in the previous chunk.
//
// Build reads from an io.ReaderAt in per-chunk windows and writes directly to
// an io.Writer — no full-image copy is held in RAM at any point.  The raw
// input bytes are hashed in-stream to produce the DiffID.
//
// The chunk-index uses variable-mode entries (BlockOffset + UncompressedOffset
// + SHA-256 checksum, 48 bytes each) with SHA-256 per-chunk checksums.
package chunked

import (
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

	chunkIndexMagic           uint32 = 0x67ECE4CD
	chunkIndexVersion         uint8  = 1
	chunkIndexCompressionZstd uint8  = 1
	hashAlgoSHA2              uint8  = 1
	hashSizeSHA256            uint8  = 32

	// Entry layout: BlockOffset(8) + UncompressedOffset(8) + SHA-256(32).
	entrySize = 8 + 8 + int(hashSizeSHA256) // 48 bytes

	skippableFrameMagic uint32 = 0x184D2A50
)

// Result holds the output of Build.  The actual blob bytes are written
// directly to the io.Writer passed to Build; this struct carries the
// metadata the caller needs to register the descriptor.
type Result struct {
	// Written is the total number of bytes written to the io.Writer.
	Written int64
	// DiffID is the SHA-256 digest of the raw (uncompressed) EROFS input
	// bytes, suitable for use as the layer's diff-id annotation.
	DiffID digest.Digest
	// Annotations are the chunk-index descriptor annotations
	// (AnnotationChunkIndexRange, AnnotationChunkIndexDigest,
	// AnnotationChunkIndexMediaType) ready to be merged into the layer
	// descriptor.
	Annotations map[string]string
	// Chunks contains per-chunk addressing metadata.
	Chunks []contentindex.ChunkRef
	// Descriptor has MediaType, Annotations, and Size populated.
	// Digest is intentionally left empty — the caller reads it from the
	// content.Writer after committing (cw.Digest()).
	Descriptor ocispec.Descriptor
}

// Build converts a raw EROFS image (ra, totalSize bytes) into a +zstd
// chunked blob with an appended chunk index, writing it directly to w.
//
//   - ra              — source of the raw EROFS image (must be readable at
//     any offset; *os.File satisfies this)
//   - totalSize       — exact byte size of the EROFS image
//   - w               — destination writer (typically a content.Writer)
//   - mediaType       — must be one of the +zstd EROFS layer media types
//   - targetFrame     — target compressed frame size in bytes; pass 0 to use
//     TargetFrameSize (4.5 MiB)
//   - forcedBoundaries — optional sorted list of uncompressed byte offsets
//     at which a new chunk MUST start (e.g. for dm-verity hash_offset).
//
// No full-image copy is held in RAM.  Each chunk is read from ra in a
// per-chunk window, compressed, and written to w.  The raw EROFS bytes are
// hashed in-stream to produce Result.DiffID.
func Build(ra io.ReaderAt, totalSize int64, w io.Writer, mediaType string, targetFrame int, forcedBoundaries ...int64) (*Result, error) {
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

	// Build a deduplicated, sorted set of forced boundary offsets that fall
	// strictly inside [1, totalSize-1].
	var boundaries []int64
	seen := map[int64]bool{}
	for _, b := range forcedBoundaries {
		if b > 0 && b < totalSize && !seen[b] {
			boundaries = append(boundaries, b)
			seen[b] = true
		}
	}
	for i := 1; i < len(boundaries); i++ {
		for j := i; j > 0 && boundaries[j] < boundaries[j-1]; j-- {
			boundaries[j], boundaries[j-1] = boundaries[j-1], boundaries[j]
		}
	}

	var (
		chunks           []chunkMeta
		blobOffset       int64    // running total of bytes written to w
		nextBoundaryIdx  int
		ratio            = 3.0   // initial estimate: 3:1 compression ratio
		pos              = int64(0)

		// Reusable read buffer; grown on demand but never shrunk.
		chunkBuf []byte

		// DiffID hasher: accumulates raw EROFS bytes from every chunk in order.
		diffIDHasher = digest.SHA256.Digester()
	)

	writeFrame := func(frame []byte, uncompOffset, uncompLen int64) error {
		// Hash the compressed frame bytes for the per-chunk SHA-256 entry.
		h := digest.SHA256.Digester()
		h.Hash().Write(frame)

		start := blobOffset
		n, werr := w.Write(frame)
		blobOffset += int64(n)
		if werr != nil {
			return fmt.Errorf("chunked: write frame: %w", werr)
		}
		chunks = append(chunks, chunkMeta{
			blobStart:    start,
			blobEnd:      blobOffset,
			uncompOffset: uncompOffset,
			uncompLen:    uncompLen,
			sha256:       h.Hash().Sum(nil),
		})
		return nil
	}

	// compressChunk reads [pos, pos+length) from ra, hashes the raw bytes into
	// diffIDHasher, compresses them, writes the frame to w, and records metadata.
	compressChunk := func(chunkPos, length int64) error {
		// Grow the buffer if needed.
		if int64(cap(chunkBuf)) < length {
			chunkBuf = make([]byte, length)
		}
		buf := chunkBuf[:length]
		if _, err := ra.ReadAt(buf, chunkPos); err != nil && err != io.EOF {
			return fmt.Errorf("chunked: read at %d: %w", chunkPos, err)
		}
		diffIDHasher.Hash().Write(buf)
		frame := enc.EncodeAll(buf, nil)
		if len(frame) > 0 {
			ratio = float64(length) / float64(len(frame))
		}
		return writeFrame(frame, chunkPos, length)
	}

	for pos < totalSize {
		remaining := totalSize - pos

		// Skip boundaries we have already passed.
		for nextBoundaryIdx < len(boundaries) && boundaries[nextBoundaryIdx] <= pos {
			nextBoundaryIdx++
		}

		// If the next forced boundary is within reach, cut exactly there.
		if nextBoundaryIdx < len(boundaries) {
			nextBoundary := boundaries[nextBoundaryIdx]
			inputGuess := int64(float64(targetFrame) * ratio)
			if pos+2*inputGuess >= nextBoundary {
				cutLen := nextBoundary - pos
				if err := compressChunk(pos, cutLen); err != nil {
					return nil, err
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
		if err := compressChunk(pos, inputGuess); err != nil {
			return nil, err
		}
		pos += inputGuess
	}

	// ── Build the chunk-index payload ──────────────────────────────────────
	idxPayload := buildChunkIndex(totalSize, chunks)

	// ── Wrap chunk-index in a zstd skippable frame ─────────────────────────
	chunkIndexSectionOffset := blobOffset
	var frameHdr [8]byte
	binary.LittleEndian.PutUint32(frameHdr[0:4], skippableFrameMagic)
	binary.LittleEndian.PutUint32(frameHdr[4:8], uint32(len(idxPayload)))
	if n, werr := w.Write(frameHdr[:]); werr != nil {
		return nil, fmt.Errorf("chunked: write index frame header: %w", werr)
	} else {
		blobOffset += int64(n)
	}
	if n, werr := w.Write(idxPayload); werr != nil {
		return nil, fmt.Errorf("chunked: write index payload: %w", werr)
	} else {
		blobOffset += int64(n)
	}

	idxDigest := digest.FromBytes(idxPayload)
	rangeAnnotation := fmt.Sprintf("%d:%d", chunkIndexSectionOffset, blobOffset)

	annotations := map[string]string{
		contentindex.AnnotationChunkIndexRange:     rangeAnnotation,
		contentindex.AnnotationChunkIndexDigest:    idxDigest.String(),
		contentindex.AnnotationChunkIndexMediaType: contentindex.ChunkIndexMediaTypeEROFSV1,
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
		Written: blobOffset,
		DiffID:  diffIDHasher.Digest(),
		Annotations: annotations,
		Chunks:  chunkRefs,
		Descriptor: ocispec.Descriptor{
			MediaType:   mediaType,
			Size:        blobOffset,
			Annotations: annotations,
		},
	}, nil
}

// buildChunkIndex serialises the chunk-index payload (header + entries).
func buildChunkIndex(totalSize int64, chunks []chunkMeta) []byte {
	var hdr [32]byte
	binary.LittleEndian.PutUint32(hdr[0:4], chunkIndexMagic)
	hdr[4] = chunkIndexVersion
	hdr[5] = chunkIndexCompressionZstd
	binary.LittleEndian.PutUint64(hdr[8:16], uint64(totalSize))
	binary.LittleEndian.PutUint32(hdr[16:20], uint32(len(chunks)))
	hdr[20] = hashAlgoSHA2
	hdr[21] = hashSizeSHA256

	payload := make([]byte, 32+len(chunks)*entrySize)
	copy(payload, hdr[:])
	for i, c := range chunks {
		off := 32 + i*entrySize
		var entry [entrySize]byte
		binary.LittleEndian.PutUint64(entry[0:8], uint64(c.blobStart))
		binary.LittleEndian.PutUint64(entry[8:16], uint64(c.uncompOffset))
		copy(entry[16:16+hashSizeSHA256], c.sha256)
		copy(payload[off:], entry[:])
	}
	return payload
}

// chunkMeta is an unexported alias used inside buildChunkIndex.
type chunkMeta struct {
	blobStart    int64
	blobEnd      int64
	uncompOffset int64
	uncompLen    int64
	sha256       []byte
}
