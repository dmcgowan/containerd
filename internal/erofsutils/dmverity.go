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

// Package erofsutils provides utilities for building and inspecting EROFS images.
//
// This file implements streaming dm-verity hash-tree generation.
// Because the data size is known upfront, the superblock (which only needs
// DataBlocks = dataSize/blockSize) can be written immediately to the output
// stream before any data is processed. The data then flows in through a
// LimitReader and out to the writer while leaf hashes are accumulated in
// memory. When the reader returns EOF the tree levels are folded upward and
// written, producing the root hash.
//
// The on-disk layout written to the output is:
//
//	[superblock padded to blockSize][hash level 0][hash level 1]...
//
// The caller writes the EROFS data to a separate writer (or has already done
// so); this writer only emits the superblock and hash tree, starting at
// hash_offset. To use it in a pipeline:
//
//	1. Write EROFS data to the underlying output.
//	2. Pipe the same data through VerityWriter to accumulate hashes.
//	3. Call Flush() once all data bytes have been written; it appends
//	   superblock + tree to the underlying output and returns the root hash.
//
// The superblock uses the go-dmverity verity.Superblock type so that the
// on-disk layout is guaranteed to match the kernel dm-verity target:
//
//	Offset  Size  Field
//	     0     8  Signature  "verity\0\0"
//	     8     4  Version    1
//	    12     4  HashType   1  (SHA-256(salt||block); SaltSize=0 → no salt)
//	    16    16  UUID       (zero)
//	    32    32  Algorithm  "sha256\0..."
//	    64     4  DataBlockSize
//	    68     4  HashBlockSize
//	    72     8  DataBlocks
//	    80     2  SaltSize   0
//	    82     6  Pad1       (zero)
//	    88   256  Salt       (zero)
//	   344   168  Pad2       (zero)
//	   512        total
package erofsutils

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/containerd/go-dmverity/pkg/verity"
	"github.com/containerd/log"
)

// DmVerityResult holds the parameters of a generated dm-verity merkle tree.
type DmVerityResult struct {
	// HashOffset is the byte offset in the combined stream where the
	// superblock (and hash tree) begins — i.e. the original data size.
	// This is the value used for the org.erofs.dmverity.hash_offset annotation.
	HashOffset int64

	// RootDigest is the SHA-256 root hash, prefixed with "sha256:".
	RootDigest string

	// BlockSize is the dm-verity data and hash block size in bytes.
	BlockSize uint32
}

// VerityWriter accumulates dm-verity leaf hashes as data is written through it.
// The superblock is written to the output immediately on construction (since
// DataBlocks is known from dataSize). After all data bytes have been written,
// call Flush() to write the hash tree levels and obtain the root hash.
//
// Data passthrough: every byte written to VerityWriter is teed to the
// underlying io.Writer so it can be used inline in a copy pipeline.
//
// Typical usage:
//
//	vw, result, err := NewVerityWriter(dst, dataSize, blockSize)
//	io.Copy(vw, limitedSrc)   // streams data to dst; hashes each block
//	rootDigest, err := vw.Flush() // writes tree to dst; returns root hash
type VerityWriter struct {
	w         io.Writer
	blockSize int
	dataSize  int64

	blockBuf    []byte
	blockPos    int
	bytesHashed int64
	leafHashes  [][]byte
}

