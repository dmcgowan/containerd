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

package cache

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/containerd/containerd/v2/core/content"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// blobState holds the in-memory state for one cached blob.
type blobState struct {
	desc     ocispec.Descriptor
	provider contentindex.ByteProvider
	store    contentindex.Store
	cs       content.Store
	dir      string

	mu         sync.Mutex
	refs       int
	bitmap     *bitmap
	dataFile   *os.File
	inflight   map[int]chan error // per-chunkIdx fill gates
	numChunks  int
	chunkRefs  []contentindex.ChunkRef // all chunks in order
}

func (bs *blobState) dataPath() string   { return filepath.Join(bs.dir, "data") }
func (bs *blobState) bitmapPath() string { return filepath.Join(bs.dir, "present.bm") }

// init creates or reopens the sparse file and bitmap.
func (bs *blobState) init(ctx context.Context) error {
	info, err := bs.store.Info(ctx, bs.desc.Digest)
	if err != nil {
		return fmt.Errorf("get info: %w", err)
	}

	// Load ALL chunk refs (present + missing) for the sparse-file layout.
	allChunks, err := bs.store.AllChunks(ctx, bs.desc.Digest)
	if err != nil {
		return fmt.Errorf("all chunks: %w", err)
	}
	bs.numChunks = len(allChunks)
	bs.chunkRefs = allChunks

	// Load missing chunks to seed the bitmap (absent chunks = not yet in content store).
	missing, err := bs.store.MissingChunks(ctx, bs.desc.Digest)
	if err != nil {
		return fmt.Errorf("missing chunks: %w", err)
	}
	missingSet := make(map[int]bool, len(missing))
	// Build a set keyed by chunk index (position in allChunks).
	for mi := range missing {
		for ai, ac := range allChunks {
			if ac.Digest == missing[mi].Digest {
				missingSet[ai] = true
				break
			}
		}
	}

	// Build bitmap; fresh file starts all-zero; we then mark present bits.
	bm, err := openOrCreateBitmap(bs.bitmapPath(), bs.numChunks)
	if err != nil {
		return fmt.Errorf("open bitmap: %w", err)
	}
	bs.bitmap = bm

	// If the bitmap file was freshly created (all zero), mark all non-missing
	// chunks as present. If the file was reloaded from disk (restart), trust
	// the file contents — only update bits for chunks whose presence in the
	// content store has changed since the last run.
	bmpFi, _ := os.Stat(bs.bitmapPath())
	freshBitmap := bmpFi == nil || bmpFi.Size() == 0
	if freshBitmap {
		for i := range allChunks {
			if !missingSet[i] {
				bm.set(i)
			}
		}
	}
	// (On restart the bitmap on disk already has the correct bits; trust it.)

	// Open / create sparse data file.
	f, err := os.OpenFile(bs.dataPath(), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		bm.close()
		return fmt.Errorf("open data file: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		bm.close()
		return fmt.Errorf("stat data file: %w", err)
	}

	// Determine sparse file target size: uncompressed data size.
	// For v1 we use the blob's total size from info as an approximation.
	// A later version will use the chunk-index header's UncompressedSize.
	targetSize := info.Size
	if fi.Size() == 0 && targetSize > 0 {
		if err := f.Truncate(targetSize); err != nil {
			f.Close()
			bm.close()
			return fmt.Errorf("truncate data file to %d: %w", targetSize, err)
		}
	}
	bs.dataFile = f
	return nil
}

// handle is one user's reference to a blobState.
type handle struct {
	bs    *blobState
	cache *LocalCache
}

// BackingFile returns the path to the sparse data file.
func (h *handle) BackingFile() string { return h.bs.dataPath() }

// ReadAt ensures the chunks intersecting [off, off+len(p)) are present,
// then reads directly from the sparse file.
func (h *handle) ReadAt(p []byte, off int64) (int, error) {
	ctx := context.Background()
	bs := h.bs
	end := off + int64(len(p))

	for i, c := range bs.chunkRefs {
		cEnd := c.Offset + c.Length
		if c.Offset >= end || cEnd <= off {
			continue
		}
		if err := h.ensureChunk(ctx, i, contentindex.PriorityForeground); err != nil {
			return 0, fmt.Errorf("cache: ReadAt: ensure chunk %d: %w", i, err)
		}
	}

	n, err := bs.dataFile.ReadAt(p, off)
	if err == io.EOF && n == len(p) {
		return n, nil
	}
	return n, err
}

// EnsureAll fills every missing chunk (for loop-mount full-population).
func (h *handle) EnsureAll(ctx context.Context) error {
	bs := h.bs
	for i := 0; i < bs.numChunks; i++ {
		bs.mu.Lock()
		already := bs.bitmap.isSet(i)
		bs.mu.Unlock()
		if already {
			continue
		}
		if err := h.ensureChunk(ctx, i, contentindex.PriorityForeground); err != nil {
			return fmt.Errorf("cache: EnsureAll chunk %d: %w", i, err)
		}
	}
	return nil
}

// Prefetch fires background fills for chunks intersecting [off, off+length).
func (h *handle) Prefetch(_ context.Context, off, length int64) error {
	ctx := context.Background()
	end := off + length
	for i, c := range h.bs.chunkRefs {
		cEnd := c.Offset + c.Length
		if c.Offset >= end || cEnd <= off {
			continue
		}
		h.bs.mu.Lock()
		already := h.bs.bitmap.isSet(i)
		h.bs.mu.Unlock()
		if already {
			continue
		}
		idx := i
		go func() {
			_ = h.ensureChunk(ctx, idx, contentindex.PriorityBackground)
		}()
	}
	return nil
}

// Release decrements the refcount.
func (h *handle) Release() error {
	bs := h.bs
	bs.mu.Lock()
	bs.refs--
	refs := bs.refs
	var f *os.File
	var bm *bitmap
	if refs == 0 {
		f = bs.dataFile
		bm = bs.bitmap
		bs.dataFile = nil
		bs.bitmap = nil
	}
	bs.mu.Unlock()

	if refs == 0 {
		if f != nil {
			f.Close()
		}
		if bm != nil {
			bm.close()
		}
		h.cache.evict(bs.desc.Digest.String())
	}
	return nil
}

// ensureChunk coalesces concurrent fills for the same chunk index.
func (h *handle) ensureChunk(ctx context.Context, idx int, priority contentindex.Priority) error {
	bs := h.bs
	bs.mu.Lock()
	if bs.bitmap.isSet(idx) {
		bs.mu.Unlock()
		return nil
	}
	if ch, ok := bs.inflight[idx]; ok {
		bs.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-ch:
			return err
		}
	}
	ch := make(chan error, 1)
	bs.inflight[idx] = ch
	bs.mu.Unlock()

	err := h.fillChunk(ctx, idx, priority)

	bs.mu.Lock()
	delete(bs.inflight, idx)
	bs.mu.Unlock()

	ch <- err
	close(ch)
	return err
}

