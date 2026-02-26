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

type dirEntry struct {
	inode    uint32
	name     string
	fileType uint8
}

// readDir reads directory entries from the inode's data blocks.
// It handles both linear and htree directories (for htree, the linear
// scan of all data blocks naturally works because the root block's
// ".." entry spans the rest of the block, hiding the htree metadata).
func (fs *filesystem) readDir(inode *inodeData) ([]dirEntry, error) {
	if inode.mode&typeMask != modeIFDIR {
		return nil, fmt.Errorf("ext4: inode %d is not a directory", inode.ino)
	}

	extents, err := fs.readExtents(inode.block)
	if err != nil {
		return nil, fmt.Errorf("ext4: reading directory extents for inode %d: %w", inode.ino, err)
	}

	// Read all directory data
	data := make([]byte, inode.size)
	if _, err := fs.readBlocks(extents, 0, data); err != nil {
		return nil, fmt.Errorf("ext4: reading directory data for inode %d: %w", inode.ino, err)
	}

	return parseDirEntries(data), nil
}

// parseDirEntries parses linear directory entries from raw block data.
func parseDirEntries(data []byte) []dirEntry {
	var entries []dirEntry
	off := 0
	for off+8 <= len(data) {
		ino := binary.LittleEndian.Uint32(data[off:])
		recLen := binary.LittleEndian.Uint16(data[off+4:])
		nameLen := data[off+6]
		fileType := data[off+7]

		if recLen == 0 {
			break
		}
		if ino != 0 && int(nameLen) > 0 && off+8+int(nameLen) <= len(data) {
			name := string(data[off+8 : off+8+int(nameLen)])
			// Skip . and ..
			if name != "." && name != ".." {
				entries = append(entries, dirEntry{
					inode:    ino,
					name:     name,
					fileType: fileType,
				})
			}
		}

		off += int(recLen)
	}
	return entries
}
