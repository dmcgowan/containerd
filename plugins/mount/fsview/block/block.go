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

// Package block registers an fsview.FSHandler for `type="block"` mounts
// — the lazy/sparse EROFS layout used by the block mount handler.  When
// the spec builder calls fsview.FSMounts (pkg/oci/spec_opts.go:withReadonlyFS)
// to resolve username → UID/GID from /etc/passwd / /etc/group, the
// previous behaviour was to mount the snapshot via the daemon
// MountManager.Activate path, run the lookup, and immediately
// Deactivate — a full kernel mount + bind mount round-trip per
// container start.
//
// With this handler registered, the spec builder takes a pure
// io.ReaderAt path instead: open the sparse backing file directly and
// hand it to go-erofs (github.com/erofs/go-erofs).  The
// `cache.LocalCache.PrepareForFSView` step that ran during the lazy
// pull synchronously warmed the EROFS superblock and inode-table
// region, so the small reads go-erofs performs to resolve /etc/passwd
// / /etc/group hit already-resident pages in the sparse file.  No
// kernel mount, no fanotify supervisor cycle, no MountManager round
// trip — and no second "spec-build" mount on every `ctr run`.
//
// Fallback: if erofs.Open fails (image not yet pre-warmed, sparse zero
// superblock, image isn't actually EROFS), the handler returns
// errdefs.ErrNotImplemented and the spec builder falls back to the
// MountManager-based path it always used.  This guarantees container
// startup correctness even in transient states where the pre-warm
// hasn't completed yet.
package block

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/internal/fsview"
	"github.com/containerd/errdefs"
	"github.com/erofs/go-erofs"
)

// MountType is the mount entry type this handler claims.  Matches
// plugins/mount/block.MountType.
const MountType = "block"

func init() {
	fsview.Register(fsview.FSHandler{
		HandleMount: handleMount,
		Getxattr:    getxattr,
		IsWhiteout:  isWhiteout,
	})
}

// handleMount opens the sparse backing file directly via go-erofs.  No
// kernel mount, no MountManager.Activate, no daemon RPC — just an
// io.ReaderAt over the local file.  Path-resolution reads issued by
// go-erofs go straight to the underlying file's ReadAt; the
// pre-warming step that ran during lazy pull (see
// cache.LocalCache.PrepareForFSView) made the EROFS superblock and the
// inode-table region resident in the sparse file, so the small reads
// needed to walk /etc/passwd / /etc/group hit real bytes.
func handleMount(m mount.Mount) (fsview.View, error) {
	if m.Type != MountType {
		return nil, errdefs.ErrNotImplemented
	}

	// Backing file path is the mount source — the same path the daemon
	// block handler passes to unix.Mount for the kernel mount.  Cache
	// files are mode 0644 / dirs 0755 (see plugins/cache/plugin/plugin.go
	// + core/content/index/cache/{cache,handle,bitmap}.go) so this open
	// succeeds even when the daemon runs as root and the spec builder
	// runs as the invoking user.
	f, err := os.Open(m.Source)
	if err != nil {
		// Path doesn't exist or perm denied — let the MountManager
		// path try; it has its own error surface.
		return nil, errdefs.ErrNotImplemented
	}

	efs, err := erofs.Open(f)
	if err != nil {
		// SB is sparse zeros (PrepareForFSView didn't run, or hasn't
		// finished, or the image isn't EROFS).  Fall back to the
		// kernel-mount path so container start still succeeds.
		f.Close()
		return nil, errdefs.ErrNotImplemented
	}

	rlfs, ok := efs.(fs.ReadLinkFS)
	if !ok {
		f.Close()
		return nil, fmt.Errorf("block fsview: filesystem does not implement fs.ReadLinkFS: %w", errdefs.ErrNotImplemented)
	}

	return &blockView{
		ReadLinkFS: rlfs,
		closers:    []io.Closer{f},
	}, nil
}

type blockView struct {
	fs.ReadLinkFS
	closers []io.Closer
}

func (v *blockView) Close() error {
	var errs []error
	for _, c := range v.closers {
		errs = append(errs, c.Close())
	}
	return errors.Join(errs...)
}

// getxattr mirrors the erofs fsview handler — go-erofs returns
// *erofs.Stat via fs.FileInfo.Sys(), and Xattrs is a map[string]string
// on that struct.
func getxattr(f fs.File, name string) (string, bool) {
	fi, err := f.Stat()
	if err != nil {
		return "", false
	}
	estatfi, ok := fi.Sys().(*erofs.Stat)
	if !ok {
		return "", false
	}
	val, ok := estatfi.Xattrs[name]
	return val, ok
}

// isWhiteout: EROFS encodes overlay whiteouts as character devices
// with rdev == 0.  Same semantics as the existing erofs fsview handler.
func isWhiteout(fi fs.FileInfo) bool {
	if (fi.Mode() & fs.ModeCharDevice) == 0 {
		return false
	}
	estatfi, ok := fi.Sys().(*erofs.Stat)
	if !ok {
		return false
	}
	return estatfi.Rdev == 0
}
