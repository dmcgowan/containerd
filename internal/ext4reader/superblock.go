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

const (
	superblockOffset = 1024
	superblockMagic  = 0xef53

	incompatExtents = 0x40
	incompat64Bit   = 0x80
)

type superblock struct {
	InodesCount        uint32
	BlocksCountLow     uint32
	_                  [4]byte // RootBlocksCountLow
	_                  [4]byte // FreeBlocksCountLow
	_                  [4]byte // FreeInodesCount
	FirstDataBlock     uint32
	LogBlockSize       uint32
	_                  [4]byte // LogClusterSize
	BlocksPerGroup     uint32
	_                  [4]byte // ClustersPerGroup
	InodesPerGroup     uint32
	_                  [12]byte // Mtime, Wtime, MountCount, MaxMountCount
	Magic              uint16
	_                  [2]byte // State
	_                  [2]byte // Errors
	_                  [2]byte // MinorRevisionLevel
	_                  [4]byte // LastCheck
	_                  [4]byte // CheckInterval
	_                  [4]byte // CreatorOS
	RevisionLevel      uint32
	_                  [2]byte // DefaultReservedUid
	_                  [2]byte // DefaultReservedGid
	FirstInode         uint32
	InodeSize          uint16
	_                  [2]byte // BlockGroupNr
	FeatureCompat      uint32
	FeatureIncompat    uint32
	FeatureRoCompat    uint32
	_                  [16]byte // UUID
	_                  [16]byte // VolumeName
	_                  [64]byte // LastMounted
	_                  [4]byte  // AlgorithmUsageBitmap
	_                  [1]byte  // PreallocBlocks
	_                  [1]byte  // PreallocDirBlocks
	_                  [2]byte  // ReservedGdtBlocks
	_                  [16]byte // JournalUUID
	_                  [4]byte  // JournalInum
	_                  [4]byte  // JournalDev
	_                  [4]byte  // LastOrphan
	_                  [16]byte // HashSeed
	_                  [1]byte  // DefHashVersion
	_                  [1]byte  // JournalBackupType
	DescSize           uint16
}

type fsInfo struct {
	blockSize      int64
	inodeSize      int
	inodesPerGroup uint32
	blocksPerGroup uint32
	firstDataBlock uint32
	groupDescSize  int
	has64Bit       bool
	hasExtents     bool
}

func readSuperblock(r io.ReaderAt) (*fsInfo, error) {
	var sb superblock
	sr := io.NewSectionReader(r, superblockOffset, int64(binary.Size(sb)))
	if err := binary.Read(sr, binary.LittleEndian, &sb); err != nil {
		return nil, fmt.Errorf("ext4: reading superblock: %w", err)
	}
	if sb.Magic != superblockMagic {
		return nil, fmt.Errorf("ext4: bad magic %#x (expected %#x)", sb.Magic, superblockMagic)
	}

	info := &fsInfo{
		blockSize:      1024 << sb.LogBlockSize,
		inodeSize:      int(sb.InodeSize),
		inodesPerGroup: sb.InodesPerGroup,
		blocksPerGroup: sb.BlocksPerGroup,
		firstDataBlock: sb.FirstDataBlock,
		hasExtents:     sb.FeatureIncompat&incompatExtents != 0,
		has64Bit:       sb.FeatureIncompat&incompat64Bit != 0,
	}

	if info.inodeSize == 0 {
		if sb.RevisionLevel == 0 {
			info.inodeSize = 128
		} else {
			return nil, fmt.Errorf("ext4: inode size is 0")
		}
	}

	info.groupDescSize = 32
	if info.has64Bit && sb.DescSize > 32 {
		info.groupDescSize = int(sb.DescSize)
	}

	return info, nil
}
