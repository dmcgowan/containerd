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

package local

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"strconv"
	"strings"

	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// EROFS chunk-index v1 binary layout (designs/erofs-image-spec/spec.md §3.4).
//
// Header (32 bytes, little-endian):
//
//	 0:4   Magic            uint32   = 0xCD 0xE4 0xEC 0x67
//	 4     Version          uint8    = 1
//	 5     CompressionType  uint8    (0=none, 1=zstd)
//	 6:8   Flags            uint16   reserved; MUST be 0 in v1
//	 8:16  UncompressedSize uint64
//	16:20  NumChunks        uint32
//	20     HashAlgo         uint8    (0=none, 1=SHA-2)
//	21     HashSize         uint8    (32=SHA-256, 64=SHA-512)
//	22:32  Reserved         [10]byte MUST be all zero
//
// Entry (uniform shape, 16 + HashSize bytes):
//
//	 0:8   BlockOffset        uint64
//	 8:16  UncompressedOffset uint64
//	16:+H  Checksum           [H]byte  (only when HashAlgo != 0)
const (
	chunkIndexMagic        uint32 = 0x67ECE4CD
	chunkIndexHeaderSize          = 32
	chunkIndexVersion             = 1

	// CompressionType values (header byte 5).
	chunkIndexCompressionNone uint8 = 0 // raw byte ranges
	chunkIndexCompressionZstd uint8 = 1 // one zstd frame per chunk

	chunkIndexHashAlgoNone uint8 = 0
	chunkIndexHashAlgoSHA2 uint8 = 1

	zstdSkippableFrameHeaderSize = 8
	// zstdSkippableMagicBase is the lowest magic number for zstd skippable
	// frames (0x184D2A50–0x184D2A5F per RFC 8878 §3.1.1.3).
	zstdSkippableMagicBase uint32 = 0x184D2A50
)

// chunkIndexHeader is the parsed form of the 32-byte header.
type chunkIndexHeader struct {
	UncompressedSize uint64
	NumChunks        uint32
	HashAlgo         uint8
	HashSize         uint8
	Flags            uint16
	CompressionType  uint8
}

// indexLocation describes the chunk index's position within the original
// blob, parsed from the descriptor's annotations.
type indexLocation struct {
	// Offset is the absolute byte offset of the start of the chunk-index
	// section in the blob. For +zstd this is the first byte of the
	// enclosing skippable frame; for raw this is the first byte of the
	// 32-byte header (the Magic).
	Offset int64

	// End is one past the last byte of the chunk-index section, or 0 if
	// the section runs to the end of the blob.
	End int64

	// Digest is the chunk-index digest, when annotated.
	Digest digest.Digest

	// MediaType is the chunk-index media type, defaulting to
	// ChunkIndexMediaTypeEROFSV1.
	MediaType string

	// HeaderOffset is the absolute offset of the chunk-index header
	// (the 32-byte header). For raw layers HeaderOffset == Offset; for
	// +zstd layers it is Offset + 8 (after the skippable-frame header).
	HeaderOffset int64
}

// parseIndexLocation reads org.erofs.chunk-index.* annotations from a
// descriptor and returns the resolved chunk-index location. Returns an error
// when the descriptor does not declare a chunk index.
func parseIndexLocation(desc ocispec.Descriptor) (*indexLocation, error) {
	rawRange, ok := desc.Annotations[contentindex.AnnotationChunkIndexRange]
	if !ok {
		return nil, fmt.Errorf("content/index: descriptor missing %s annotation",
			contentindex.AnnotationChunkIndexRange)
	}
	off, end, err := parseRange(rawRange)
	if err != nil {
		return nil, err
	}
	loc := &indexLocation{
		Offset:    off,
		End:       end,
		MediaType: desc.Annotations[contentindex.AnnotationChunkIndexMediaType],
	}
	if loc.MediaType == "" {
		loc.MediaType = contentindex.ChunkIndexMediaTypeEROFSV1
	}
	if d, ok := desc.Annotations[contentindex.AnnotationChunkIndexDigest]; ok && d != "" {
		dgst, err := digest.Parse(d)
		if err != nil {
			return nil, fmt.Errorf("content/index: invalid %s annotation: %w",
				contentindex.AnnotationChunkIndexDigest, err)
		}
		loc.Digest = dgst
	}
	// For +zstd layers the chunk index is wrapped in a zstd skippable
	// frame; the 32-byte header begins 8 bytes after the section start.
	if isZstdMediaType(desc.MediaType) {
		loc.HeaderOffset = off + zstdSkippableFrameHeaderSize
	} else {
		loc.HeaderOffset = off
	}
	return loc, nil
}

