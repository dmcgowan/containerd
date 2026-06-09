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

// Package erofsutils provides pure-Go implementations of EROFS image creation
// that previously required the external mkfs.erofs binary.
//
// All functions in this file use only:
//   - github.com/erofs/go-erofs — in-process EROFS writer
//   - github.com/containerd/continuity/tarconv — tar→EROFS conversion
//
// No external process is spawned.  The implementations are cross-platform
// (Linux, macOS, Windows) to the same extent that go-erofs and continuity are.
package erofsutils

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/containerd/log"
	"github.com/containerd/continuity/tarconv"
	goerofs "github.com/erofs/go-erofs"
)

// ConvertTarErofsGo converts a tar stream r into a full EROFS image at
// layerPath using the pure-Go go-erofs + continuity/tarconv stack.
//
// It replaces the mkfs.erofs --tar=f --aufs invocation.  Whiteouts are
// translated to overlayfs representation (char-device 0:0 + xattrs), matching
// the behaviour of mkfs.erofs --aufs.
//
// The uuid parameter is currently unused (go-erofs derives a deterministic UUID
// from the image content); it is retained for API compatibility.
func ConvertTarErofsGo(ctx context.Context, r io.Reader, layerPath, uuid string) error {
	f, err := os.Create(layerPath)
	if err != nil {
		return fmt.Errorf("ConvertTarErofsGo: create output: %w", err)
	}
	defer f.Close()

	w := goerofs.Create(f)
	if err := tarconv.Apply(w, r); err != nil {
		return fmt.Errorf("ConvertTarErofsGo: apply tar: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("ConvertTarErofsGo: finalise EROFS: %w", err)
	}
	log.G(ctx).Debugf("ConvertTarErofsGo: wrote %s", layerPath)
	return nil
}

// ConvertDirErofsGo converts a source directory srcDir into an EROFS image at
// layerPath using the pure-Go go-erofs writer.
//
// It replaces the mkfs.erofs <layerPath> <srcDir> invocation used by the EROFS
// snapshotter's Commit() path (converting an overlayfs upperdir to EROFS).
//
// On platforms where go-erofs cannot extract Unix metadata from os.Stat()
// results (i.e., non-Linux, non-Darwin), file ownership and device numbers
// will be zero-valued.  Permission bits and timestamps are always preserved.
func ConvertDirErofsGo(ctx context.Context, layerPath, srcDir string) error {
	f, err := os.Create(layerPath)
	if err != nil {
		return fmt.Errorf("ConvertDirErofsGo: create output: %w", err)
	}
	defer f.Close()

	w := goerofs.Create(f)
	src := os.DirFS(srcDir)
	if err := w.CopyFrom(src); err != nil {
		return fmt.Errorf("ConvertDirErofsGo: copy from dir: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("ConvertDirErofsGo: finalise EROFS: %w", err)
	}
	log.G(ctx).Debugf("ConvertDirErofsGo: wrote %s from %s", layerPath, srcDir)
	return nil
}

// GenerateTarIndexAndAppendTarGo produces the EROFS tar-index format in pure Go.
//
// # Output file layout
//
//	[EROFS metadata image (chunk-index table referencing the data file)]
//	[Sequential file payload bytes at 512-byte-aligned positions]
//
// The EROFS portion contains only filesystem metadata and chunk-index entries.
// Each file's payload is stored at a 512-byte-block-aligned position in the
// appended data region; the chunk indexes record those block offsets so a
// consumer using the EROFS kernel driver (with DeviceID=1 for the data device)
// can read file content directly from the combined blob.
//
// This is the pure-Go equivalent of mkfs.erofs --tar=i --aufs, with the
// difference that the data region contains raw payload bytes (block-padded)
// rather than the original tar stream.  Consumers that use the EROFS
// chunk-based inode format to locate file data are unaffected by this
// distinction.
//
// This replaces the mkfs.erofs --tar=i --aufs invocation.
func GenerateTarIndexAndAppendTarGo(ctx context.Context, r io.Reader, layerPath, uuid string) error {
	// dataFile receives file payload bytes from the tar stream.
	// go-erofs writes payloads here at 512-byte-block-aligned positions and
	// records chunk indexes (DeviceID=1) pointing into this file.
	dataFile, err := os.CreateTemp("", "erofs-tar-idx-data-*")
	if err != nil {
		return fmt.Errorf("GenerateTarIndexAndAppendTarGo: create data temp: %w", err)
	}
	defer os.Remove(dataFile.Name())
	defer dataFile.Close()

	// metaFile receives the EROFS metadata image (superblock + inodes + chunk
	// table, no payload bytes).
	metaFile, err := os.CreateTemp("", "erofs-tar-idx-meta-*.erofs")
	if err != nil {
		return fmt.Errorf("GenerateTarIndexAndAppendTarGo: create meta temp: %w", err)
	}
	defer os.Remove(metaFile.Name())
	defer metaFile.Close()

	// Block size 512 matches tar's granularity: file data in a tar stream
	// always starts on a 512-byte boundary (one header block = 512 bytes).
	w := goerofs.Create(metaFile,
		goerofs.WithBlockSize(512),
		goerofs.WithDataFile(dataFile),
	)

	// Apply the tar stream.  In the default (non-tar-index) mode, addFile
	// calls io.Copy(f, tr) which routes data through the go-erofs File and
	// on to dataFile.  go-erofs tracks the write offset and records chunk
	// indexes; closeDataFile() pads each file to a 512-byte boundary and
	// records Chunk{PhysicalBlock: startBlock, Count: …, DeviceID: 1}.
	if err := tarconv.Apply(w, r); err != nil {
		return fmt.Errorf("GenerateTarIndexAndAppendTarGo: apply tar: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("GenerateTarIndexAndAppendTarGo: finalise EROFS: %w", err)
	}

	// Assemble output = EROFS metadata + payload data.
	out, err := os.Create(layerPath)
	if err != nil {
		return fmt.Errorf("GenerateTarIndexAndAppendTarGo: create output: %w", err)
	}
	defer out.Close()

	if _, err := metaFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("GenerateTarIndexAndAppendTarGo: seek meta: %w", err)
	}
	if _, err := io.Copy(out, metaFile); err != nil {
		return fmt.Errorf("GenerateTarIndexAndAppendTarGo: copy meta: %w", err)
	}

	if _, err := dataFile.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("GenerateTarIndexAndAppendTarGo: seek data: %w", err)
	}
	if _, err := io.Copy(out, dataFile); err != nil {
		return fmt.Errorf("GenerateTarIndexAndAppendTarGo: append data: %w", err)
	}

	log.G(ctx).Debugf("GenerateTarIndexAndAppendTarGo: wrote tar-index EROFS at %s", layerPath)
	return nil
}

// noDataFS wraps an fs.FS and returns zero-length readers for all regular files.
// Used to build metadata-only EROFS images.
type noDataFS struct {
	inner fs.FS
}

func (n noDataFS) Open(name string) (fs.File, error) {
	f, err := n.inner.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if info.IsDir() {
		return f, nil
	}
	// For regular files return a zero-length reader to skip payload.
	return &zeroFile{File: f, info: info}, nil
}

type zeroFile struct {
	fs.File
	info fs.FileInfo
}

func (z *zeroFile) Read(p []byte) (int, error)          { return 0, io.EOF }
func (z *zeroFile) Stat() (fs.FileInfo, error)           { return z.info, nil }
func (z *zeroFile) Close() error                         { return z.File.Close() }
func (z *zeroFile) ReadDir(n int) ([]fs.DirEntry, error) { return nil, fmt.Errorf("not a directory") }

// BuildTimeFromStat extracts the mtime from a FileInfo for use as the EROFS
// build time (applied via WithBuildTime option if available).
func BuildTimeFromStat(info fs.FileInfo) time.Time {
	return info.ModTime()
}
