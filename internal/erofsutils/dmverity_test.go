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

package erofsutils

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"strings"
	"testing"
)

// expectedTreeSize computes the on-disk size of a no-superblock dm-verity
// hash tree for nDataBlocks leaf blocks at the given block size, using the
// same bottom-up folding rule as VerityWriter.Flush: each level packs the
// previous level's per-block SHA-256 hashes and zero-pads to a block
// multiple; folding stops when a level fits in one block.
func expectedTreeSize(nDataBlocks, blockSize int) int {
	const digest = sha256.Size // 32
	hashesPerBlock := blockSize / digest
	total := 0
	n := nDataBlocks
	for {
		// blocks needed to hold n hashes
		blocks := (n + hashesPerBlock - 1) / hashesPerBlock
		total += blocks * blockSize
		if blocks <= 1 {
			break
		}
		n = blocks
	}
	return total
}

// TestNewVerityWriter_NoSuperblock asserts that the writer emits ONLY the
// merkle tree — no superblock block precedes it.  This is the core
// invariant of the no-superblock switch: the first byte written is the
// top hash-tree level, not a "verity\0\0" signature.
func TestNewVerityWriter_NoSuperblock(t *testing.T) {
	const blockSize = 4096
	const nBlocks = 16
	const dataSize = blockSize * nBlocks

	var buf bytes.Buffer
	vw, err := NewVerityWriter(&buf, int64(dataSize), blockSize)
	if err != nil {
		t.Fatalf("NewVerityWriter: %v", err)
	}
	// Nothing is written on construction (no superblock).
	if buf.Len() != 0 {
		t.Fatalf("expected 0 bytes written on construction, got %d", buf.Len())
	}

	data := make([]byte, dataSize)
	for i := range data {
		data[i] = byte(i)
	}
	// Mirror the production pattern: the data bytes are already on disk,
	// so the write pass is teed to io.Discard and only the tree lands in
	// buf via Flush.  (Feeding data through the live writer would also
	// copy the data into buf, which is not what we're measuring here.)
	vw.w = io.Discard
	if _, err := vw.Write(data); err != nil {
		t.Fatalf("Write: %v", err)
	}
	vw.w = &buf
	res, err := vw.Flush()
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// The output must be exactly the tree — no superblock signature.
	out := buf.Bytes()
	if bytes.HasPrefix(out, []byte("verity\x00\x00")) {
		t.Errorf("output begins with a verity superblock signature; expected tree-only, no-superblock output")
	}
	if want := expectedTreeSize(nBlocks, blockSize); len(out) != want {
		t.Errorf("tree size = %d, want %d (no-superblock)", len(out), want)
	}

	// HashOffset must equal the data size and point directly at the tree.
	if res.HashOffset != dataSize {
		t.Errorf("HashOffset = %d, want %d", res.HashOffset, dataSize)
	}
	if res.BlockSize != blockSize {
		t.Errorf("BlockSize = %d, want %d", res.BlockSize, blockSize)
	}
	if !strings.HasPrefix(res.RootDigest, "sha256:") {
		t.Errorf("RootDigest = %q, want sha256: prefix", res.RootDigest)
	}
}

// TestAppendDmVerity_NoSuperblockLayout covers the in-memory convenience
// path.  The combined output must be exactly [data][tree] with the tree
// starting at HashOffset == len(data) — no superblock in between.
func TestAppendDmVerity_NoSuperblockLayout(t *testing.T) {
	const blockSize = 4096
	const nBlocks = 32
	const dataSize = blockSize * nBlocks

	data := make([]byte, dataSize)
	for i := range data {
		data[i] = byte(i % 251)
	}

	combined, res, err := AppendDmVerity(context.Background(), data, blockSize)
	if err != nil {
		t.Fatalf("AppendDmVerity: %v", err)
	}
	if res.HashOffset != dataSize {
		t.Errorf("HashOffset = %d, want %d", res.HashOffset, dataSize)
	}
	// Data section is preserved verbatim.
	if !bytes.Equal(combined[:dataSize], data) {
		t.Errorf("data section altered by AppendDmVerity")
	}
	// The byte at HashOffset is the start of the tree, not a superblock.
	tree := combined[res.HashOffset:]
	if bytes.HasPrefix(tree, []byte("verity\x00\x00")) {
		t.Errorf("region at HashOffset begins with a superblock signature; expected tree")
	}
	if want := expectedTreeSize(nBlocks, blockSize); len(tree) != want {
		t.Errorf("tree size = %d, want %d", len(tree), want)
	}
}