// parseRange parses an erofs-image-spec range value of the form
// "offset[:end]" into [offset, end).
func parseRange(s string) (int64, int64, error) {
	parts := strings.SplitN(s, ":", 2)
	off, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("content/index: invalid range offset %q: %w", parts[0], err)
	}
	if off < 0 {
		return 0, 0, fmt.Errorf("content/index: negative range offset %d", off)
	}
	if len(parts) == 1 {
		return off, 0, nil
	}
	end, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("content/index: invalid range end %q: %w", parts[1], err)
	}
	if end < off {
		return 0, 0, fmt.Errorf("content/index: range end %d < offset %d", end, off)
	}
	return off, end, nil
}

// parseDmVerity reads org.erofs.dmverity.* annotations from a descriptor.
// Returns nil and a nil error when no dm-verity annotations are present.
//
// dm-verity parameters are NOT stored in the metadata record; callers that
// need them (e.g. mount activation) call this function directly with the
// descriptor.
func parseDmVerity(desc ocispec.Descriptor) (*contentindex.DmVerityInfo, error) {
	rawOffset, hasOffset := desc.Annotations[contentindex.AnnotationDmVerityHashOffset]
	rawDigest, hasDigest := desc.Annotations[contentindex.AnnotationDmVerityRootDigest]
	if !hasOffset && !hasDigest {
		return nil, nil
	}
	if !hasOffset || !hasDigest {
		return nil, fmt.Errorf(
			"content/index: dm-verity annotations must be all-or-nothing (have offset=%v digest=%v)",
			hasOffset, hasDigest)
	}
	offset, err := strconv.ParseInt(rawOffset, 10, 64)
	if err != nil || offset < 0 {
		return nil, fmt.Errorf("content/index: invalid %s value %q",
			contentindex.AnnotationDmVerityHashOffset, rawOffset)
	}
	dgst, err := digest.Parse(rawDigest)
	if err != nil {
		return nil, fmt.Errorf("content/index: invalid %s value: %w",
			contentindex.AnnotationDmVerityRootDigest, err)
	}
	bs := contentindex.DefaultDmVerityBlockSize
	if rawBs, ok := desc.Annotations[contentindex.AnnotationDmVerityBlockSize]; ok && rawBs != "" {
		v, err := strconv.ParseUint(rawBs, 10, 32)
		if err != nil || v == 0 {
			return nil, fmt.Errorf("content/index: invalid %s value %q",
				contentindex.AnnotationDmVerityBlockSize, rawBs)
		}
		bs = uint32(v)
	}
	if int64(bs) > 0 && offset%int64(bs) != 0 {
		return nil, fmt.Errorf(
			"content/index: dm-verity hash_offset %d not a multiple of block_size %d",
			offset, bs)
	}
	return &contentindex.DmVerityInfo{
		RootDigest: dgst,
		HashOffset: offset,
		BlockSize:  bs,
	}, nil
}