// fillChunk fetches the chunk via the indexed content store, then writes
// the uncompressed bytes into the sparse file.
func (h *handle) fillChunk(ctx context.Context, idx int, priority contentindex.Priority) error {
	bs := h.bs

	// Ask the indexed content store to fetch + verify + store the chunk bytes.
	if err := bs.store.FillChunk(ctx, bs.desc.Digest, idx, bs.provider, priority); err != nil {
		return fmt.Errorf("fill chunk %d: %w", idx, err)
	}

	// Find the chunk ref so we know its uncompressed offset and length.
	if idx >= len(bs.chunkRefs) {
		// Chunk not in our missing-refs list (already present before Attach).
		// Bitmap should already be set; mark it now.
		bs.mu.Lock()
		bs.bitmap.set(idx)
		bs.mu.Unlock()
		return nil
	}
	c := bs.chunkRefs[idx]

	// Read the uncompressed chunk bytes from the content store.
	ra, err := bs.cs.ReaderAt(ctx, ocispec.Descriptor{
		Digest: c.Digest,
		Size:   c.Length,
	})
	if err != nil {
		return fmt.Errorf("open content store entry for chunk %d: %w", idx, err)
	}
	defer ra.Close()

	data := make([]byte, c.Length)
	if _, err := ra.ReadAt(data, 0); err != nil && err != io.EOF {
		return fmt.Errorf("read chunk %d from content store: %w", idx, err)
	}

	// Write to the sparse file at the chunk's uncompressed offset.
	if _, err := bs.dataFile.WriteAt(data, c.Offset); err != nil {
		return fmt.Errorf("write chunk %d to sparse file: %w", idx, err)
	}
	if err := bs.dataFile.Sync(); err != nil {
		return fmt.Errorf("sync sparse file after chunk %d: %w", idx, err)
	}

	// Update in-memory bitmap.
	bs.mu.Lock()
	bs.bitmap.set(idx)
	bs.mu.Unlock()

	// Persist the updated bitmap word.
	if err := bs.bitmap.persistWord(bs.bitmapPath(), idx); err != nil {
		return fmt.Errorf("persist bitmap after chunk %d: %w", idx, err)
	}
	return nil
}

// Compile-time assertion.
var _ Handle = (*handle)(nil)
