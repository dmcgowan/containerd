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

// Package ext4reader provides a minimal read-only ext4 filesystem
// implementation that satisfies fs.FS. It reads ext4 images from an
// io.ReaderAt without requiring mount syscalls, making it usable
// cross-platform for reading ext4 block files.
package ext4reader

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"
)

// InodeInfo carries ext4-specific metadata accessible via FileInfo.Sys().
type InodeInfo struct {
	Ino        uint64
	UID        uint32
	GID        uint32
	Atime      time.Time
	Mtime      time.Time
	Ctime      time.Time
	Nlink      uint32
	DevMajor   uint32
	DevMinor   uint32
	LinkTarget string
	Xattrs     map[string][]byte
}

// FS opens a read-only ext4 filesystem from an io.ReaderAt.
func FS(r io.ReaderAt) (fs.FS, error) {
	info, err := readSuperblock(r)
	if err != nil {
		return nil, err
	}
	return &filesystem{r: r, info: info}, nil
}

type filesystem struct {
	r    io.ReaderAt
	info *fsInfo
}

// Open implements fs.FS.
func (f *filesystem) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}

	inode, err := f.lookup(name)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}

	return &file{
		fs:    f,
		inode: inode,
		name:  path.Base(name),
	}, nil
}

// lookup resolves a path to its inode, starting from root.
func (f *filesystem) lookup(name string) (*inodeData, error) {
	inode, err := f.readInode(inodeRoot)
	if err != nil {
		return nil, err
	}

	if name == "." {
		return inode, nil
	}

	parts := strings.Split(name, "/")
	for _, part := range parts {
		if inode.mode&typeMask != modeIFDIR {
			return nil, fs.ErrNotExist
		}
		entries, err := f.readDir(inode)
		if err != nil {
			return nil, err
		}
		found := false
		for _, e := range entries {
			if e.name == part {
				inode, err = f.readInode(uint64(e.inode))
				if err != nil {
					return nil, err
				}
				found = true
				break
			}
		}
		if !found {
			return nil, fs.ErrNotExist
		}
	}

	return inode, nil
}

type file struct {
	fs     *filesystem
	inode  *inodeData
	name   string
	off    int64
	exts   []extent // cached extents for reading
}

// Stat implements fs.File.
func (f *file) Stat() (fs.FileInfo, error) {
	return &fileInfo{inode: f.inode, name: f.name}, nil
}

// Read implements fs.File.
func (f *file) Read(buf []byte) (int, error) {
	if f.inode.mode&typeMask == modeIFDIR {
		return 0, &fs.PathError{Op: "read", Path: f.name, Err: fmt.Errorf("is a directory")}
	}

	if f.off >= f.inode.size {
		return 0, io.EOF
	}

	if f.exts == nil {
		exts, err := f.fs.readExtents(f.inode.block)
		if err != nil {
			return 0, err
		}
		f.exts = exts
	}

	// Limit read to file size
	remaining := f.inode.size - f.off
	if int64(len(buf)) > remaining {
		buf = buf[:remaining]
	}

	n, err := f.fs.readBlocks(f.exts, f.off, buf)
	f.off += int64(n)
	if err == nil && f.off >= f.inode.size {
		err = io.EOF
	}
	return n, err
}

// Close implements fs.File.
func (f *file) Close() error {
	return nil
}

// ReadDir implements fs.ReadDirFile.
func (f *file) ReadDir(n int) ([]fs.DirEntry, error) {
	if f.inode.mode&typeMask != modeIFDIR {
		return nil, &fs.PathError{Op: "readdir", Path: f.name, Err: fmt.Errorf("not a directory")}
	}

	entries, err := f.fs.readDir(f.inode)
	if err != nil {
		return nil, err
	}

	result := make([]fs.DirEntry, 0, len(entries))
	for _, e := range entries {
		inode, err := f.fs.readInode(uint64(e.inode))
		if err != nil {
			return nil, err
		}
		result = append(result, &dirEntryInfo{
			name:  e.name,
			inode: inode,
		})
	}

	if n > 0 && n < len(result) {
		result = result[:n]
	}

	return result, nil
}

type fileInfo struct {
	inode *inodeData
	name  string
}

