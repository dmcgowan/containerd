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
	"time"
)

const (
	inodeRoot = 2

	// File type flags in inode mode
	typeMask = 0xF000
	modeIFIFO = 0x1000
	modeIFCHR = 0x2000
	modeIFDIR = 0x4000
	modeIFBLK = 0x6000
	modeIFREG  = 0x8000
	modeIFLNK  = 0xA000
	modeIFSOCK = 0xC000

	// Inode flags
	inodeFlagExtents = 0x80000

	// Base inode size
	inodeBaseSize = 128

	// Maximum inline symlink size (stored in inode Block field)
	maxInlineSymlink = 60
)

// rawInode is the on-disk inode structure (128 bytes base).
type rawInode struct {
	Mode         uint16
	Uid          uint16
	SizeLow      uint32
	Atime        uint32
	Ctime        uint32
	Mtime        uint32
	Dtime        uint32
	Gid            uint16
	LinksCount     uint16
	_              [4]byte // BlocksLow
	Flags          uint32
	_              [4]byte // Version
	Block          [60]byte
	_              [4]byte // Generation
	XattrBlockLow  uint32
	SizeHigh       uint32
	_              [4]byte // ObsoleteFragmentAddr
	_              [2]byte // BlocksHigh
	XattrBlockHigh uint16
	UidHigh      uint16
	GidHigh      uint16
	_            uint16 // ChecksumLow
	_            uint16 // Reserved
}

// rawInodeExtra contains extended inode fields starting at offset 128.
type rawInodeExtra struct {
	ExtraIsize   uint16
	_            uint16 // ChecksumHigh
	CtimeExtra   uint32
	MtimeExtra   uint32
	AtimeExtra   uint32
	_            uint32 // Crtime
	_            uint32 // CrtimeExtra
}

type inodeData struct {
	mode       uint16
	uid        uint32
	gid        uint32
	size       int64
	atime      time.Time
	ctime      time.Time
	mtime      time.Time
	linksCount uint32
	flags      uint32
	block      [60]byte
	xattrBlock uint64
	devMajor   uint32
	devMinor   uint32
	linkTarget string
	xattrs     map[string][]byte
	ino        uint64
}

func (fs *filesystem) readInode(ino uint64) (*inodeData, error) {
	group := (ino - 1) / uint64(fs.info.inodesPerGroup)
	index := (ino - 1) % uint64(fs.info.inodesPerGroup)

	// Read group descriptor to find inode table location
	inodeTableBlock, err := fs.readGroupDescInodeTable(uint32(group))
	if err != nil {
		return nil, err
	}

	// Calculate inode offset within the device
	inodeOff := int64(inodeTableBlock)*fs.info.blockSize + int64(index)*int64(fs.info.inodeSize)

	// Read raw inode (128 bytes)
	raw := make([]byte, fs.info.inodeSize)
	if _, err := fs.r.ReadAt(raw, inodeOff); err != nil {
		return nil, fmt.Errorf("ext4: reading inode %d: %w", ino, err)
	}

	var ri rawInode
	ri.Mode = binary.LittleEndian.Uint16(raw[0:])
	ri.Uid = binary.LittleEndian.Uint16(raw[2:])
	ri.SizeLow = binary.LittleEndian.Uint32(raw[4:])
	ri.Atime = binary.LittleEndian.Uint32(raw[8:])
	ri.Ctime = binary.LittleEndian.Uint32(raw[12:])
	ri.Mtime = binary.LittleEndian.Uint32(raw[16:])
	ri.Gid = binary.LittleEndian.Uint16(raw[24:])
	ri.LinksCount = binary.LittleEndian.Uint16(raw[26:])
	ri.Flags = binary.LittleEndian.Uint32(raw[32:])
	copy(ri.Block[:], raw[40:100])
	ri.XattrBlockLow = binary.LittleEndian.Uint32(raw[104:])
	ri.SizeHigh = binary.LittleEndian.Uint32(raw[108:])
	ri.XattrBlockHigh = binary.LittleEndian.Uint16(raw[118:])
	ri.UidHigh = binary.LittleEndian.Uint16(raw[120:])
	ri.GidHigh = binary.LittleEndian.Uint16(raw[122:])

	inode := &inodeData{
		mode:       ri.Mode,
		uid:        uint32(ri.Uid) | uint32(ri.UidHigh)<<16,
		gid:        uint32(ri.Gid) | uint32(ri.GidHigh)<<16,
		size:       int64(ri.SizeLow) | int64(ri.SizeHigh)<<32,
		linksCount: uint32(ri.LinksCount),
		flags:      ri.Flags,
		block:      ri.Block,
		xattrBlock: uint64(ri.XattrBlockLow) | uint64(ri.XattrBlockHigh)<<32,
		ino:        ino,
	}

	// Parse timestamps
	var extraAtime, extraMtime, extraCtime uint32
	if fs.info.inodeSize > inodeBaseSize {
		var extra rawInodeExtra
		extra.ExtraIsize = binary.LittleEndian.Uint16(raw[128:])
		if extra.ExtraIsize >= 12 {
			extraCtimeOff := 132
			extraCtime = binary.LittleEndian.Uint32(raw[extraCtimeOff:])
		}
		if extra.ExtraIsize >= 16 {
			extraMtime = binary.LittleEndian.Uint32(raw[136:])
		}
		if extra.ExtraIsize >= 20 {
			extraAtime = binary.LittleEndian.Uint32(raw[140:])
		}
	}
	inode.atime = decodeTime(ri.Atime, extraAtime)
	inode.ctime = decodeTime(ri.Ctime, extraCtime)
	inode.mtime = decodeTime(ri.Mtime, extraMtime)

	// Read xattrs
	inode.xattrs = make(map[string][]byte)
	if fs.info.inodeSize > inodeBaseSize {
		extraIsize := int(binary.LittleEndian.Uint16(raw[128:]))
		xattrStart := inodeBaseSize + extraIsize
		if xattrStart < fs.info.inodeSize {
			readInlineXattrs(raw[xattrStart:fs.info.inodeSize], inode.xattrs)
		}
	}
	if inode.xattrBlock != 0 {
		if err := fs.readBlockXattrs(inode.xattrBlock, inode.xattrs); err != nil {
			return nil, err
		}
	}

	// Handle device files
	fileType := inode.mode & typeMask
	if fileType == modeIFCHR || fileType == modeIFBLK {
		// Try new-style device encoding (Block[4:8]) first, fall back to old-style (Block[0:4])
		dev := binary.LittleEndian.Uint32(inode.block[4:8])
		if dev == 0 {
			dev = binary.LittleEndian.Uint32(inode.block[0:4])
		}
		inode.devMajor = (dev >> 8) & 0xfff
		inode.devMinor = (dev & 0xff) | ((dev >> 12) & 0xffffff00)
	}

	// Handle symlinks
	if fileType == modeIFLNK {
		target, err := fs.readSymlink(inode)
		if err != nil {
			return nil, err
		}
		inode.linkTarget = target
	}

	return inode, nil
}