// chunkIndexEntrySize returns the byte size of a single chunk entry.
// The entry layout is uniform: BlockOffset(8) + UncompressedOffset(8) +
// Checksum(HashSize).
func chunkIndexEntrySize(hdr *chunkIndexHeader) (int, error) {
	if hdr.HashAlgo != chunkIndexHashAlgoNone && hdr.HashAlgo != chunkIndexHashAlgoSHA2 {
		return 0, fmt.Errorf("content/index: unsupported HashAlgo %d", hdr.HashAlgo)
	}
	return 16 + int(hdr.HashSize), nil
}

// parseChunkIndex reads and decodes the chunk index from r at the position
// given by loc.HeaderOffset.  The returned slice is in chunk-index entry
// order (which is also ascending UncompressedOffset order).
//
// Each ChunkRef has its Digest populated only when the index carries
// per-chunk checksums (HashAlgo != 0).
func parseChunkIndex(r io.ReaderAt, loc *indexLocation, mediaType string) ([]contentindex.ChunkRef, *chunkIndexHeader, error) {
	hdr, err := readChunkIndexHeader(r, loc.HeaderOffset)
	if err != nil {
		return nil, nil, err
	}
	// Validate CompressionType against the carrying layer's media type.
	if err := validateCompressionType(hdr.CompressionType, mediaType); err != nil {
		return nil, nil, err
	}
	entrySize, err := chunkIndexEntrySize(hdr)
	if err != nil {
		return nil, nil, err
	}
	n := int(hdr.NumChunks)
	if n == 0 {
		return nil, hdr, nil
	}

	// Validate that the payload length is exactly what NumChunks implies.
	if err := validatePayloadLength(loc, hdr, mediaType, n, entrySize); err != nil {
		return nil, nil, err
	}

	entriesOffset := loc.HeaderOffset + chunkIndexHeaderSize
	buf := make([]byte, int64(n)*int64(entrySize))
	if _, err := r.ReadAt(buf, entriesOffset); err != nil {
		return nil, nil, fmt.Errorf("content/index: read chunk entries: %w", err)
	}

	zstd := hdr.CompressionType == chunkIndexCompressionZstd
	hashed := hdr.HashAlgo != chunkIndexHashAlgoNone
	chunks := make([]contentindex.ChunkRef, n)

	for i := 0; i < n; i++ {
		entry := buf[i*entrySize : (i+1)*entrySize]
		blockOff := int64(binary.LittleEndian.Uint64(entry[0:8]))
		uncompOff := int64(binary.LittleEndian.Uint64(entry[8:16]))
		pos := 16

		var chunkDigest digest.Digest
		if hashed {
			sum := entry[pos : pos+int(hdr.HashSize)]
			alg := digestAlgorithmForHash(hdr.HashAlgo, hdr.HashSize)
			if alg == "" {
				return nil, nil, fmt.Errorf(
					"content/index: unsupported HashAlgo=%d HashSize=%d",
					hdr.HashAlgo, hdr.HashSize)
			}
			chunkDigest = digest.NewDigestFromBytes(alg, sum)
		}
		chunks[i] = contentindex.ChunkRef{
			Digest:      chunkDigest,
			Offset:      uncompOff,
			OnBlobStart: blockOff,
		}
	}

	// Compute on-blob lengths and logical lengths.
	chunkIndexStart := loc.Offset
	totalImageData := int64(hdr.UncompressedSize)

	for i := range chunks {
		// Logical length: distance to next UncompressedOffset (or end).
		var nextLogical int64
		if i+1 < len(chunks) {
			nextLogical = chunks[i+1].Offset
		} else {
			nextLogical = totalImageData
		}
		chunks[i].Length = nextLogical - chunks[i].Offset

		// On-blob length depends on compression.
		var endOnBlob int64
		if zstd {
			if i+1 < len(chunks) {
				endOnBlob = chunks[i+1].OnBlobStart
			} else {
				endOnBlob = chunkIndexStart
			}
		} else {
			// CompressionType=0: raw bytes; on-blob length == logical length.
			endOnBlob = chunks[i].OnBlobStart + chunks[i].Length
		}
		chunks[i].OnBlobEnd = endOnBlob
	}

	if err := validateChunks(chunks, totalImageData, chunkIndexStart, zstd); err != nil {
		return nil, nil, err
	}
	return chunks, hdr, nil
}

