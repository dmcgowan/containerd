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

// Package cache implements the sparse-file cache for lazy-loaded EROFS blobs.
//
// Each indexed-content blob that is mounted lazily gets one sparse file per
// blob holding the uncompressed image bytes. Holes in the file correspond to
// chunks not yet fetched. A sidecar bitmap file tracks which chunks are
// present for restart recovery.
//
// See designs/cache.md for the full design rationale.
package cache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/containerd/containerd/v2/core/content"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Cache manages sparse-file cache entries for indexed-content blobs.
type Cache interface {
	// Attach opens (or creates) the cache file for the blob described by desc.
	// The provider p is used to fetch missing chunks. Refcount is incremented.
	Attach(ctx context.Context, desc ocispec.Descriptor, p contentindex.ByteProvider) (Handle, error)
}

// Handle is a live reference to the cache file for one indexed-content blob.
type Handle interface {
	// ReadAt satisfies reads by ensuring chunks are present (filling
	// at PriorityForeground if not), then reading from the sparse file.
	ReadAt(p []byte, off int64) (int, error)

	// BackingFile returns the absolute path to the sparse file.
	BackingFile() string

	// EnsureAll fills every missing chunk. Used by the loop adapter to
	// fully populate the file before mounting. Blocks until complete.
	EnsureAll(ctx context.Context) error

	// Prefetch fires background fills for chunks intersecting [off, off+length).
	Prefetch(ctx context.Context, off, length int64) error

	// Release decrements the refcount; in-memory state is evicted at 0.
	Release() error
}

// New returns a Cache rooted at stateRoot.
// store is used for MissingChunks and FillChunk; cs is used to read chunk
// bytes from the content store after a FillChunk call completes.
func New(stateRoot string, store contentindex.Store, cs content.Store) *LocalCache {
	return &LocalCache{
		root:  stateRoot,
		store: store,
		cs:    cs,
		blobs: make(map[string]*blobState),
	}
}

// LocalCache is the concrete implementation of Cache.
type LocalCache struct {
	root  string
	store contentindex.Store
	cs    content.Store

	mu    sync.Mutex
	blobs map[string]*blobState // keyed by blob digest string
}

// Attach implements Cache.
func (c *LocalCache) Attach(ctx context.Context, desc ocispec.Descriptor, p contentindex.ByteProvider) (Handle, error) {
	key := desc.Digest.String()

	c.mu.Lock()
	if bs, ok := c.blobs[key]; ok {
		bs.mu.Lock()
		bs.refs++
		bs.mu.Unlock()
		c.mu.Unlock()
		return &handle{bs: bs, cache: c}, nil
	}

	dir := c.blobDir(key)
	if err := os.MkdirAll(dir, 0700); err != nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("cache: create blob dir: %w", err)
	}

	bs := &blobState{
		desc:     desc,
		provider: p,
		store:    c.store,
		cs:       c.cs,
		dir:      dir,
		refs:     1,
		inflight: make(map[int]chan error),
	}
	c.blobs[key] = bs
	c.mu.Unlock()

	if err := bs.init(ctx); err != nil {
		c.mu.Lock()
		delete(c.blobs, key)
		c.mu.Unlock()
		os.RemoveAll(dir)
		return nil, fmt.Errorf("cache: init: %w", err)
	}

	return &handle{bs: bs, cache: c}, nil
}

func (c *LocalCache) blobDir(digestStr string) string {
	key := digestStr
	if len(digestStr) > 7 && digestStr[6] == ':' {
		key = digestStr[7:]
	}
	return filepath.Join(c.root, key)
}

func (c *LocalCache) evict(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if bs, ok := c.blobs[key]; ok {
		bs.mu.Lock()
		refs := bs.refs
		bs.mu.Unlock()
		if refs == 0 {
			delete(c.blobs, key)
		}
	}
}

// Compile-time assertions.
var _ Cache = (*LocalCache)(nil)