// NewVerityWriter creates a VerityWriter. It immediately writes the padded
// superblock (blockSize bytes) to w, then returns. Callers should write
// exactly dataSize bytes through the returned writer before calling Flush.
//
// dataSize must be a positive multiple of blockSize.
// blockSize must be a power of two between 512 and 524288; pass 0 for 4096.
func NewVerityWriter(w io.Writer, dataSize int64, blockSize uint32) (*VerityWriter, error) {
	if blockSize == 0 {
		blockSize = 4096
	}
	if !verity.IsBlockSizeValid(blockSize) {
		return nil, fmt.Errorf("dmverity: invalid block size %d", blockSize)
	}
	if dataSize <= 0 {
		return nil, fmt.Errorf("dmverity: dataSize must be > 0, got %d", dataSize)
	}
	if dataSize%int64(blockSize) != 0 {
		return nil, fmt.Errorf("dmverity: dataSize %d is not a multiple of blockSize %d",
			dataSize, blockSize)
	}

	nDataBlocks := uint64(dataSize) / uint64(blockSize)

	// Build and write the superblock immediately — it only needs DataBlocks,
	// which we know from dataSize. The root hash is NOT stored in the
	// superblock (it is supplied to the kernel at activation time via the
	// veritysetup open command or the device-mapper table).
	sb := verity.DefaultSuperblock()
	sb.DataBlockSize = blockSize
	sb.HashBlockSize = blockSize
	sb.DataBlocks = nDataBlocks
	sb.SaltSize = 0
	// UUID left zero; salt left zero (no salt → blockHash = SHA-256(block)).

	sbBytes, err := sb.Serialize()
	if err != nil {
		return nil, fmt.Errorf("dmverity: serialize superblock: %w", err)
	}

	// Pad superblock to blockSize (it is 512 bytes; for 4 KiB blocks this
	// adds 3584 zero bytes so the hash tree starts on a block boundary).
	sbPadded := make([]byte, blockSize)
	copy(sbPadded, sbBytes)
	if _, err := w.Write(sbPadded); err != nil {
		return nil, fmt.Errorf("dmverity: write superblock: %w", err)
	}

	return &VerityWriter{
		w:          w,
		blockSize:  int(blockSize),
		dataSize:   dataSize,
		blockBuf:   make([]byte, blockSize),
		leafHashes: make([][]byte, 0, int(nDataBlocks)),
	}, nil
}

// Write tees p to the underlying writer and hashes completed blocks.
// Returns an error if more than dataSize bytes are written in total.
func (vw *VerityWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		if vw.bytesHashed+int64(vw.blockPos)+int64(len(p)) > vw.dataSize {
			return written, fmt.Errorf("dmverity: write would exceed declared dataSize %d", vw.dataSize)
		}
		n := copy(vw.blockBuf[vw.blockPos:], p)
		if _, err := vw.w.Write(vw.blockBuf[vw.blockPos : vw.blockPos+n]); err != nil {
			return written, fmt.Errorf("dmverity: passthrough write: %w", err)
		}
		vw.blockPos += n
		written += n
		p = p[n:]
		if vw.blockPos == vw.blockSize {
			vw.leafHashes = append(vw.leafHashes, blockHash(vw.blockBuf))
			vw.bytesHashed += int64(vw.blockSize)
			vw.blockPos = 0
		}
	}
	return written, nil
}

// Flush finalises the hash tree and writes all hash-tree levels to the
// underlying writer. It must be called after exactly dataSize bytes have been
// written. Returns the DmVerityResult containing the root digest and the
// hash_offset annotation value.
func (vw *VerityWriter) Flush() (*DmVerityResult, error) {
	total := vw.bytesHashed + int64(vw.blockPos)
	if total != vw.dataSize {
		return nil, fmt.Errorf("dmverity: Flush called after %d bytes but dataSize is %d",
			total, vw.dataSize)
	}
	if vw.blockPos != 0 {
		// Partial block — shouldn't happen since dataSize is a multiple of blockSize.
		return nil, fmt.Errorf("dmverity: %d bytes remain in partial block at Flush", vw.blockPos)
	}
	if len(vw.leafHashes) == 0 {
		return nil, fmt.Errorf("dmverity: no data blocks hashed")
	}

	hashOffset := vw.dataSize // hash_offset = data size (annotation value)
	digestSize := sha256.Size

	// Build hash tree bottom-up.
	// levels[0] = leaf hashes packed and padded to blockSize multiples.
	// levels[k] = hashes of levels[k-1] blocks, padded to blockSize.
	// Stop when a level fits in a single block.
	levels := [][]byte{packAndPad(vw.leafHashes, vw.blockSize)}
	for {
		prev := levels[len(levels)-1]
		nBlocks := len(prev) / vw.blockSize
		if nBlocks <= 1 {
			break
		}
		next := make([]byte, 0, nBlocks*digestSize)
		for i := 0; i < nBlocks; i++ {
			blk := prev[i*vw.blockSize : (i+1)*vw.blockSize]
			next = append(next, blockHash(blk)...)
		}
		levels = append(levels, padToBlock(next, vw.blockSize))
	}

	// Root hash = SHA-256 of the single block at the top level.
	rootHash := blockHash(levels[len(levels)-1][:vw.blockSize])

	// Write hash tree levels top-down: highest level first (fewest hashes),
	// leaf level (level 0) last. This matches the layout produced by
	// veritysetup(8) and expected by the kernel dm-verity target.
	for i := len(levels) - 1; i >= 0; i-- {
		if _, err := vw.w.Write(levels[i]); err != nil {
			return nil, fmt.Errorf("dmverity: write hash level: %w", err)
		}
	}

	return &DmVerityResult{
		HashOffset: hashOffset,
		RootDigest: "sha256:" + hex.EncodeToString(rootHash),
		BlockSize:  uint32(vw.blockSize),
	}, nil
}

