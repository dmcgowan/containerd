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
	"errors"
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
// Header (24 bytes, little-endian):
//
//   0:4   Magic            = 0xCD 0xE4 0xEC 0x67
//   4:8   Version          = 1
//   8:16  UncompressedSize uint64
//   16:20 ChunkSize        uint32   (0 = variable-size mode)
//   20    HashAlgo         uint8    (0 = none, 1 = SHA-2)
//   21    HashSize         uint8    (32 = SHA-256, 64 = SHA-512)
//   22    Flags            uint8    (bit 0 = per-entry Weight present)
//   23    Reserved         uint8    (must be 0)
//
// Entry (size depends on media type, mode, and flags):
//   BlockOffset        uint64  (always for +zstd; raw only when ChunkSize=0)
//   UncompressedOffset uint64  (only +zstd && ChunkSize=0)
//   Weight             int8    (only when Flags bit 0 set)
//   Checksum           [N]byte (only when HashAlgo != 0; N = HashSize)
const (
	chunkIndexMagic        uint32 = 0x67ECE4CD
	chunkIndexHeaderSize          = 24
	chunkIndexVersion             = 1
	chunkIndexFlagWeight   uint8  = 0x01
	chunkIndexHashAlgoNone uint8  = 0
	chunkIndexHashAlgoSHA2 uint8  = 1

	zstdSkippableFrameHeaderSize = 8
	// zstdSkippableMagicBase is the lowest magic number for zstd skippable
	// frames (0x184D2A50–0x184D2A5F per RFC 8878 §3.1.1.3).
	zstdSkippableMagicBase uint32 = 0x184D2A50
)

// chunkIndexHeader is the parsed form of the 24-byte header.
type chunkIndexHeader struct {
	UncompressedSize uint64
	ChunkSize        uint32
	HashAlgo         uint8
	HashSize         uint8
	Flags            uint8
}

// indexLocation describes the chunk index's position within the original
// blob, parsed from the descriptor's annotations.
type indexLocation struct {
	// Offset is the absolute byte offset of the start of the chunk-index
	// section in the blob. For +zstd this is the first byte of the
	// enclosing skippable frame; for raw this is the first byte of the
	// header (the Magic).
	Offset int64

	// End is one past the last byte of the chunk-index section, or 0 if
	// the section runs to the end of the blob.
	End int64

	// Digest is the chunk-index digest, when annotated.
	Digest digest.Digest

	// MediaType is the chunk-index media type, defaulting to
	// ChunkIndexMediaTypeEROFSv1.
	MediaType string

	// HeaderOffset is the absolute offset of the chunk-index header
	// (the 24-byte header). For raw layers HeaderOffset == Offset; for
	// +zstd layers it is Offset + 8 (after the skippable-frame header).
	HeaderOffset int64
}