// parseChunkIndexPayload parses a chunk-index from its raw payload bytes —
// the 32-byte header followed by N chunk entries, without any surrounding
// zstd skippable-frame header.
//
// This is used when the chunk-index is read back from its content-store entry
// rather than from the original blob.
//
// payload contains exactly the bytes that were hashed to produce IndexDigest.
// blobSectionOffset is the absolute byte offset of the chunk-index section
// in the original blob (used to validate on-blob chunk ranges).
// mediaType is the layer media type.
func parseChunkIndexPayload(payload []byte, blobSectionOffset int64, mediaType string) ([]contentindex.ChunkRef, *chunkIndexHeader, error) {
	// Synthesise an indexLocation that makes parseChunkIndex work correctly
	// when reading from a bytes.Reader over the raw payload.
	//
	// HeaderOffset = 0: the payload starts with the 32-byte header.
	// Offset = blobSectionOffset: used as chunkIndexStart for on-blob
	//   range validation and payload-length checking.
	// End: for the payload-length validation in validatePayloadLength we
	//   need a consistent End. For +zstd the skippable-frame header is
	//   not present in the payload, so we set End = Offset + len(payload) + 8
	//   to account for the 8 bytes that validatePayloadLength subtracts.
	//   For raw, End = Offset + len(payload).
	var end int64
	if isZstdMediaType(mediaType) {
		end = blobSectionOffset + int64(len(payload)) + zstdSkippableFrameHeaderSize
	} else {
		end = blobSectionOffset + int64(len(payload))
	}
	loc := &indexLocation{
		Offset:       blobSectionOffset,
		HeaderOffset: 0,
		End:          end,
		MediaType:    mediaType,
	}
	return parseChunkIndex(bytes.NewReader(payload), loc, mediaType)
}

// readChunkIndexHeader reads and validates the 32-byte header at headerOffset.
func readChunkIndexHeader(r io.ReaderAt, headerOffset int64) (*chunkIndexHeader, error) {
	var raw [chunkIndexHeaderSize]byte
	if _, err := r.ReadAt(raw[:], headerOffset); err != nil {
		return nil, fmt.Errorf("content/index: read chunk index header: %w", err)
	}
	magic := binary.LittleEndian.Uint32(raw[0:4])
	if magic != chunkIndexMagic {
		return nil, fmt.Errorf("content/index: bad chunk index magic 0x%08x", magic)
	}
	version := raw[4]
	if version != chunkIndexVersion {
		return nil, fmt.Errorf("content/index: unsupported chunk index version %d", version)
	}
	compressionType := raw[5]
	flags := binary.LittleEndian.Uint16(raw[6:8])
	if flags != 0 {
		return nil, fmt.Errorf(
			"content/index: unsupported Flags 0x%04x; falling back to sequential reads",
			flags)
	}
	hdr := &chunkIndexHeader{
		CompressionType:  compressionType,
		Flags:            flags,
		UncompressedSize: binary.LittleEndian.Uint64(raw[8:16]),
		NumChunks:        binary.LittleEndian.Uint32(raw[16:20]),
		HashAlgo:         raw[20],
		HashSize:         raw[21],
	}
	// Validate reserved bytes 22-31 are all zero.
	for i := 22; i < 32; i++ {
		if raw[i] != 0 {
			return nil, fmt.Errorf(
				"content/index: non-zero reserved header byte at offset %d: 0x%02x",
				i, raw[i])
		}
	}
	switch hdr.CompressionType {
	case chunkIndexCompressionNone, chunkIndexCompressionZstd:
		// recognized
	default:
		return nil, fmt.Errorf(
			"content/index: unsupported CompressionType %d", hdr.CompressionType)
	}
	if hdr.HashAlgo == chunkIndexHashAlgoNone && hdr.HashSize != 0 {
		return nil, fmt.Errorf("content/index: HashSize must be 0 when HashAlgo is 0")
	}
	return hdr, nil
}

