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

package ext4reader

import (
	"encoding/binary"
	"fmt"
	"io"
)

const extentHeaderMagic = 0xf30a

type extentHeader struct {
	Magic   uint16
	Entries uint16
	Max     uint16
	Depth   uint16
	_       uint32 // Generation
}

type extentLeaf struct {
	Block     uint32
	Length    uint16
	StartHi  uint16
	StartLow uint32
}

type extentIndex struct {
	Block   uint32
	LeafLow uint32
	LeafHi  uint16
	_       uint16
}

// extent represents a mapping from logical blocks to physical blocks.
type extent struct {
	logicalStart uint32
	physStart    uint64
	length       uint32
}

// readExtents reads the extent tree rooted in the inode's Block field
// and returns a flat list of extents.
func (fs *filesystem) readExtents(blockData [60]byte) ([]extent, error) {
	return fs.parseExtentNode(blockData[:])
}

func (fs *filesystem) parseExtentNode(data []byte) ([]extent, error) {
	if len(data) < 12 {
		return nil, fmt.Errorf("ext4: extent data too short")
	}

	var hdr extentHeader
	hdr.Magic = binary.LittleEndian.Uint16(data[0:])
	hdr.Entries = binary.LittleEndian.Uint16(data[2:])
	hdr.Depth = binary.LittleEndian.Uint16(data[6:])

	if hdr.Magic != extentHeaderMagic {
		return nil, fmt.Errorf("ext4: bad extent header magic %#x", hdr.Magic)
	}

	entryData := data[12:]
	if hdr.Depth == 0 {
		return parseLeaves(entryData, int(hdr.Entries))
	}
	return fs.parseIndexNodes(entryData, int(hdr.Entries))
}

func parseLeaves(data []byte, count int) ([]extent, error) {
	extents := make([]extent, 0, count)
	for i := 0; i < count; i++ {
		off := i * 12
		if off+12 > len(data) {
			break
		}
		leaf := extentLeaf{
			Block:    binary.LittleEndian.Uint32(data[off:]),
			Length:   binary.LittleEndian.Uint16(data[off+4:]),
			StartHi:  binary.LittleEndian.Uint16(data[off+6:]),
			StartLow: binary.LittleEndian.Uint32(data[off+8:]),
		}
		length := uint32(leaf.Length)
		if length > 0x8000 {
			// Uninitialized extent, mask out the high bit for the real length
			length -= 0x8000
		}
		extents = append(extents, extent{
			logicalStart: leaf.Block,
			physStart:    uint64(leaf.StartHi)<<32 | uint64(leaf.StartLow),
			length:       length,
		})
	}
	return extents, nil
}

func (fs *filesystem) parseIndexNodes(data []byte, count int) ([]extent, error) {
	var allExtents []extent
	for i := 0; i < count; i++ {
		off := i * 12
		if off+12 > len(data) {
			break
		}
		idx := extentIndex{
			LeafLow: binary.LittleEndian.Uint32(data[off+4:]),
			LeafHi:  binary.LittleEndian.Uint16(data[off+8:]),
		}
		childBlock := uint64(idx.LeafHi)<<32 | uint64(idx.LeafLow)
		childData := make([]byte, fs.info.blockSize)
		if _, err := fs.r.ReadAt(childData, int64(childBlock)*fs.info.blockSize); err != nil {
			return nil, fmt.Errorf("ext4: reading extent index block %d: %w", childBlock, err)
		}
		exts, err := fs.parseExtentNode(childData)
		if err != nil {
			return nil, err
		}
		allExtents = append(allExtents, exts...)
	}
	return allExtents, nil
}

// readBlocks reads data from the given extents starting at byte offset
// off, reading up to len(buf) bytes.
func (fs *filesystem) readBlocks(extents []extent, off int64, buf []byte) (int, error) {
	total := 0
	for len(buf) > 0 {
		// Find which extent covers the logical offset
		logBlock := uint32(off / fs.info.blockSize)
		blockOff := off % fs.info.blockSize

		var found *extent
		for i := range extents {
			e := &extents[i]
			if logBlock >= e.logicalStart && logBlock < e.logicalStart+e.length {
				found = e
				break
			}
		}
		if found == nil {
			// Past end of file or sparse hole - return zeros
			break
		}

		physBlock := found.physStart + uint64(logBlock-found.logicalStart)
		physOff := int64(physBlock)*fs.info.blockSize + blockOff

		// How many bytes can we read from this extent?
		remainInExtent := int64(found.logicalStart+found.length)*fs.info.blockSize - off
		toRead := int64(len(buf))
		if toRead > remainInExtent {
			toRead = remainInExtent
		}

		n, err := fs.r.ReadAt(buf[:toRead], physOff)
		total += n
		buf = buf[n:]
		off += int64(n)
		if err != nil {
			if err == io.EOF && total > 0 {
				return total, nil
			}
			return total, err
		}
	}
	return total, nil
}
