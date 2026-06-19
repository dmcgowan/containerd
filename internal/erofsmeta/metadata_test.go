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

package erofsmeta

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

// buildSyntheticSuperblock returns a byte slice positioned so that
// reading SuperBlockSize bytes at SuperBlockOffset extracts the magic,
// blkszbits, blocks, meta_blkaddr we set.  `inos` is left zero;
// tests that exercise the inos-derived ceiling use
// buildSyntheticSuperblockWithInos below.
func buildSyntheticSuperblock(t *testing.T, magic uint32, blkSzBits byte, totalBlocks, metaBlkAddr uint32) io.ReaderAt {
	t.Helper()
	return buildSyntheticSuperblockWithInos(t, magic, blkSzBits, totalBlocks, metaBlkAddr, 0)
}

func buildSyntheticSuperblockWithInos(t *testing.T, magic uint32, blkSzBits byte, totalBlocks, metaBlkAddr uint32, inos uint64) io.ReaderAt {
	t.Helper()
	buf := make([]byte, SuperBlockOffset+SuperBlockSize)
	binary.LittleEndian.PutUint32(buf[SuperBlockOffset+0:], magic)
	buf[SuperBlockOffset+12] = blkSzBits
	binary.LittleEndian.PutUint64(buf[SuperBlockOffset+16:], inos)
	binary.LittleEndian.PutUint32(buf[SuperBlockOffset+36:], totalBlocks)
	binary.LittleEndian.PutUint32(buf[SuperBlockOffset+40:], metaBlkAddr)
	return bytes.NewReader(buf)
}

func TestMetadataRange_validSuperblock(t *testing.T) {
	// 4 KiB blocks, 100 total blocks, meta_blkaddr at block 80.
	// fromAddr = (100-80) * 4096 = 81920.
	// MinMetadataLength floor would push length to 1 MiB, BUT the
	// final image-bounds clamp pulls it back to fit inside the
	// image: off + length must be ≤ totalBlocks*blkSize = 400 KiB,
	// so length = 400 KiB - 327680 = 81920.
	r := buildSyntheticSuperblock(t, MagicNumber, 12, 100, 80)
	off, length, err := MetadataRange(r)
	if err != nil {
		t.Fatalf("MetadataRange: %v", err)
	}
	if off != 80*4096 {
		t.Errorf("off = %d, want %d", off, 80*4096)
	}
	const want = int64(81920)
	if length != want {
		t.Errorf("length = %d, want %d (image-bounds clamp)", length, want)
	}
}

func TestMetadataRange_largeImage(t *testing.T) {
	// 4 KiB blocks, 2M blocks (8 GiB image), meta_blkaddr at block 1.5M.
	// fromAddr = (2M - 1.5M) * 4096 = 2 GiB.  With MaxMetadataLength
	// = 128 MiB (the absolute pre-fill ceiling), the returned length
	// must be clamped to 128 MiB even though the formal "metadata
	// extends to end of image" extent is much larger.  This bound is
	// what makes the SB+metadata pre-fill safe regardless of the
	// image's physical layout.
	const blkSize = 4096
	const totalBlocks = uint32(2 << 20)
	const metaBlkAddr = uint32(1572864)
	r := buildSyntheticSuperblock(t, MagicNumber, 12, totalBlocks, metaBlkAddr)
	off, length, err := MetadataRange(r)
	if err != nil {
		t.Fatalf("MetadataRange: %v", err)
	}
	if want := int64(metaBlkAddr) * blkSize; off != want {
		t.Errorf("off = %d, want %d", off, want)
	}
	if length != MaxMetadataLength {
		t.Errorf("length = %d, want MaxMetadataLength = %d", length, MaxMetadataLength)
	}
}

func TestMetadataRange_badMagic(t *testing.T) {
	r := buildSyntheticSuperblock(t, 0xDEADBEEF, 12, 100, 80)
	off, length, err := MetadataRange(r)
	if err == nil {
		t.Fatal("expected error for bad magic")
	}
	if off != 0 || length != 0 {
		t.Errorf("want zero range on bad magic, got (%d,%d)", off, length)
	}
}

func TestMetadataRange_outOfRangeBlkBits(t *testing.T) {
	for _, bits := range []byte{0, 1, 8, 17, 32, 255} {
		r := buildSyntheticSuperblock(t, MagicNumber, bits, 100, 80)
		off, length, err := MetadataRange(r)
		if err == nil {
			t.Errorf("blkszbits=%d: expected error", bits)
		}
		if off != 0 || length != 0 {
			t.Errorf("blkszbits=%d: want zero range, got (%d,%d)", bits, off, length)
		}
	}
}

func TestMetadataRange_metaBlkAddrOutOfBounds(t *testing.T) {
	// meta_blkaddr >= total_blocks ⇒ unparseable.
	r := buildSyntheticSuperblock(t, MagicNumber, 12, 100, 100)
	_, _, err := MetadataRange(r)
	if err == nil {
		t.Fatal("expected error when meta_blkaddr >= blocks")
	}
	// meta_blkaddr = 0 ⇒ no metadata region.
	r = buildSyntheticSuperblock(t, MagicNumber, 12, 100, 0)
	_, _, err = MetadataRange(r)
	if err == nil {
		t.Fatal("expected error when meta_blkaddr is zero")
	}
}