func (fi *fileInfo) Name() string      { return fi.name }
func (fi *fileInfo) Size() int64       { return fi.inode.size }
func (fi *fileInfo) ModTime() time.Time { return fi.inode.mtime }
func (fi *fileInfo) IsDir() bool       { return fi.inode.mode&typeMask == modeIFDIR }
func (fi *fileInfo) Sys() any  {
	return &InodeInfo{
		Ino:        fi.inode.ino,
		UID:        fi.inode.uid,
		GID:        fi.inode.gid,
		Atime:      fi.inode.atime,
		Mtime:      fi.inode.mtime,
		Ctime:      fi.inode.ctime,
		Nlink:      fi.inode.linksCount,
		DevMajor:   fi.inode.devMajor,
		DevMinor:   fi.inode.devMinor,
		LinkTarget: fi.inode.linkTarget,
		Xattrs:     fi.inode.xattrs,
	}
}

func (fi *fileInfo) Mode() fs.FileMode {
	mode := fs.FileMode(fi.inode.mode & 0o7777)
	switch fi.inode.mode & typeMask {
	case modeIFDIR:
		mode |= fs.ModeDir
	case modeIFLNK:
		mode |= fs.ModeSymlink
	case modeIFCHR:
		mode |= fs.ModeDevice | fs.ModeCharDevice
	case modeIFBLK:
		mode |= fs.ModeDevice
	case modeIFIFO:
		mode |= fs.ModeNamedPipe
	case modeIFSOCK:
		mode |= fs.ModeSocket
	}
	if fi.inode.mode&0o4000 != 0 {
		mode |= fs.ModeSetuid
	}
	if fi.inode.mode&0o2000 != 0 {
		mode |= fs.ModeSetgid
	}
	if fi.inode.mode&0o1000 != 0 {
		mode |= fs.ModeSticky
	}
	return mode
}

type dirEntryInfo struct {
	name  string
	inode *inodeData
}

func (d *dirEntryInfo) Name() string               { return d.name }
func (d *dirEntryInfo) IsDir() bool                { return d.inode.mode&typeMask == modeIFDIR }
func (d *dirEntryInfo) Type() fs.FileMode          { return (&fileInfo{inode: d.inode}).Mode().Type() }
func (d *dirEntryInfo) Info() (fs.FileInfo, error) {
	return &fileInfo{inode: d.inode, name: d.name}, nil
}

// Walk traverses the filesystem tree starting at the given root path,
// calling fn for each file or directory. The path passed to fn is
// relative to root (using forward slashes). This is similar to
// fs.WalkDir but provides access to the full InodeInfo via FileInfo.Sys().
func Walk(fsys fs.FS, root string, fn func(path string, info fs.FileInfo, err error) error) error {
	return walkDir(fsys, root, fn)
}

func walkDir(fsys fs.FS, dir string, fn func(string, fs.FileInfo, error) error) error {
	f, err := fsys.Open(dir)
	if err != nil {
		return fn(dir, nil, err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return fn(dir, nil, err)
	}

	if err := fn(dir, fi, nil); err != nil {
		if err == fs.SkipDir {
			return nil
		}
		return err
	}

	if !fi.IsDir() {
		return nil
	}

	dirFile, ok := f.(fs.ReadDirFile)
	if !ok {
		return fmt.Errorf("ext4: directory does not implement ReadDirFile")
	}

	entries, err := dirFile.ReadDir(-1)
	if err != nil {
		return fn(dir, nil, err)
	}

	for _, entry := range entries {
		childPath := dir + "/" + entry.Name()
		if dir == "." {
			childPath = entry.Name()
		}

		childInfo, err := entry.Info()
		if err != nil {
			if err2 := fn(childPath, nil, err); err2 != nil {
				return err2
			}
			continue
		}

		if childInfo.IsDir() {
			if err := walkDir(fsys, childPath, fn); err != nil {
				return err
			}
		} else {
			if err := fn(childPath, childInfo, nil); err != nil {
				return err
			}
		}
	}

	return nil
}

// inodeFileMode converts an ext4 mode field (uint16) to the os.FileMode
// value used in tar headers. This preserves the low 12 permission bits
// plus the file type.
func InodeFileMode(mode uint16) os.FileMode {
	fi := &fileInfo{inode: &inodeData{mode: mode}}
	return fi.Mode()
}
