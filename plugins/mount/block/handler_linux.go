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

// Package block provides a containerd mount handler for the "block" mount type.
//
// A "block" mount entry has:
//
//	type   = "block"
//	source = "<blob-digest>"       // e.g. "sha256:abcd..."
//	options = ["target=erofs", "ro", ...]
//
// The handler:
//  1. Looks up the blob in the indexed content store.
//  2. Attaches the sparse-file cache (cache.Attach).
//  3. Calls EnsureAll to populate the cache (loop-delivery requires full
//     population before mounting).
//  4. Sets up a loop device over the cache's sparse file.
//  5. Mounts the target filesystem (default: erofs) over the loop device.
//
// Deactivation unmounts, detaches the loop device, and releases the cache
// handle.
package block

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sys/unix"

	"github.com/containerd/containerd/v2/core/content/index/cache"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/core/content/index/provider"
	coremount "github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/errdefs"
)

// Handler implements coremount.Handler for the "block" mount type.
type Handler struct {
	store contentindex.Store
	cache cache.Cache

	mu      sync.Mutex
	handles map[string]*activeBlock // keyed by mountpoint
}

type activeBlock struct {
	handle    cache.Handle
	loopDev   string
	digestStr string
}

// NewHandler returns a Handler that uses store to look up blobs and c as the
// sparse-file cache.
func NewHandler(store contentindex.Store, c cache.Cache) *Handler {
	return &Handler{
		store:   store,
		cache:   c,
		handles: make(map[string]*activeBlock),
	}
}

// Mount activates a "block" mount entry.
func (h *Handler) Mount(ctx context.Context, m coremount.Mount, mp string, _ []coremount.ActiveMount) (coremount.ActiveMount, error) {
	if m.Type != "block" {
		return coremount.ActiveMount{}, errdefs.ErrNotImplemented
	}

	digestStr := m.Source
	if digestStr == "" {
		return coremount.ActiveMount{}, fmt.Errorf("block: mount source (blob digest) must not be empty")
	}

	// Parse options.
	target := "erofs"
	var extraOpts []string
	for _, opt := range m.Options {
		if v, ok := strings.CutPrefix(opt, "target="); ok {
			target = v
			continue
		}
		extraOpts = append(extraOpts, opt)
	}

	dgst, err := digest.Parse(digestStr)
	if err != nil {
		return coremount.ActiveMount{}, fmt.Errorf("block: parse digest %q: %w", digestStr, err)
	}

	info, err := h.store.Info(ctx, dgst)
	if err != nil {
		return coremount.ActiveMount{}, fmt.Errorf("block: get blob info for %s: %w", digestStr, err)
	}
	desc := ocispec.Descriptor{
		MediaType: info.MediaType,
		Digest:    info.Digest,
		Size:      info.Size,
	}

	// Get the provider for this blob from the global registry.
	p, err := provider.Global.Get(info.Provider)
	if err != nil {
		return coremount.ActiveMount{}, fmt.Errorf("block: get provider %q for blob %s: %w",
			info.Provider, digestStr, err)
	}

	// Attach to the cache.
	handle, err := h.cache.Attach(ctx, desc, p)
	if err != nil {
		return coremount.ActiveMount{}, fmt.Errorf("block: cache attach %s: %w", digestStr, err)
	}

	// Eagerly fill all missing chunks.
	if err := handle.EnsureAll(ctx); err != nil {
		_ = handle.Release()
		return coremount.ActiveMount{}, fmt.Errorf("block: ensure all chunks for %s: %w", digestStr, err)
	}

	// Set up a loop device.
	loopFile, err := coremount.SetupLoop(handle.BackingFile(), coremount.LoopParams{
		Readonly:  true,
		Autoclear: true,
		Direct:    false,
	})
	if err != nil {
		_ = handle.Release()
		return coremount.ActiveMount{}, fmt.Errorf("block: setup loop for %s: %w", digestStr, err)
	}
	loopDev := loopFile.Name()
	loopFile.Close()

	// Create the mountpoint directory if it doesn't exist.
	if err := os.MkdirAll(mp, 0755); err != nil {
		_ = coremount.DetachLoopDevice(loopDev)
		_ = handle.Release()
		return coremount.ActiveMount{}, fmt.Errorf("block: create mountpoint %s: %w", mp, err)
	}

	// Mount the filesystem.
	flags := uintptr(unix.MS_RDONLY)
	opts := strings.Join(extraOpts, ",")
	if err := unix.Mount(loopDev, mp, target, flags, opts); err != nil {
		_ = coremount.DetachLoopDevice(loopDev)
		_ = handle.Release()
		return coremount.ActiveMount{}, fmt.Errorf("block: mount %s (%s) at %s: %w",
			loopDev, target, mp, err)
	}

	h.mu.Lock()
	h.handles[mp] = &activeBlock{
		handle:    handle,
		loopDev:   loopDev,
		digestStr: digestStr,
	}
	h.mu.Unlock()

	return coremount.ActiveMount{
		Mount:      m,
		MountPoint: mp,
		MountData: map[string]string{
			"block.digest":  digestStr,
			"block.loopdev": loopDev,
		},
	}, nil
}

// Unmount deactivates a "block" mount.
func (h *Handler) Unmount(_ context.Context, mp string) error {
	h.mu.Lock()
	ab, ok := h.handles[mp]
	if ok {
		delete(h.handles, mp)
	}
	h.mu.Unlock()

	var errs []string

	if err := unix.Unmount(mp, unix.MNT_DETACH); err != nil {
		errs = append(errs, fmt.Sprintf("unmount %s: %v", mp, err))
	}

	if ab != nil {
		if ab.loopDev != "" {
			if err := coremount.DetachLoopDevice(ab.loopDev); err != nil {
				errs = append(errs, fmt.Sprintf("detach loop %s: %v", ab.loopDev, err))
			}
		}
		if err := ab.handle.Release(); err != nil {
			errs = append(errs, fmt.Sprintf("cache release: %v", err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("block unmount errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Compile-time check that Handler implements coremount.Handler.
var _ coremount.Handler = (*Handler)(nil)
