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
// The cache is a pure content-addressed byte store: it holds no namespace
// metadata and no garbage-collection state of its own.  Each indexed-content
// blob that is mounted lazily gets one directory keyed by its digest:
//
//	<root>/<sha256-hex>/
//	    data         — sparse uncompressed EROFS image
//	    present.bm   — per-chunk presence bitmap
//
// # Lifetime / garbage collection
//
// The cache does not participate in the metadata GC directly.  Lifetime is
// entirely governed by the indexed-content store: an indexed blob is kept
// alive by a forward reference (containerd.io/gc.ref.content-index) on the
// manifest that owns the layer.  When the indexed blob becomes unreferenced
// in every namespace, the index store's GC collector calls Cache.Remove with
// the blob digest, which deletes the on-disk cache directory.  Because the
// cache is keyed purely by digest, a blob shared across namespaces maps to a
// single cache directory that is removed only once the last namespace stops
// referencing it.
//
// In-memory ref-counting (blobState.refs) is an FD-lifecycle concern only and
// has nothing to do with GC.
package cache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/containerd/containerd/v2/core/content"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/internal/erofsmeta"
	"github.com/containerd/containerd/v2/internal/netbudget"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/log"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// detachContext returns a context derived from context.Background() carrying
// only the namespace from the input ctx (cancellation / deadlines dropped).
func detachContext(ctx context.Context) context.Context {
	bg := context.Background()
	if ns, ok := namespaces.Namespace(ctx); ok {
		bg = namespaces.WithNamespace(bg, ns)
	}
	return bg
}

// Cache manages sparse-file cache entries for indexed-content blobs, keyed by
// blob digest.
type Cache interface {
	Attach(ctx context.Context, desc ocispec.Descriptor, p contentindex.ByteProvider) (Handle, error)
}

// Remover deletes the on-disk cache for a blob digest.  The indexed-content
// store calls this from its GC collector when the blob becomes unreferenced.
type Remover interface {
	Remove(dgst digest.Digest) error
}

// Warmer drives speculative background population of the cache.
type Warmer interface {
	Warm(ctx context.Context, desc ocispec.Descriptor, p contentindex.ByteProvider) error
}

// Handle is a live reference to the cache file for one indexed-content blob.
type Handle interface {
	ReadAt(p []byte, off int64) (int, error)
	BackingFile() string
	EnsureAll(ctx context.Context) error
	EnsureRange(ctx context.Context, off, length int64) error
	Prefetch(ctx context.Context, off, length int64) error
	WarmAll(ctx context.Context) error
	AllPresent() bool
	// ResidentRanges returns the uncompressed byte ranges currently resident.
	ResidentRanges() []ByteRange
	// ChunksInRange returns metadata for every chunk whose uncompressed
	// byte range intersects [off, off+length).  Used by the daemon
	// fanotify supervisor to produce per-event trace lines that name the
	// exact chunk digests the kernel asked for — the raw material for an
	// image's empirical load-order profile.
	ChunksInRange(off, length int64) []ChunkInfo
	// Release decrements the refcount; on-disk files are NOT removed.
	Release() error
}

// ChunkInfo summarises one chunk that intersects a queried byte range.
// Offset/Length are uncompressed (cache-file) coordinates; OnBlobStart/
// OnBlobEnd are the byte range inside the on-blob (registry-side)
// payload; Digest is the chunk content digest from the chunk index.
type ChunkInfo struct {
	Index        int
	Digest       digest.Digest
	Offset       int64
	Length       int64
	OnBlobStart  int64
	OnBlobEnd    int64
	Present      bool
}

// ByteRange is a half-open [Start, End) uncompressed-byte interval.
type ByteRange struct {
	Start int64
	End   int64
}

// New returns a Cache rooted at stateRoot.
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
	blobs map[string]*blobState // keyed by digest string
}

