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
//   [zstd frame 0 (chunk 0)] ... [zstd frame N-1 (chunk N-1)]
//   [zstd skippable frame containing the chunk-index payload]
//
// The chunk-index uses fixed-size mode (ChunkSize > 0) with SHA-256 per-chunk
// checksums so the result qualifies for the tier-1 index-based DiffID per
// spec §5.2.  The output descriptor carries org.erofs.index.* annotations.
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
	// DefaultChunkSize is the default uncompressed chunk size (4 MiB).
	// For testing, callers can pass smaller values.
	DefaultChunkSize = 4 * 1024 * 1024

	chunkIndexMagic   uint32 = 0x67ECE4CD
	chunkIndexVersion uint32 = 1
	hashAlgoSHA2      uint8  = 1
	hashSizeSHA256    uint8  = 32

	// zstd skippable frame magic (RFC 8878 §3.1.1.3).
	skippableFrameMagic uint32 = 0x184D2A50
)

// Result holds everything needed to register and describe the built blob.
type Result struct {
	// Blob is the complete on-wire blob bytes.
	Blob []byte

	// Descriptor is the OCI descriptor for the blob, with media type,
	// digest, size, and all org.erofs.index.* annotations populated.
	Descriptor ocispec.Descriptor

	// Chunks lists the parsed ChunkRef for each compressed chunk, in order.
	// OnBlobStart/OnBlobEnd refer to offsets within Blob.
	Chunks []contentindex.ChunkRef
}

// Build converts uncompressed image data (r, totalSize bytes) into a
// +zstd chunked blob with an appended chunk index.
//
// Parameters:
//   - r           — source of uncompressed image data
//   - totalSize   — exact number of bytes to read from r
//   - mediaType   — must be one of the +zstd EROFS layer media types
//   - chunkSize   — uncompressed chunk size in bytes; must be > 0
func Build(r io.Reader, totalSize int64, mediaType string, chunkSize int) (*Result, error) {
	if chunkSize <= 0 {
		return nil, fmt.Errorf("chunked: chunkSize must be > 0")
	}
	switch mediaType {
	case contentindex.MediaTypeEROFSLayerZstd,
		contentindex.MediaTypeEROFSLayerMergedZstd,
		contentindex.MediaTypeEROFSLayerDataZstd:
	default:
		return nil, fmt.Errorf("chunked: Build requires a +zstd media type, got %q", mediaType)
	}

	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return nil, fmt.Errorf("chunked: new zstd encoder: %w", err)
	}

	type chunkMeta struct {
		blobStart int64
		blobEnd   int64
		sha256    []byte // SHA-256 of the compressed frame
	}

	var (
		blobBuf     bytes.Buffer
		chunks      []chunkMeta
		uncompTotal int64
		plain       = make([]byte, chunkSize)
	)

	for uncompTotal < totalSize {
		want := int64(chunkSize)
		if totalSize-uncompTotal < want {
			want = totalSize - uncompTotal
		}
		n, err := io.ReadFull(r, plain[:want])
		if err != nil && err != io.ErrUnexpectedEOF {
			return nil, fmt.Errorf("chunked: read chunk: %w", err)
		}
		if int64(n) < want {
			return nil, fmt.Errorf("chunked: short read: got %d bytes, want %d", n, want)
		}

		frame := enc.EncodeAll(plain[:n], nil)

		// SHA-256 is computed over the on-blob bytes (the compressed frame),
		// per erofs-image-spec §3.4.6.
		h := digest.SHA256.Digester()
		h.Hash().Write(frame)
		sum := h.Hash().Sum(nil)

		blobStart := int64(blobBuf.Len())
		blobBuf.Write(frame)
		chunks = append(chunks, chunkMeta{
			blobStart: blobStart,
			blobEnd:   int64(blobBuf.Len()),
			sha256:    sum,
		})
		uncompTotal += int64(n)
	}

	// ── Build the chunk-index payload ──────────────────────────────────────
	// Fixed-size +zstd mode: entry shape = BlockOffset(8) + Checksum(32).
	const entrySize = 8 + int(hashSizeSHA256) // 40 bytes
	var idxBuf bytes.Buffer

	// 24-byte header.
	var hdr [24]byte
	binary.LittleEndian.PutUint32(hdr[0:4], chunkIndexMagic)
	binary.LittleEndian.PutUint32(hdr[4:8], chunkIndexVersion)
	binary.LittleEndian.PutUint64(hdr[8:16], uint64(uncompTotal))
	binary.LittleEndian.PutUint32(hdr[16:20], uint32(chunkSize))
	hdr[20] = hashAlgoSHA2
	hdr[21] = hashSizeSHA256
	hdr[22] = 0 // no Weight flag
	hdr[23] = 0 // Reserved
	idxBuf.Write(hdr[:])

	// One entry per chunk.
	for _, c := range chunks {
		var entry [entrySize]byte
		binary.LittleEndian.PutUint64(entry[0:8], uint64(c.blobStart))
		copy(entry[8:8+hashSizeSHA256], c.sha256)
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

	// ── Compute digests ────────────────────────────────────────────────────
	blobDigest := digest.FromBytes(blob)
	idxDigest := digest.FromBytes(idxPayload)

	// ── Build OCI descriptor with annotations ─────────────────────────────
	rangeAnnotation := fmt.Sprintf("%d:%d", chunkIndexSectionOffset, len(blob))
	desc := ocispec.Descriptor{
		MediaType: mediaType,
		Digest:    blobDigest,
		Size:      int64(len(blob)),
		Annotations: map[string]string{
			contentindex.AnnotationIndexRange:     rangeAnnotation,
			contentindex.AnnotationIndexDigest:    idxDigest.String(),
			contentindex.AnnotationIndexMediaType: contentindex.ChunkIndexMediaTypeEROFSv1,
		},
	}

	// ── Build ChunkRef list ────────────────────────────────────────────────
	chunkRefs := make([]contentindex.ChunkRef, len(chunks))
	for i, c := range chunks {
		uncompOff := int64(i) * int64(chunkSize)
		uncompEnd := uncompOff + int64(chunkSize)
		if uncompEnd > uncompTotal {
			uncompEnd = uncompTotal
		}
		chunkRefs[i] = contentindex.ChunkRef{
			Digest:      digest.NewDigestFromBytes(digest.SHA256, c.sha256),
			Offset:      uncompOff,
			Length:      uncompEnd - uncompOff,
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