// parseIndexLocation reads org.erofs.index.* annotations from a descriptor
// and returns the resolved chunk-index location. Returns an error when the
// descriptor does not declare a chunk index.
func parseIndexLocation(desc ocispec.Descriptor) (*indexLocation, error) {
	rawRange, ok := desc.Annotations[contentindex.AnnotationIndexRange]
	if !ok {
		return nil, fmt.Errorf("content/index: descriptor missing %s annotation", contentindex.AnnotationIndexRange)
	}
	off, end, err := parseRange(rawRange)
	if err != nil {
		return nil, err
	}
	loc := &indexLocation{
		Offset:    off,
		End:       end,
		MediaType: desc.Annotations[contentindex.AnnotationIndexMediaType],
	}
	if loc.MediaType == "" {
		loc.MediaType = contentindex.ChunkIndexMediaTypeEROFSv1
	}
	if d, ok := desc.Annotations[contentindex.AnnotationIndexDigest]; ok && d != "" {
		dgst, err := digest.Parse(d)
		if err != nil {
			return nil, fmt.Errorf("content/index: invalid %s annotation: %w", contentindex.AnnotationIndexDigest, err)
		}
		loc.Digest = dgst
	}
	switch desc.MediaType {
	case contentindex.MediaTypeEROFSLayerZstd,
		contentindex.MediaTypeEROFSLayerMergedZstd,
		contentindex.MediaTypeEROFSLayerDataZstd:
		loc.HeaderOffset = off + zstdSkippableFrameHeaderSize
	default:
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
// dm-verity parameters are NOT stored in the sidecar; callers that need them
// (e.g. mount activation) call this function directly with the descriptor.
func parseDmVerity(desc ocispec.Descriptor) (*contentindex.DmVerityInfo, error) {
	rawOffset, hasOffset := desc.Annotations[contentindex.AnnotationDmVerityHashOffset]
	rawDigest, hasDigest := desc.Annotations[contentindex.AnnotationDmVerityRootDigest]
	if !hasOffset && !hasDigest {
		return nil, nil
	}
	if !hasOffset || !hasDigest {
		return nil, fmt.Errorf("content/index: dm-verity annotations must be all-or-nothing (have offset=%v digest=%v)", hasOffset, hasDigest)
	}
	offset, err := strconv.ParseInt(rawOffset, 10, 64)
	if err != nil || offset < 0 {
		return nil, fmt.Errorf("content/index: invalid %s value %q", contentindex.AnnotationDmVerityHashOffset, rawOffset)
	}
	dgst, err := digest.Parse(rawDigest)
	if err != nil {
		return nil, fmt.Errorf("content/index: invalid %s value: %w", contentindex.AnnotationDmVerityRootDigest, err)
	}
	bs := contentindex.DefaultDmVerityBlockSize
	if rawBs, ok := desc.Annotations[contentindex.AnnotationDmVerityBlockSize]; ok && rawBs != "" {
		v, err := strconv.ParseUint(rawBs, 10, 32)
		if err != nil || v == 0 {
			return nil, fmt.Errorf("content/index: invalid %s value %q", contentindex.AnnotationDmVerityBlockSize, rawBs)
		}
		bs = uint32(v)
	}
	if int64(bs) > 0 && offset%int64(bs) != 0 {
		return nil, fmt.Errorf("content/index: dm-verity hash_offset %d not a multiple of block_size %d", offset, bs)
	}
	return &contentindex.DmVerityInfo{
		RootDigest: dgst,
		HashOffset: offset,
		BlockSize:  bs,
	}, nil
}

// chunkIndexEntrySize returns the byte size of a single chunk entry given
// the layer media type, header chunk-mode (variable when ChunkSize == 0),
// and flags.
func chunkIndexEntrySize(mediaType string, hdr *chunkIndexHeader) (int, error) {
	if hdr.HashAlgo != chunkIndexHashAlgoNone && hdr.HashAlgo != chunkIndexHashAlgoSHA2 {
		return 0, fmt.Errorf("content/index: unsupported HashAlgo %d", hdr.HashAlgo)
	}
	zstd := isZstdMediaType(mediaType)
	variable := hdr.ChunkSize == 0
	size := 0
	switch {
	case zstd && variable:
		size = 16 // BlockOffset + UncompressedOffset
	case zstd && !variable:
		size = 8 // BlockOffset
	case !zstd && variable:
		size = 8 // BlockOffset only
	case !zstd && !variable:
		size = 0
	}
	if hdr.Flags&chunkIndexFlagWeight != 0 {
		size++
	}
	if hdr.HashAlgo != chunkIndexHashAlgoNone {
		size += int(hdr.HashSize)
	}
	return size, nil
}

// parseChunkIndex reads and decodes the chunk index from r at the position
// given by loc.HeaderOffset.  The returned slice is sorted by chunk-index
// entry order (which is also OnBlobStart order).
//
// Each ChunkRef has its Digest populated only when the index carries
// per-chunk checksums (HashAlgo != 0).
func parseChunkIndex(r io.ReaderAt, loc *indexLocation, mediaType string) ([]contentindex.ChunkRef, *chunkIndexHeader, error) {
	hdr, err := readChunkIndexHeader(r, loc.HeaderOffset)
	if err != nil {
		return nil, nil, err
	}
	entrySize, err := chunkIndexEntrySize(mediaType, hdr)
	if err != nil {
		return nil, nil, err
	}
	n, err := chunkCount(loc, hdr, mediaType, entrySize)
	if err != nil {
		return nil, nil, err
	}
	if n == 0 {
		return nil, hdr, nil
	}
	entriesOffset := loc.HeaderOffset + chunkIndexHeaderSize
	buf := make([]byte, int64(n)*int64(entrySize))
	if _, err := r.ReadAt(buf, entriesOffset); err != nil {
		return nil, nil, fmt.Errorf("content/index: read chunk entries: %w", err)
	}
	zstd := isZstdMediaType(mediaType)
	variable := hdr.ChunkSize == 0
	hasWeight := hdr.Flags&chunkIndexFlagWeight != 0
	hashed := hdr.HashAlgo != chunkIndexHashAlgoNone
	chunks := make([]contentindex.ChunkRef, n)
	for i := 0; i < n; i++ {
		entry := buf[i*entrySize : (i+1)*entrySize]
		var blockOff, uncompOff int64
		pos := 0
		switch {
		case zstd && variable:
			blockOff = int64(binary.LittleEndian.Uint64(entry[0:8]))
			uncompOff = int64(binary.LittleEndian.Uint64(entry[8:16]))
			pos = 16
		case zstd && !variable:
			blockOff = int64(binary.LittleEndian.Uint64(entry[0:8]))
			uncompOff = int64(i) * int64(hdr.ChunkSize)
			pos = 8
		case !zstd && variable:
			blockOff = int64(binary.LittleEndian.Uint64(entry[0:8]))
			uncompOff = blockOff
			pos = 8
		default: // raw fixed-size
			blockOff = int64(i) * int64(hdr.ChunkSize)
			uncompOff = blockOff
		}
		if hasWeight {
			pos++
		}
		var chunkDigest digest.Digest
		if hashed {
			sum := entry[pos : pos+int(hdr.HashSize)]
			alg := digestAlgorithmForHash(hdr.HashAlgo, hdr.HashSize)
			if alg == "" {
				return nil, nil, fmt.Errorf("content/index: unsupported HashAlgo=%d HashSize=%d", hdr.HashAlgo, hdr.HashSize)
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
	if !zstd && !variable { // raw fixed-size: BlockOffset is implicit
		for i := range chunks {
			start := int64(i) * int64(hdr.ChunkSize)
			end := start + int64(hdr.ChunkSize)
			if end > totalImageData {
				end = totalImageData
			}
			chunks[i].OnBlobStart = start
			chunks[i].OnBlobEnd = end
			chunks[i].Length = end - start
		}
	} else {
		for i := range chunks {
			var endOnBlob int64
			if i+1 < len(chunks) {
				endOnBlob = chunks[i+1].OnBlobStart
			} else {
				endOnBlob = chunkIndexStart
			}
			chunks[i].OnBlobEnd = endOnBlob
			var nextLogical int64
			if i+1 < len(chunks) {
				nextLogical = chunks[i+1].Offset
			} else {
				nextLogical = totalImageData
			}
			chunks[i].Length = nextLogical - chunks[i].Offset
		}
	}
	if err := validateChunks(chunks, hdr, totalImageData, chunkIndexStart, zstd); err != nil {
		return nil, nil, err
	}
	return chunks, hdr, nil
}

// parseChunkIndexPayload parses a chunk-index from its raw payload bytes —
// the 24-byte header followed by N chunk entries, without any surrounding
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
	// parseChunkIndex uses loc.HeaderOffset for reading the header and
	// entries (set to 0: the payload starts with the header), and uses
	// loc.Offset as chunkIndexStart for on-blob range validation (set to
	// blobSectionOffset: the original position in the blob).
	//
	// For variable-size chunk count, chunkCount() computes:
	//   payload_len = loc.End - loc.Offset [- 8 for +zstd]
	//   N = (payload_len - 24) / entrySize
	//
	// We need payload_len == len(payload), so:
	//   +zstd: loc.End = loc.Offset + len(payload) + 8
	//   raw:   loc.End = loc.Offset + len(payload)
	var end int64
	switch mediaType {
	case contentindex.MediaTypeEROFSLayerZstd,
		contentindex.MediaTypeEROFSLayerMergedZstd,
		contentindex.MediaTypeEROFSLayerDataZstd:
		end = blobSectionOffset + int64(len(payload)) + zstdSkippableFrameHeaderSize
	default:
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

// readChunkIndexHeader reads and validates the 24-byte header at headerOffset.
func readChunkIndexHeader(r io.ReaderAt, headerOffset int64) (*chunkIndexHeader, error) {
	var raw [chunkIndexHeaderSize]byte
	if _, err := r.ReadAt(raw[:], headerOffset); err != nil {
		return nil, fmt.Errorf("content/index: read chunk index header: %w", err)
	}
	magic := binary.LittleEndian.Uint32(raw[0:4])
	if magic != chunkIndexMagic {
		return nil, fmt.Errorf("content/index: bad chunk index magic 0x%08x", magic)
	}
	version := binary.LittleEndian.Uint32(raw[4:8])
	if version != chunkIndexVersion {
		return nil, fmt.Errorf("content/index: unsupported chunk index version %d", version)
	}
	hdr := &chunkIndexHeader{
		UncompressedSize: binary.LittleEndian.Uint64(raw[8:16]),
		ChunkSize:        binary.LittleEndian.Uint32(raw[16:20]),
		HashAlgo:         raw[20],
		HashSize:         raw[21],
		Flags:            raw[22],
	}
	if hdr.Flags&^chunkIndexFlagWeight != 0 {
		return nil, fmt.Errorf("content/index: reserved flags bits set: 0x%02x", hdr.Flags)
	}
	if raw[23] != 0 {
		return nil, fmt.Errorf("content/index: reserved header byte non-zero: 0x%02x", raw[23])
	}
	if hdr.HashAlgo == chunkIndexHashAlgoNone && hdr.HashSize != 0 {
		return nil, errors.New("content/index: HashSize must be 0 when HashAlgo is 0")
	}
	return hdr, nil
}

// chunkCount returns the number of chunk entries described by the header.
func chunkCount(loc *indexLocation, hdr *chunkIndexHeader, mediaType string, entrySize int) (int, error) {
	if hdr.ChunkSize > 0 {
		if hdr.UncompressedSize == 0 {
			return 0, nil
		}
		n := (int64(hdr.UncompressedSize) + int64(hdr.ChunkSize) - 1) / int64(hdr.ChunkSize)
		return int(n), nil
	}
	if loc.End == 0 {
		return 0, fmt.Errorf("content/index: variable-size chunk index requires end offset in %s annotation", contentindex.AnnotationIndexRange)
	}
	payload := loc.End - loc.Offset
	switch mediaType {
	case contentindex.MediaTypeEROFSLayerZstd,
		contentindex.MediaTypeEROFSLayerMergedZstd,
		contentindex.MediaTypeEROFSLayerDataZstd:
		payload -= zstdSkippableFrameHeaderSize
	}
	payload -= chunkIndexHeaderSize
	if payload < 0 {
		return 0, fmt.Errorf("content/index: chunk index payload negative (range too small)")
	}
	if entrySize == 0 {
		return 0, fmt.Errorf("content/index: variable-size mode requires non-zero entry size")
	}
	if payload%int64(entrySize) != 0 {
		return 0, fmt.Errorf("content/index: chunk index payload %d not a multiple of entry size %d", payload, entrySize)
	}
	return int(payload / int64(entrySize)), nil
}

// validateChunks performs sanity checks on the parsed chunk slice.
func validateChunks(chunks []contentindex.ChunkRef, hdr *chunkIndexHeader, imageDataSize, chunkIndexStart int64, zstd bool) error {
	for i, c := range chunks {
		if c.OnBlobStart < 0 || c.OnBlobEnd < c.OnBlobStart {
			return fmt.Errorf("content/index: chunk %d has invalid on-blob range [%d, %d)", i, c.OnBlobStart, c.OnBlobEnd)
		}
		if zstd {
			if c.OnBlobEnd > chunkIndexStart {
				return fmt.Errorf("content/index: chunk %d on-blob end %d overlaps chunk index at %d", i, c.OnBlobEnd, chunkIndexStart)
			}
		} else if c.OnBlobEnd > imageDataSize {
			return fmt.Errorf("content/index: chunk %d on-blob end %d exceeds image data size %d", i, c.OnBlobEnd, imageDataSize)
		}
		if c.Length < 0 {
			return fmt.Errorf("content/index: chunk %d has negative logical length %d", i, c.Length)
		}
		if i > 0 && chunks[i].OnBlobStart < chunks[i-1].OnBlobEnd {
			return fmt.Errorf("content/index: chunk %d on-blob range overlaps previous chunk", i)
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
func isZstdMediaType(mediaType string) bool {
	switch mediaType {
	case contentindex.MediaTypeEROFSLayerZstd,
		contentindex.MediaTypeEROFSLayerMergedZstd,
		contentindex.MediaTypeEROFSLayerDataZstd:
		return true
	}
	return false
}