// Attach opens (or creates) the cache file for the blob described by desc.
// The blob is addressed purely by its digest; ctx supplies the namespace used
// by the underlying index store lookups (Info/AllChunks/MissingChunks).
func (c *LocalCache) Attach(ctx context.Context, desc ocispec.Descriptor, p contentindex.ByteProvider) (Handle, error) {
	if _, err := namespaces.NamespaceRequired(ctx); err != nil {
		return nil, err
	}

	key := desc.Digest.String()
	dir := c.blobDir(digestKey(key))

	// 0755 (not 0700) so that out-of-process observers (e.g. lazy-viz running
	// as the invoking user while containerd-testenv --root runs containerd as
	// root via sudo) can introspect the bitmap and sparse data file.  The
	// cache holds decompressed EROFS image bytes — the same content already
	// readable via the registry pull — and contains no per-user secrets.
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("cache: create blob dir: %w", err)
	}

	c.mu.Lock()
	if bs, ok := c.blobs[key]; ok {
		bs.mu.Lock()
		bs.refs++
		bs.mu.Unlock()
		c.mu.Unlock()
		return &handle{bs: bs, cache: c, attachCtx: ctx}, nil
	}

	bs := &blobState{
		desc:     desc,
		provider: p,
		store:    c.store,
		cs:       c.cs,
		dir:      dir,
		refs:     1,
		inflight: make(map[int]chan error),
		budget:   netbudget.NewDefaultTracker(),
	}
	// Initialise lastForegroundChunk to -1: WarmAll's adaptive picker
	// falls back to sequential order until the first foreground event
	// lands.
	bs.lastForegroundChunk.Store(-1)
	c.blobs[key] = bs
	c.mu.Unlock()

	if err := bs.init(ctx); err != nil {
		c.mu.Lock()
		delete(c.blobs, key)
		c.mu.Unlock()
		os.RemoveAll(dir)
		return nil, fmt.Errorf("cache: init: %w", err)
	}

	log.G(ctx).WithFields(log.Fields{
		"blob":       desc.Digest,
		"num_chunks": bs.numChunks,
		"cache_dir":  dir,
	}).Info("[lazy-viz] blob_attach")

	return &handle{bs: bs, cache: c, attachCtx: ctx}, nil
}

// Remove deletes the on-disk cache directory for dgst and drops any in-memory
// state.  Called by the indexed-content store's GC collector when the blob is
// no longer referenced in any namespace.  It is a no-op if the cache does not
// exist.  Active handles, if any, keep their open file descriptors valid until
// released (POSIX unlink semantics); subsequent Attach re-materialises.
func (c *LocalCache) Remove(dgst digest.Digest) error {
	key := dgst.String()
	c.mu.Lock()
	delete(c.blobs, key)
	c.mu.Unlock()
	if err := os.RemoveAll(c.blobDir(digestKey(key))); err != nil {
		return fmt.Errorf("cache: remove %s: %w", key, err)
	}
	return nil
}

// Root returns the cache state root directory.
func (c *LocalCache) Root() string { return c.root }

func (c *LocalCache) blobDir(hexDigest string) string {
	return filepath.Join(c.root, hexDigest)
}

// Warm implements Warmer.
func (c *LocalCache) Warm(ctx context.Context, desc ocispec.Descriptor, p contentindex.ByteProvider) error {
	handle, err := c.Attach(ctx, desc, p)
	if err != nil {
		return fmt.Errorf("cache: warm attach: %w", err)
	}
	if handle.AllPresent() {
		_ = handle.Release()
		return nil
	}
	bgCtx := detachContext(ctx)
	go func() {
		defer handle.Release()
		_ = handle.WarmAll(bgCtx)
	}()
	return nil
}

