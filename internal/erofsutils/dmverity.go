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
// This file implements streaming dm-verity hash-tree generation in
// no-superblock mode.  Because the data size is known upfront, leaf hashes
// can be accumulated as data flows through the writer; once all data bytes
// have been written the tree levels are folded upward and emitted, producing
// the root hash.
//
// The on-disk layout written to the output is:
//
//	[hash level N (root, 1 block)]...[hash level 1][hash level 0 (leaves)]
//
// i.e. the merkle tree ONLY — there is no on-disk dm-verity superblock.
// The device is always activated with `veritysetup --no-superblock`
// semantics: every parameter the superblock would have carried (block
// sizes, data-block count, algorithm, salt) is supplied out-of-band at
// activation time via the org.erofs.dmverity.* annotations / mount
// options.  See internal/dmverity.Open.  Dropping the superblock removes a
// self-description block we never read in this stack and avoids the
// superblock-UUID handling entirely.
//
// The caller writes the EROFS data to a separate writer (or has already done
// so); this writer only emits the hash tree, starting at hash_offset (=
// the EROFS data size). To use it in a pipeline:
//
//	1. Write EROFS data to the underlying output.
//	2. Pipe the same data through VerityWriter to accumulate hashes.
//	3. Call Flush() once all data bytes have been written; it appends the
//	   hash tree to the underlying output and returns the root hash.
package erofsutils

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/containerd/log"
)

// DmVerityResult holds the parameters of a generated dm-verity merkle tree.
type DmVerityResult struct {
	// HashOffset is the byte offset in the combined stream where the hash
	// tree begins — i.e. the original EROFS data size.  This is the value
	// used for the org.erofs.dmverity.hash_offset annotation.  In
	// no-superblock mode the tree begins directly at this offset (there is
	// no superblock block preceding it).
	HashOffset int64

	// RootDigest is the SHA-256 root hash, prefixed with "sha256:".
	RootDigest string

	// BlockSize is the dm-verity data and hash block size in bytes.
	BlockSize uint32
}

// VerityWriter accumulates dm-verity leaf hashes as data is written through it.
// No superblock is written; after all data bytes have been written, call
// Flush() to write the hash tree levels and obtain the root hash.
//
// Data passthrough: every byte written to VerityWriter is teed to the
// underlying io.Writer so it can be used inline in a copy pipeline.
//
// Typical usage:
//
//	vw, err := NewVerityWriter(dst, dataSize, blockSize)
//	io.Copy(vw, limitedSrc)   // streams data to dst; hashes each block
//	result, err := vw.Flush() // writes tree to dst; returns root hash
type VerityWriter struct {
	w         io.Writer
	blockSize int
	dataSize  int64

	blockBuf    []byte
	blockPos    int
	bytesHashed int64
	leafHashes  [][]byte
}

// isValidBlockSize reports whether n is a power of two in [512, 524288],
// matching the dm-verity block-size constraint.  Kept local so this file
// no longer needs the go-dmverity verity package (which was only used for
// the superblock we no longer write).
func isValidBlockSize(n uint32) bool {
	if n < 512 || n > 524288 {
		return false
	}
	return n&(n-1) == 0
}

// NewVerityWriter creates a VerityWriter for no-superblock dm-verity output.
// It writes nothing on construction; callers should write exactly dataSize
// bytes through the returned writer before calling Flush, which emits the
// hash tree.
//
// dataSize must be a positive multiple of blockSize.
// blockSize must be a power of two between 512 and 524288; pass 0 for 4096.
func NewVerityWriter(w io.Writer, dataSize int64, blockSize uint32) (*VerityWriter, error) {
	if blockSize == 0 {
		blockSize = 4096
	}
	if !isValidBlockSize(blockSize) {
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

// AppendDmVerityStream computes the dm-verity merkle tree (no superblock)
// for the dataSize bytes already written to dst at offsets [0, dataSize),
// then appends the tree to dst starting at offset dataSize.
//
// The caller is responsible for:
//   - having written exactly dataSize bytes to dst (positioned anywhere; this
//     function seeks dst);
//   - providing a separate ReaderAt (typically dst itself) from which the
//     dataSize bytes can be re-read to compute the leaf hashes.
//
// Memory: O(leafHashes) ≈ dataSize/blockSize * 32 bytes. For a 2 GiB image
// at 4096-byte blocks that is 16 MiB — far smaller than holding the whole
// image. Disk I/O: dataSize bytes are read twice (once when written, once
// when re-read here for hashing).
//
// blockSize: pass 0 for the default 4096. dataSize must be a positive
// multiple of blockSize.
//
// On success, dst is positioned at the end of the appended tree.
func AppendDmVerityStream(ctx context.Context, dst io.WriteSeeker, dataRA io.ReaderAt, dataSize int64, blockSize uint32) (*DmVerityResult, error) {
	if blockSize == 0 {
		blockSize = 4096
	}
	// Position dst at offset dataSize (where the tree will go).
	if _, err := dst.Seek(dataSize, io.SeekStart); err != nil {
		return nil, fmt.Errorf("dmverity stream: seek append point: %w", err)
	}
	// NewVerityWriter writes nothing on construction (no superblock).
	vw, err := NewVerityWriter(dst, dataSize, blockSize)
	if err != nil {
		return nil, err
	}
	// Feed the data bytes through the hasher only — passthrough goes to
	// io.Discard since the bytes are already on disk.
	vw.w = io.Discard
	if _, err := io.Copy(vw, io.NewSectionReader(dataRA, 0, dataSize)); err != nil {
		return nil, fmt.Errorf("dmverity stream: hash data: %w", err)
	}
	// Restore the real writer so Flush appends the hash tree to dst.
	vw.w = dst
	res, err := vw.Flush()
	if err != nil {
		return nil, fmt.Errorf("dmverity stream: flush tree: %w", err)
	}
	log.G(ctx).Debugf("dmverity stream: hash_offset=%d root_digest=%s", res.HashOffset, res.RootDigest)
	return res, nil
}

// AppendDmVerity is a convenience wrapper for callers that have the entire
// EROFS image in memory. It streams the data through VerityWriter without
// any intermediate temp file or external binary and returns:
//
//   - combined: [erofsData][hash tree levels]   (no superblock)
//   - result:   DmVerityResult with HashOffset and RootDigest
func AppendDmVerity(ctx context.Context, erofsData []byte, blockSize uint32) ([]byte, *DmVerityResult, error) {
	if blockSize == 0 {
		blockSize = 4096
	}

	dataSize := int64(len(erofsData))

	// Pre-allocate output: erofsData + estimate for the tree.
	// Tree ≈ dataSize / (blockSize/32) bytes, plus one root block of slack.
	treeEstimate := dataSize/(int64(blockSize)/32) + int64(blockSize)
	out := make([]byte, 0, dataSize+treeEstimate)
	out = append(out, erofsData...)

	// The VerityWriter writes the tree into appendW (appended after erofsData).
	appendW := &sliceAppender{dst: &out}

	vw, err := NewVerityWriter(appendW, dataSize, blockSize)
	if err != nil {
		return nil, nil, err
	}

	// Feed data for hashing. Passthrough goes to appendW (which appends to out),
	// but we only want the data once (already in out). Use io.Discard for the
	// passthrough and only let Flush write the tree portion.
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
