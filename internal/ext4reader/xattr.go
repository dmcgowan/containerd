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
)

const xattrHeaderMagic = 0xea020000

// xattr name index to prefix mapping, matching the kernel's ext4 implementation.
var xattrPrefixes = [...]struct {
	index  uint8
	prefix string
}{
	{1, "user."},
	{2, "system.posix_acl_access"},
	{3, "system.posix_acl_default"},
	{4, "trusted."},
	{6, "security."},
	{7, "system."},
}

func decompressXattrName(index uint8, name string) string {
	for _, p := range xattrPrefixes {
		if p.index == index {
			return p.prefix + name
		}
	}
	return name
}

// readInlineXattrs parses inline extended attributes stored in the inode
// body after the fixed + extra fields. The data starts at the first byte
// after inodeBaseSize + extraIsize.
func readInlineXattrs(data []byte, xattrs map[string][]byte) {
	if len(data) < 4 {
		return
	}
	magic := binary.LittleEndian.Uint32(data[0:])
	if magic != xattrHeaderMagic {
		return
	}
	parseXattrEntries(data[4:], xattrs, 0)
}

// readBlockXattrs reads extended attributes from an external xattr block.
func (fs *filesystem) readBlockXattrs(block uint64, xattrs map[string][]byte) error {
	buf := make([]byte, fs.info.blockSize)
	if _, err := fs.r.ReadAt(buf, int64(block)*fs.info.blockSize); err != nil {
		return fmt.Errorf("ext4: reading xattr block %d: %w", block, err)
	}
	magic := binary.LittleEndian.Uint32(buf[0:])
	if magic != xattrHeaderMagic {
		return fmt.Errorf("ext4: bad xattr block magic %#x", magic)
	}
	// Block xattr entries start at offset 32 (after the header).
	// Value offsets stored in entries are relative to the start of the block,
	// so we subtract 32 (the header size) when indexing into our data slice.
	parseXattrEntries(buf[32:], xattrs, 32)
	return nil
}

// parseXattrEntries parses a sequence of xattr entries from data.
// offsetDelta is subtracted from the value offset stored in each entry
// to get the offset relative to data.
func parseXattrEntries(data []byte, xattrs map[string][]byte, offsetDelta uint16) {
	pos := 0
	for pos+16 <= len(data) {
		nameLen := data[pos]
		if nameLen == 0 {
			break
		}
		nameIndex := data[pos+1]
		valueOffset := binary.LittleEndian.Uint16(data[pos+2:]) - offsetDelta
		valueSize := binary.LittleEndian.Uint32(data[pos+8:])

		nameEnd := pos + 16 + int(nameLen)
		if nameEnd > len(data) {
			break
		}
		name := string(data[pos+16 : nameEnd])
		fullName := decompressXattrName(nameIndex, name)

		// Skip system.data which is used internally for inline data
		if fullName == "system.data" {
			pos += (16 + int(nameLen) + 3) &^ 3
			continue
		}

		if int(valueOffset)+int(valueSize) <= len(data) && valueSize > 0 {
			val := make([]byte, valueSize)
			copy(val, data[valueOffset:int(valueOffset)+int(valueSize)])
			xattrs[fullName] = val
		}

		// Entry size is padded to 4 bytes
		pos += (16 + int(nameLen) + 3) &^ 3
	}
}