// TestAppendDmVerityStream_MatchesInMemory asserts the streaming path
// (used by the chunked converter) produces byte-identical output to the
// in-memory path for the same input — same tree, same root hash, same
// no-superblock layout.
func TestAppendDmVerityStream_MatchesInMemory(t *testing.T) {
	const blockSize = 4096
	const nBlocks = 48
	const dataSize = blockSize * nBlocks

	data := make([]byte, dataSize)
	for i := range data {
		data[i] = byte((i * 7) % 253)
	}

	// In-memory reference.
	combined, memRes, err := AppendDmVerity(context.Background(), data, blockSize)
	if err != nil {
		t.Fatalf("AppendDmVerity: %v", err)
	}

	// Streaming: write data into a buffer, then append the tree.
	backing := make([]byte, len(combined)+blockSize) // slack
	copy(backing, data)
	rw := &bufferAt{buf: backing}
	streamRes, err := AppendDmVerityStream(context.Background(), rw, rw, int64(dataSize), blockSize)
	if err != nil {
		t.Fatalf("AppendDmVerityStream: %v", err)
	}

	if memRes.RootDigest != streamRes.RootDigest {
		t.Errorf("root digest mismatch: mem=%s stream=%s", memRes.RootDigest, streamRes.RootDigest)
	}
	if memRes.HashOffset != streamRes.HashOffset {
		t.Errorf("hash offset mismatch: mem=%d stream=%d", memRes.HashOffset, streamRes.HashOffset)
	}
	// The streamed [data][tree] region must equal the in-memory combined.
	if !bytes.Equal(backing[:len(combined)], combined) {
		t.Errorf("streamed output differs from in-memory output")
	}
}

// TestNewVerityWriter_BadBlockSize verifies the block-size validation
// that replaced the vendored verity.IsBlockSizeValid dependency.
func TestNewVerityWriter_BadBlockSize(t *testing.T) {
	for _, bs := range []uint32{100, 3000, 1024*1024 + 1} {
		if _, err := NewVerityWriter(&bytes.Buffer{}, 4096, bs); err == nil {
			t.Errorf("block size %d should be rejected", bs)
		}
	}
	// Valid power-of-two sizes are accepted.
	for _, bs := range []uint32{512, 4096, 65536, 524288} {
		if _, err := NewVerityWriter(&bytes.Buffer{}, int64(bs), bs); err != nil {
			t.Errorf("block size %d should be accepted, got %v", bs, err)
		}
	}
}

// bufferAt is a minimal io.WriteSeeker+ReaderAt over a fixed-size byte
// slice — used to drive AppendDmVerityStream without touching the
// filesystem.
type bufferAt struct {
	buf []byte
	pos int64
}

func (b *bufferAt) Write(p []byte) (int, error) {
	n := copy(b.buf[b.pos:], p)
	b.pos += int64(n)
	return n, nil
}

func (b *bufferAt) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case 0:
		b.pos = off
	case 1:
		b.pos += off
	case 2:
		b.pos = int64(len(b.buf)) + off
	}
	return b.pos, nil
}

func (b *bufferAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(b.buf)) {
		return 0, nil
	}
	n := copy(p, b.buf[off:])
	return n, nil
}
