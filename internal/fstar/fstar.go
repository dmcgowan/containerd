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

// Package fstar writes OCI-format tar streams from an fs.FS, converting
// overlay-style whiteouts to OCI whiteout entries.
package fstar

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"time"
)

const (
	whiteoutPrefix    = ".wh."
	whiteoutOpaqueDir = ".wh..wh..opq"
	paxSchilyXattr    = "SCHILY.xattr."
)

// FileMetadata carries rich per-file metadata that fs.FileInfo alone
// doesn't provide.
type FileMetadata struct {
	Ino        uint64
	UID, GID   uint32
	Mtime      time.Time
	Nlink      uint32
	DevMajor   uint32
	DevMinor   uint32
	LinkTarget string
	Xattrs     map[string][]byte
}

// MetadataFunc extracts FileMetadata from a FileInfo's Sys() value.
// The caller supplies an implementation appropriate for the underlying fs.FS.
type MetadataFunc func(info fs.FileInfo) (*FileMetadata, error)

// WriteOverlayTar walks root within fsys and writes an OCI-format tar stream
// to w. Overlay-style whiteouts (character devices with major:minor 0:0) are
// converted to OCI whiteout files (.wh.<name>), and directories with the
// trusted.overlay.opaque xattr get a .wh..wh..opq entry.
func WriteOverlayTar(ctx context.Context, w io.Writer, fsys fs.FS, root string, metadataFn MetadataFunc) error {
	// Check if the root directory exists
	if _, err := fs.Stat(fsys, root); err != nil {
		return fmt.Errorf("fs has no %s directory: %w", root, err)
	}

	tw := tar.NewWriter(w)
	defer tw.Close()

	// Track inodes for hardlink detection
	inodeSrc := make(map[uint64]string) // inode -> first path seen

	return fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Skip the root directory itself
		if p == root {
			return nil
		}

		// Get path relative to root
		relPath := strings.TrimPrefix(p, root+"/")
		if relPath == "" {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("getting file info for %s: %w", p, err)
		}

		md, err := metadataFn(info)
		if err != nil {
			return fmt.Errorf("getting metadata for %s: %w", p, err)
		}

		// Handle overlay opaque directories
		if info.IsDir() {
			if v, exists := md.Xattrs["trusted.overlay.opaque"]; exists && len(v) == 1 && v[0] == 'y' {
				opqPath := path.Join(relPath, whiteoutOpaqueDir)
				if err := writeWhiteoutEntry(tw, opqPath); err != nil {
					return err
				}
			}
		}

		// Handle overlay whiteout (char device 0:0) -> OCI whiteout
		if info.Mode()&fs.ModeCharDevice != 0 && md.DevMajor == 0 && md.DevMinor == 0 {
			dir := path.Dir(relPath)
			base := path.Base(relPath)
			whiteout := path.Join(dir, whiteoutPrefix+base)
			if dir == "." {
				whiteout = whiteoutPrefix + base
			}
			return writeWhiteoutEntry(tw, whiteout)
		}

		// Build tar header
		hdr := &tar.Header{
			Format: tar.FormatPAX,
		}

		// Set name - directories end with /
		name := relPath
		if info.IsDir() && !strings.HasSuffix(name, "/") {
			name += "/"
		}
		hdr.Name = name

		// Set common metadata
		hdr.Uid = int(md.UID)
		hdr.Gid = int(md.GID)
		hdr.ModTime = md.Mtime.Truncate(time.Second)
		hdr.AccessTime = time.Time{}
		hdr.ChangeTime = time.Time{}
		hdr.Mode = int64(info.Mode().Perm())
		if info.Mode()&fs.ModeSetuid != 0 {
			hdr.Mode |= 0o4000
		}
		if info.Mode()&fs.ModeSetgid != 0 {
			hdr.Mode |= 0o2000
		}
		if info.Mode()&fs.ModeSticky != 0 {
			hdr.Mode |= 0o1000
		}

		switch {
		case info.IsDir():
			hdr.Typeflag = tar.TypeDir
		case info.Mode()&fs.ModeSymlink != 0:
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = md.LinkTarget
		case info.Mode()&fs.ModeDevice != 0 && info.Mode()&fs.ModeCharDevice != 0:
			hdr.Typeflag = tar.TypeChar
			hdr.Devmajor = int64(md.DevMajor)
			hdr.Devminor = int64(md.DevMinor)
		case info.Mode()&fs.ModeDevice != 0:
			hdr.Typeflag = tar.TypeBlock
			hdr.Devmajor = int64(md.DevMajor)
			hdr.Devminor = int64(md.DevMinor)
		case info.Mode()&fs.ModeNamedPipe != 0:
			hdr.Typeflag = tar.TypeFifo
		case info.Mode().IsRegular():
			// Check for hardlinks
			if md.Nlink > 1 {
				if src, ok := inodeSrc[md.Ino]; ok {
					hdr.Typeflag = tar.TypeLink
					hdr.Linkname = src
					hdr.Size = 0
					return tw.WriteHeader(hdr)
				}
				inodeSrc[md.Ino] = relPath
			}
			hdr.Typeflag = tar.TypeReg
			hdr.Size = info.Size()
		default:
			// Skip sockets and unknown types
			return nil
		}

		// Add xattrs as PAX records (skip overlay-internal ones)
		for name, val := range md.Xattrs {
			if name == "trusted.overlay.opaque" ||
				name == "trusted.overlay.origin" ||
				name == "trusted.overlay.impure" ||
				name == "trusted.overlay.nlink" {
				continue
			}
			if hdr.PAXRecords == nil {
				hdr.PAXRecords = map[string]string{}
			}
			hdr.PAXRecords[paxSchilyXattr+name] = string(val)
		}

		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("writing tar header for %s: %w", relPath, err)
		}

		// Write file content
		if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 {
			file, err := fsys.Open(p)
			if err != nil {
				return fmt.Errorf("opening %s for reading: %w", p, err)
			}
			defer file.Close()

			n, err := io.Copy(tw, file)
			if err != nil {
				return fmt.Errorf("writing content for %s: %w", relPath, err)
			}
			if n != hdr.Size {
				return fmt.Errorf("short write for %s: %d != %d", relPath, n, hdr.Size)
			}
		}

		return nil
	})
}

func writeWhiteoutEntry(tw *tar.Writer, name string) error {
	whiteOutT := time.Unix(0, 0).UTC()
	return tw.WriteHeader(&tar.Header{
		Typeflag:   tar.TypeReg,
		Name:       name,
		Size:       0,
		ModTime:    whiteOutT,
		AccessTime: whiteOutT,
		ChangeTime: whiteOutT,
	})
}