// PrepareRanges synchronously warms each requested byte range into the
// sparse cache file and returns once they are resident.  Subsequent
// reads of those ranges via Handle.ReadAt (or directly from the backing
// file path) hit already-resident pages — no kernel mount, no fanotify
// supervisor, no per-read RPC.
//
// The (ranges-aware) caller is responsible for knowing WHICH ranges to
// warm.  This lets the cache stay format-agnostic: callers that know
// about EROFS metadata layouts pass the SB chunk + the inode-table
// region; callers that just want a byte range pass that range.  See
// PrepareForFSView for the EROFS-aware convenience wrapper that resolves
// the metadata range using internal/erofsmeta.
//
// Releases the temporary handle before returning.
func (c *LocalCache) PrepareRanges(ctx context.Context, desc ocispec.Descriptor, p contentindex.ByteProvider, ranges []ByteRange) error {
	h, err := c.Attach(ctx, desc, p)
	if err != nil {
		return fmt.Errorf("cache: prepare attach: %w", err)
	}
	defer h.Release()
	for _, r := range ranges {
		if r.End <= r.Start {
			continue
		}
		if err := h.EnsureRange(ctx, r.Start, r.End-r.Start); err != nil {
			return fmt.Errorf("cache: prepare [%d,%d): %w", r.Start, r.End, err)
		}
	}
	return nil
}

// PrepareForFSView synchronously warms an EROFS image's superblock and
// inode-table (metadata) region so that subsequent path-resolution reads
// — typically /etc/passwd and /etc/group via go-erofs's fs.FS over the
// backing file's io.ReaderAt — hit already-resident pages.  This is the
// core building block for replacing the spec-build kernel mount with a
// pure ReaderAt fsview pass.
//
// Implementation note: the SB lives in the first 4 KiB chunk; the inode
// table lives somewhere later in the file (often near the end for
// merged Docker layers).  We warm the SB chunk first, then read SB via
// the auto-warming Handle.ReadAt to learn meta_blkaddr, then warm the
// inode-table region.  EROFS knowledge is contained entirely in
// internal/erofsmeta; this method orchestrates two EnsureRange calls.
//
// Returns nil if the image's SB cannot be parsed as EROFS — caller
// continues without pre-fill (e.g. falls back to the daemon-side
// mount).  Returns a non-nil error only when a warm operation itself
// fails (network, chunk verify, etc.).
func (c *LocalCache) PrepareForFSView(ctx context.Context, desc ocispec.Descriptor, p contentindex.ByteProvider) error {
	h, err := c.Attach(ctx, desc, p)
	if err != nil {
		return fmt.Errorf("cache: prepare-fsview attach: %w", err)
	}
	defer h.Release()

	// Step 1: ensure the SB-bearing chunk is present.  The SB lives at
	// offset 1024 and is well under 4 KiB; EnsureRange covers it.
	if err := h.EnsureRange(ctx, 0, int64(erofsmeta.SuperBlockOffset+erofsmeta.SuperBlockSize)); err != nil {
		return fmt.Errorf("cache: prepare-fsview SB chunk: %w", err)
	}

	// Step 2: parse the SB via the handle's auto-warming ReaderAt.
	// Handle.ReadAt is itself an io.ReaderAt; erofsmeta.MetadataRange
	// re-issues EnsureRange under the hood for any chunk it touches,
	// so this is safe regardless of which chunk(s) the SB straddles.
	off, length, parseErr := erofsmeta.MetadataRange(h)
	if parseErr != nil || off <= 0 || length <= 0 {
		// Not an EROFS image, or unparseable SB — silently no-op.
		return nil
	}

	// Step 3: warm the inode-table region so dirent / inode reads from
	// path resolution succeed without further fills.
	if err := h.EnsureRange(ctx, off, length); err != nil {
		return fmt.Errorf("cache: prepare-fsview metadata [%d,%d): %w", off, off+length, err)
	}
	return nil
}

// readerAtAdapter is no longer needed — Handle.ReadAt is itself the
// io.ReaderAt interface (see handle.go).  Kept here as documentation.

// digestKey strips the algorithm prefix from a digest string.
func digestKey(digestStr string) string {
	if len(digestStr) > 7 && digestStr[6] == ':' {
		return digestStr[7:]
	}
	return digestStr
}

// BackingFilePath returns the path of the sparse data file for the given
// (stateRoot, digest) without requiring an active Attach.
func BackingFilePath(stateRoot string, dgst digest.Digest) string {
	return filepath.Join(stateRoot, digestKey(dgst.String()), "data")
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
var (
	_ Cache   = (*LocalCache)(nil)
	_ Remover = (*LocalCache)(nil)
	_ Warmer  = (*LocalCache)(nil)
)