// validateCompressionType checks that the header's CompressionType is
// consistent with the carrying layer's media type.
func validateCompressionType(compressionType uint8, mediaType string) error {
	zstd := isZstdMediaType(mediaType)
	switch compressionType {
	case chunkIndexCompressionZstd:
		if !zstd {
			return fmt.Errorf(
				"content/index: CompressionType=zstd but layer media type %q is not +zstd",
				mediaType)
		}
	case chunkIndexCompressionNone:
		if zstd {
			return fmt.Errorf(
				"content/index: CompressionType=none but layer media type %q is +zstd",
				mediaType)
		}
	}
	return nil
}

// validatePayloadLength verifies that the payload accommodates exactly
// NumChunks entries.
func validatePayloadLength(loc *indexLocation, hdr *chunkIndexHeader, mediaType string, n, entrySize int) error {
	if loc.End == 0 {
		// End unknown (omitted annotation or standalone layer with no bound set
		// other than the blob size) — skip the check.
		return nil
	}
	sectionLen := loc.End - loc.Offset
	if isZstdMediaType(mediaType) {
		sectionLen -= zstdSkippableFrameHeaderSize
	}
	expectedPayload := int64(chunkIndexHeaderSize) + int64(n)*int64(entrySize)
	if sectionLen != expectedPayload {
		return fmt.Errorf(
			"content/index: chunk index payload length %d != expected %d (NumChunks=%d entrySize=%d)",
			sectionLen, expectedPayload, n, entrySize)
	}
	return nil
}

// validateChunks performs sanity checks on the parsed chunk slice.
func validateChunks(chunks []contentindex.ChunkRef, imageDataSize, chunkIndexStart int64, zstd bool) error {
	for i, c := range chunks {
		if c.OnBlobStart < 0 || c.OnBlobEnd < c.OnBlobStart {
			return fmt.Errorf(
				"content/index: chunk %d has invalid on-blob range [%d, %d)",
				i, c.OnBlobStart, c.OnBlobEnd)
		}
		if zstd {
			if c.OnBlobEnd > chunkIndexStart {
				return fmt.Errorf(
					"content/index: chunk %d on-blob end %d overlaps chunk index at %d",
					i, c.OnBlobEnd, chunkIndexStart)
			}
		} else if c.OnBlobEnd > imageDataSize {
			return fmt.Errorf(
				"content/index: chunk %d on-blob end %d exceeds image data size %d",
				i, c.OnBlobEnd, imageDataSize)
		}
		if c.Length < 0 {
			return fmt.Errorf(
				"content/index: chunk %d has negative logical length %d", i, c.Length)
		}
		if i > 0 && chunks[i].OnBlobStart < chunks[i-1].OnBlobEnd {
			return fmt.Errorf(
				"content/index: chunk %d on-blob range overlaps previous chunk", i)
		}
	}
	return nil
}

// digestAlgorithmForHash maps (HashAlgo, HashSize) to a digest.Algorithm.
// Returns "" if the combination is unsupported.
func digestAlgorithmForHash(algo, size uint8) digest.Algorithm {
	if algo != chunkIndexHashAlgoSHA2 {
		return ""
	}
	switch size {
	case 32:
		return digest.SHA256
	case 64:
		return digest.SHA512
	}
	return ""
}

// isZstdMediaType reports whether mediaType identifies a +zstd layer variant.
// Recognises both canonical (application/vnd.erofs+zstd) and legacy
// (application/vnd.erofs.layer.v1+zstd) media types.
func isZstdMediaType(mediaType string) bool {
	switch mediaType {
	case contentindex.MediaTypeEROFSZstd,
		contentindex.MediaTypeEROFSLayerZstd:
		return true
	}
	return false
}
