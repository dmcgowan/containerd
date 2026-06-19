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

// Package erofsmeta exposes a minimal EROFS superblock parser sufficient
// to derive the byte range of the inode-table (metadata) region of an
// EROFS image, without requiring a kernel mount or the full go-erofs
// reader.
//
// Both the daemon block-mount handler (for the lazy SB+metadata pre-fill
// that precedes a kernel mount) and the lazy pull path (for synchronously
// warming the metadata region into the sparse cache file so client-side
// go-erofs reads from /etc/passwd, /etc/group, etc. find resident pages)
// consume this helper.  Keeping the parser here, behind a small
// io.ReaderAt-shaped API, keeps EROFS knowledge confined to one file and
// lets the cache and pull layers stay format-agnostic.
package erofsmeta

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Layout constants from the EROFS on-disk format (Linux kernel
// fs/erofs/erofs_fs.h).  Only the fields we need to compute the metadata
// region are pulled out here; the rest of the superblock is intentionally
// not modeled.
const (
	// SuperBlockOffset is the byte offset at which the superblock starts
	// inside the EROFS image.  File-backed images put it at the same
	// offset as block-device images so a single value works for both.
	SuperBlockOffset = 1024

	// SuperBlockSize is the number of bytes we need to read from
	// SuperBlockOffset to extract the fields used by MetadataRange.
	SuperBlockSize = 64

	// MagicNumber is the EROFS superblock magic at offset 0 of the SB.
	MagicNumber uint32 = 0xE0F5E1E2

	// MinBlockBits / MaxBlockBits bracket the block-size exponent
	// (blkszbits) that EROFS supports.  Outside this range the SB is
	// almost certainly garbled — we refuse to treat the image as EROFS.
	MinBlockBits = 9
	MaxBlockBits = 16

	// MinMetadataLength is the floor we apply when the computed
	// metadata region happens to be tiny (rare; defends very small
	// images).  Lazy-warming a 1 MiB region is cheap and covers the
	// inode table + tail-packed dirents in the small-image case.
	MinMetadataLength = 1 * 1024 * 1024

	// MaxMetadataLength is the absolute hard ceiling on the
	// metadata region size we will pre-fill.  Defends against
	// pathological / corrupt SBs where the derived bounds would
	// otherwise drag us into fetching the entire image at
	// PriorityForeground before mount.  128 MiB covers tens of
	// millions of inodes at the upper bound of our per-inode
	// budget, which is far beyond any realistic container image.
	MaxMetadataLength = 128 * 1024 * 1024

	// PerInodeBudget is a generous overestimate of the on-disk
	// bytes consumed by a single inode in the EROFS inode table:
	// 64-byte extended inode core + ≤ 192 bytes of inline xattrs
	// and tail-packed data/dirents.  Most real inodes use far
	// less; this is sized so that `inos * PerInodeBudget` is a
	// SAFE UPPER BOUND on the inode-table extent for path-
	// resolution-grade metabuf reads.
	PerInodeBudget = 256
)

// MetadataRange reads the EROFS superblock from r and returns the byte
// offset and length of the inode-table (metadata) region.
//
// EROFS stores the inode table at meta_blkaddr * block_size, which for
// merged Docker layers typically sits near the END of the image (file
// data precedes the inodes).  Knowing this offset lets a caller pre-fill
// only the SB + metadata chunks so subsequent EROFS mount-time SB and
// root-inode reads — which bypass fsnotify hooks via the metabuf path —
// hit already-resident pages, and so client-side path-resolution reads
// of /etc/passwd / /etc/group via go-erofs succeed without a mount.
//
// Returns the empty range (0, 0) on any parse failure.  This is
// deliberately a soft error: callers tolerate it (they may fall back to
// EnsureAll, or to a full mount-based fsview View, depending on context).
// A non-nil err is returned alongside (0,0) so callers can log the
// reason if they wish.
func MetadataRange(r io.ReaderAt) (off, length int64, err error) {
	if r == nil {
		return 0, 0, fmt.Errorf("erofsmeta: nil ReaderAt")
	}

	buf := make([]byte, SuperBlockSize)
	if _, rerr := r.ReadAt(buf, SuperBlockOffset); rerr != nil {
		return 0, 0, fmt.Errorf("erofsmeta: read superblock: %w", rerr)
	}

	magic := binary.LittleEndian.Uint32(buf[0:4])
	if magic != MagicNumber {
		return 0, 0, fmt.Errorf("erofsmeta: bad magic 0x%x (want 0x%x)", magic, MagicNumber)
	}

	blkSizeBits := uint(buf[12])
	if blkSizeBits < MinBlockBits || blkSizeBits > MaxBlockBits {
		return 0, 0, fmt.Errorf("erofsmeta: unsupported blkszbits %d", blkSizeBits)
	}
	blkSize := int64(1) << blkSizeBits

	totalBlocks := int64(binary.LittleEndian.Uint32(buf[36:40]))
	metaBlkAddr := int64(binary.LittleEndian.Uint32(buf[40:44]))
	if metaBlkAddr <= 0 || totalBlocks <= metaBlkAddr {
		return 0, 0, fmt.Errorf("erofsmeta: meta_blkaddr=%d total_blocks=%d", metaBlkAddr, totalBlocks)
	}

	// Inode count (`inos`) — used to derive a TIGHT upper bound on
	// the inode-table extent that doesn't depend on the image's
	// physical layout.  This is the critical bound for
	// metadata-first images where (totalBlocks - metaBlkAddr) ≈
	// entire image; without it the pre-fill would foreground-fetch
	// the whole image before mount, defeating lazy loading.  We
	// clamp to int64 to keep arithmetic safe; the >>3 below
	// further guarantees inos*PerInodeBudget cannot overflow.
	inos := int64(binary.LittleEndian.Uint64(buf[16:24]))
	if inos < 0 {
		inos = 0
	}

	// fromAddr: classical "metadata extends to end of image" bound.
	// Correct (and tight) for data-first layouts where the inode
	// table is the last region; degenerate (= entire image) for
	// metadata-first layouts.
	fromAddr := (totalBlocks - metaBlkAddr) * blkSize

	// fromInos: layout-independent bound derived from inode count.
	// Tight for metadata-first; loose-but-safe for data-first
	// (we take the min of the two).  Guard the multiply against
	// overflow on absurd `inos` values.
	var fromInos int64
	if inos <= (int64(1)<<55)/PerInodeBudget {
		fromInos = inos * PerInodeBudget
	} else {
		fromInos = MaxMetadataLength
	}

	off = metaBlkAddr * blkSize
	length = fromAddr
	// inos == 0 is anomalous for a real EROFS image (the root
	// inode always exists), but synthetic or legacy SBs may leave
	// the field unset.  Only apply the inos-derived ceiling when
	// inos is positive; otherwise we keep the address-derived
	// bound exactly as before.
	if inos > 0 && fromInos < length {
		length = fromInos
	}
	if length < MinMetadataLength {
		length = MinMetadataLength
	}
	if length > MaxMetadataLength {
		length = MaxMetadataLength
	}
	// Final clamp: the metadata region must lie within the image.
	imgBytes := totalBlocks * blkSize
	if off+length > imgBytes {
		length = imgBytes - off
	}
	return off, length, nil
}