func TestMetadataRange_nilReader(t *testing.T) {
	_, _, err := MetadataRange(nil)
	if err == nil {
		t.Fatal("expected error for nil reader")
	}
}

// truncatedReader returns short reads — exercises the error path.
type truncatedReader struct{ n int }

func (t *truncatedReader) ReadAt(p []byte, off int64) (int, error) {
	if t.n >= len(p) {
		return len(p), nil
	}
	return t.n, io.ErrUnexpectedEOF
}

func TestMetadataRange_shortRead(t *testing.T) {
	_, _, err := MetadataRange(&truncatedReader{n: 3})
	if err == nil {
		t.Fatal("expected error on short read")
	}
}

// TestMetadataRange_metadataFirst_bounded simulates a metadata-first
// EROFS image: meta_blkaddr sits at block 1 (just past the SB area)
// while the image as a whole is 64 MiB.  Before the inos-derived
// ceiling existed, MetadataRange returned `(totalBlocks-1) * blkSize`
// = ~64 MiB, which the block-mount handler would then EnsureRange
// at PriorityForeground SEQUENTIALLY across every chunk — defeating
// lazy loading.  With `inos` set to a realistic value, the returned
// length must be bounded by `inos * PerInodeBudget`.
func TestMetadataRange_metadataFirst_bounded(t *testing.T) {
	const blkSize = int64(4096)
	const totalBlocks = uint32(16384) // 64 MiB image
	const metaBlkAddr = uint32(1)
	const inos = uint64(10000) // realistic for a small container layer
	r := buildSyntheticSuperblockWithInos(t, MagicNumber, 12, totalBlocks, metaBlkAddr, inos)
	off, length, err := MetadataRange(r)
	if err != nil {
		t.Fatalf("MetadataRange: %v", err)
	}
	if off != int64(metaBlkAddr)*blkSize {
		t.Errorf("off = %d, want %d", off, int64(metaBlkAddr)*blkSize)
	}
	want := int64(inos) * PerInodeBudget
	if want < MinMetadataLength {
		want = MinMetadataLength
	}
	if length != want {
		t.Errorf("length = %d, want %d (inos-derived ceiling)", length, want)
	}
	// Critical: length must be MUCH less than the address-derived
	// bound, otherwise lazy loading is broken.
	addrBound := (int64(totalBlocks) - int64(metaBlkAddr)) * blkSize
	if length >= addrBound/4 {
		t.Errorf("length = %d is too close to addr bound %d — inos ceiling not effective", length, addrBound)
	}
}

// TestMetadataRange_metadataFirst_largeInos verifies the absolute
// MaxMetadataLength ceiling kicks in for pathologically large
// `inos` values (corrupt or adversarial SB).
func TestMetadataRange_metadataFirst_largeInos(t *testing.T) {
	const totalBlocks = uint32(1 << 24) // ~64 GiB at 4 KiB blocks
	const metaBlkAddr = uint32(1)
	const inos = uint64(1 << 30) // 1 billion inodes — would yield 256 GiB without the cap
	r := buildSyntheticSuperblockWithInos(t, MagicNumber, 12, totalBlocks, metaBlkAddr, inos)
	_, length, err := MetadataRange(r)
	if err != nil {
		t.Fatalf("MetadataRange: %v", err)
	}
	if length > MaxMetadataLength {
		t.Errorf("length = %d exceeds MaxMetadataLength = %d", length, MaxMetadataLength)
	}
}

// TestMetadataRange_dataFirst_inosDoesNotInflate confirms that
// setting `inos` on a data-first SB does NOT increase the metadata
// length beyond the address-derived bound (the min() of the two
// must always be ≤ fromAddr).
func TestMetadataRange_dataFirst_inosDoesNotInflate(t *testing.T) {
	const blkSize = int64(4096)
	const totalBlocks = uint32(100)
	const metaBlkAddr = uint32(80)
	const inos = uint64(1000000) // would imply 256 MiB but addr bound is much smaller
	r := buildSyntheticSuperblockWithInos(t, MagicNumber, 12, totalBlocks, metaBlkAddr, inos)
	_, length, err := MetadataRange(r)
	if err != nil {
		t.Fatalf("MetadataRange: %v", err)
	}
	// fromAddr = 20 blocks = 80 KiB, floored up to MinMetadataLength.
	if length > MinMetadataLength {
		t.Errorf("length = %d exceeds expected floor %d", length, MinMetadataLength)
	}
	// And the final clamp must keep us inside the image.
	imgBytes := int64(totalBlocks) * blkSize
	if int64(metaBlkAddr)*blkSize+length > imgBytes {
		t.Errorf("range exceeds image bounds: off+len > %d", imgBytes)
	}
}
