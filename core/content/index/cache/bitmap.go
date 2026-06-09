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

package cache

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// bitmap tracks which chunks of a cached blob are present in the sparse file.
//
// Format on disk: ⌈numChunks/64⌉ × 8 bytes, little-endian uint64 words.
// Bit i (0-indexed from LSB of word 0) = chunk i.
// 1 = present; 0 = absent.
type bitmap struct {
	words []uint64
	n     int // numChunks
	f     *os.File
}

// openOrCreateBitmap opens an existing bitmap file or creates a fresh one.
// A fresh file has all bits zero (all chunks absent).
func openOrCreateBitmap(path string, numChunks int) (*bitmap, error) {
	nwords := (numChunks + 63) / 64
	if nwords == 0 {
		nwords = 1
	}
	words := make([]uint64, nwords)

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, fmt.Errorf("bitmap: open %s: %w", path, err)
	}

	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("bitmap: stat %s: %w", path, err)
	}

	expectedSize := int64(nwords * 8)
	if fi.Size() == expectedSize {
		// Read existing bitmap.
		if err := binary.Read(f, binary.LittleEndian, words); err != nil && err != io.EOF {
			f.Close()
			return nil, fmt.Errorf("bitmap: read %s: %w", path, err)
		}
	} else if fi.Size() == 0 {
		// Fresh file; words are already all-zero.
		// Write initial zero bytes so the file has the right size.
		if err := binary.Write(f, binary.LittleEndian, words); err != nil {
			f.Close()
			return nil, fmt.Errorf("bitmap: init %s: %w", path, err)
		}
	} else {
		// Size mismatch (corrupted or wrong numChunks). Truncate and start fresh.
		f.Truncate(0)
		f.Seek(0, io.SeekStart)
		if err := binary.Write(f, binary.LittleEndian, words); err != nil {
			f.Close()
			return nil, fmt.Errorf("bitmap: reinit %s: %w", path, err)
		}
	}

	return &bitmap{words: words, n: numChunks, f: f}, nil
}

// isSet returns true if chunk idx is marked present.
// Must be called under blobState.mu.
func (b *bitmap) isSet(idx int) bool {
	if b == nil || idx < 0 || idx >= b.n {
		return false
	}
	word := idx / 64
	bit := uint(idx % 64)
	return b.words[word]&(1<<bit) != 0
}

// set marks chunk idx as present in the in-memory bitmap.
// Must be called under blobState.mu.
func (b *bitmap) set(idx int) {
	if b == nil || idx < 0 || idx >= b.n {
		return
	}
	word := idx / 64
	bit := uint(idx % 64)
	b.words[word] |= 1 << bit
}

// persistWord writes the uint64 word containing bit idx to disk.
// Safe to call without the blobState.mu held (pwrite is atomic per POSIX).
func (b *bitmap) persistWord(path string, idx int) error {
	if b == nil || b.f == nil {
		return nil
	}
	wordIdx := idx / 64
	if wordIdx >= len(b.words) {
		return nil
	}
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], b.words[wordIdx])
	offset := int64(wordIdx * 8)
	if _, err := b.f.WriteAt(buf[:], offset); err != nil {
		return fmt.Errorf("bitmap: write word %d to %s: %w", wordIdx, path, err)
	}
	return b.f.Sync()
}

// close closes the backing file.
func (b *bitmap) close() {
	if b != nil && b.f != nil {
		b.f.Close()
		b.f = nil
	}
}