func (fs *filesystem) readSymlink(inode *inodeData) (string, error) {
	if inode.size <= maxInlineSymlink && inode.flags&inodeFlagExtents == 0 {
		// Inline symlink stored directly in Block field
		return string(inode.block[:inode.size]), nil
	}
	// Symlink target is in data blocks
	extents, err := fs.readExtents(inode.block)
	if err != nil {
		return "", fmt.Errorf("ext4: reading symlink extents: %w", err)
	}
	buf := make([]byte, inode.size)
	if _, err := fs.readBlocks(extents, 0, buf); err != nil {
		return "", fmt.Errorf("ext4: reading symlink data: %w", err)
	}
	return string(buf), nil
}

// readGroupDescInodeTable reads the inode table block from the group descriptor.
func (fs *filesystem) readGroupDescInodeTable(group uint32) (uint64, error) {
	// Group descriptors start in the block after the superblock block
	gdStart := int64(fs.info.firstDataBlock+1) * fs.info.blockSize
	gdOff := gdStart + int64(group)*int64(fs.info.groupDescSize)

	buf := make([]byte, fs.info.groupDescSize)
	if _, err := fs.r.ReadAt(buf, gdOff); err != nil {
		return 0, fmt.Errorf("ext4: reading group descriptor %d: %w", group, err)
	}

	inodeTableLow := binary.LittleEndian.Uint32(buf[8:12])
	tableBlock := uint64(inodeTableLow)
	if fs.info.has64Bit && fs.info.groupDescSize >= 64 {
		inodeTableHigh := binary.LittleEndian.Uint32(buf[40:44])
		tableBlock |= uint64(inodeTableHigh) << 32
	}
	return tableBlock, nil
}

// decodeTime decodes an ext4 timestamp from the base 32-bit seconds
// field and the optional extra 32-bit field which encodes 2 additional
// epoch bits (bits 0-1) and 30 nanosecond bits (bits 2-31).
func decodeTime(sec uint32, extra uint32) time.Time {
	s := int64(sec)
	var nsec int64
	if extra != 0 {
		// Bits 0-1 of extra are the upper bits of the epoch
		epochBits := int64(extra & 0x3)
		s = int64(uint64(sec) | uint64(epochBits)<<32)
		// Bits 2-31 are nanoseconds
		nsec = int64(extra >> 2)
	}
	return time.Unix(s, nsec)
}
