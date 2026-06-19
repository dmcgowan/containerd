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

package blockmount

import "sync"

// pageBitmap tracks which pages of a file are resident in the backing file.
// Granularity is one bit per page (page = pageSize bytes).
//
// The shim's view of "page presence" is derived from the Filled byte-range
// messages from the daemon.  A page is marked present when its entire span
// is covered by one or more Filled ranges.
//
// This is independent of the daemon-side chunk bitmap; the shim does not need
// to know about EROFS chunk sizes — it operates purely on page granularity.
type pageBitmap struct {
	mu       sync.Mutex
	bits     []uint64 // 1 bit per page, packed
	numPages int
	pageSize int64
	present  int // count of set bits
}

func newPageBitmap(numPages int, pageSize int64) *pageBitmap {
	words := (numPages + 63) / 64
	return &pageBitmap{
		bits:     make([]uint64, words),
		numPages: numPages,
		pageSize: pageSize,
	}
}

// markRange marks all pages *wholly contained* within [off, off+length).
// Partial pages at the boundaries are NOT marked — the shim requires complete
// pages to be resident before allowing reads.
func (pb *pageBitmap) markRange(off, length int64) {
	if length <= 0 {
		return
	}
	// First page wholly inside the range.
	firstPage := (off + pb.pageSize - 1) / pb.pageSize
	// Last page whose end is ≤ off+length.
	end := off + length
	lastPage := end/pb.pageSize - 1
	if end%pb.pageSize != 0 {
		// The last page ends within the range.
		lastPage = end / pb.pageSize
	}

	pb.mu.Lock()
	defer pb.mu.Unlock()
	for p := firstPage; p <= lastPage && int(p) < pb.numPages; p++ {
		word := p / 64
		bit := p % 64
		if pb.bits[word]&(1<<uint(bit)) == 0 {
			pb.bits[word] |= 1 << uint(bit)
			pb.present++
		}
	}
}

// isPagePresent returns true if page p is marked present.
func (pb *pageBitmap) isPagePresent(p int) bool {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	if p < 0 || p >= pb.numPages {
		return false
	}
	word := p / 64
	bit := p % 64
	return pb.bits[word]&(1<<uint(bit)) != 0
}

// allPagesPresent returns true if every page in the range [off, off+length)
// is marked present.  Used by the fanotify supervisor to decide whether to
// ALLOW immediately or send a Fill request first.
func (pb *pageBitmap) allPagesPresent(off, length int64) bool {
	first := off / pb.pageSize
	last := (off + length - 1) / pb.pageSize
	pb.mu.Lock()
	defer pb.mu.Unlock()
	for p := first; p <= last && int(p) < pb.numPages; p++ {
		word := p / 64
		bit := p % 64
		if pb.bits[word]&(1<<uint(bit)) == 0 {
			return false
		}
	}
	return true
}

// allPresent returns true when every page is marked present.
func (pb *pageBitmap) allPresent() bool {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	return pb.present >= pb.numPages
}