// blockHash returns SHA-256(block) with no salt.
// hash_type=1 with SaltSize=0 reduces to SHA-256(block).
func blockHash(block []byte) []byte {
	h := sha256.Sum256(block)
	return h[:]
}

// packAndPad packs a slice of equal-size hashes into a flat byte slice and
// zero-pads it to the next blockSize multiple.
func packAndPad(hashes [][]byte, blockSize int) []byte {
	packed := make([]byte, len(hashes)*sha256.Size)
	for i, h := range hashes {
		copy(packed[i*sha256.Size:], h)
	}
	return padToBlock(packed, blockSize)
}

// padToBlock zero-pads data to the next blockSize multiple. Returns data
// unchanged if it is already aligned.
func padToBlock(data []byte, blockSize int) []byte {
	rem := len(data) % blockSize
	if rem == 0 {
		return data
	}
	return append(data, make([]byte, blockSize-rem)...)
}

// AppendDmVerity is a convenience wrapper for callers that have the entire
// EROFS image in memory. It streams the data through VerityWriter without
// any intermediate temp file or external binary and returns:
//
//   - combined: [erofsData][superblock padded][hash tree levels]
//   - result:   DmVerityResult with HashOffset and RootDigest
func AppendDmVerity(ctx context.Context, erofsData []byte, blockSize uint32) ([]byte, *DmVerityResult, error) {
	if blockSize == 0 {
		blockSize = 4096
	}

	dataSize := int64(len(erofsData))

	// Pre-allocate output: erofsData + estimate for superblock+tree.
	// Tree ≈ dataSize / (blockSize/32) bytes; superblock = blockSize.
	treeEstimate := int64(blockSize) + dataSize/(int64(blockSize)/32) + int64(blockSize)
	out := make([]byte, 0, dataSize+treeEstimate)
	out = append(out, erofsData...)

	// The VerityWriter writes superblock+tree into appendW (appended after erofsData).
	appendW := &sliceAppender{dst: &out}

	vw, err := NewVerityWriter(appendW, dataSize, blockSize)
	if err != nil {
		return nil, nil, err
	}

	// Feed data for hashing. Passthrough goes to appendW (which appends to out),
	// but we only want the data once (already in out). Use io.Discard for the
	// passthrough and only let Close/Flush write the tree portion.
	//
	// Override the passthrough to Discard before writing, then restore for Flush.
	vw.w = io.Discard
	if _, err := vw.Write(erofsData); err != nil {
		return nil, nil, fmt.Errorf("dmverity: hash data: %w", err)
	}

	// Restore writer for Flush so the hash tree goes into our output buffer.
	vw.w = appendW
	result, err := vw.Flush()
	if err != nil {
		return nil, nil, fmt.Errorf("dmverity: flush: %w", err)
	}

	log.G(ctx).Infof("dmverity: hash_offset=%d root_digest=%s tree_size=%d",
		result.HashOffset, result.RootDigest, int64(len(out))-result.HashOffset)
	return out, result, nil
}

// sliceAppender appends written bytes to *dst.
type sliceAppender struct{ dst *[]byte }

func (a *sliceAppender) Write(p []byte) (int, error) {
	*a.dst = append(*a.dst, p...)
	return len(p), nil
}
