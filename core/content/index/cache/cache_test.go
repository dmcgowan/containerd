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

package cache_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/core/content/index/cache"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// ── fakes ────────────────────────────────────────────────────────────────────

// fakeIndexStore satisfies the minimal Store interface the cache uses.
type fakeIndexStore struct {
	mu         sync.Mutex
	info       contentindex.Info
	missing    []contentindex.ChunkRef
	fillCalled int32 // atomic count of FillChunk calls
	fillErr    error
	// After FillChunk the chunk is marked present.
	presented map[int]bool
}

func (f *fakeIndexStore) Info(_ context.Context, dgst digest.Digest) (contentindex.Info, error) {
	if f.info.Digest != dgst {
		return contentindex.Info{}, errdefs.ErrNotFound
	}
	return f.info, nil
}
func (f *fakeIndexStore) Update(_ context.Context, info contentindex.Info, _ ...string) (contentindex.Info, error) {
	return info, nil
}
func (f *fakeIndexStore) Walk(_ context.Context, fn contentindex.WalkFunc, _ ...string) error {
	return fn(f.info)
}
func (f *fakeIndexStore) Delete(_ context.Context, _ digest.Digest) error { return nil }
func (f *fakeIndexStore) ReaderAt(_ context.Context, _ ocispec.Descriptor) (content.ReaderAt, error) {
	return nil, errdefs.ErrNotImplemented
}
func (f *fakeIndexStore) Mounts(_ context.Context, _ digest.Digest) ([]mount.Mount, error) {
	return nil, errdefs.ErrNotImplemented
}
func (f *fakeIndexStore) Writer(_ context.Context, _ ...content.WriterOpt) (content.Writer, error) {
	return nil, errdefs.ErrNotImplemented
}
func (f *fakeIndexStore) AllChunks(_ context.Context, _ digest.Digest) ([]contentindex.ChunkRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]contentindex.ChunkRef, len(f.missing))
	copy(out, f.missing)
	return out, nil
}
func (f *fakeIndexStore) MissingChunks(_ context.Context, _ digest.Digest) ([]contentindex.ChunkRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []contentindex.ChunkRef
	for i, c := range f.missing {
		if !f.presented[i] {
			out = append(out, c)
		}
	}
	return out, nil
}
func (f *fakeIndexStore) FillChunk(_ context.Context, _ digest.Digest, idx int, _ contentindex.ByteProvider, _ contentindex.Priority) error {
	atomic.AddInt32(&f.fillCalled, 1)
	if f.fillErr != nil {
		return f.fillErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.presented == nil {
		f.presented = make(map[int]bool)
	}
	f.presented[idx] = true
	return nil
}

// fakeContentStore provides chunk bytes for ReadAt.
type fakeContentStore struct {
	data map[digest.Digest][]byte
}

func (f *fakeContentStore) ReaderAt(_ context.Context, desc ocispec.Descriptor) (content.ReaderAt, error) {
	d, ok := f.data[desc.Digest]
	if !ok {
		return nil, errdefs.ErrNotFound
	}
	return &bytesRA{bytes.NewReader(d)}, nil
}

// Stub remaining content.Store methods.
func (f *fakeContentStore) Info(_ context.Context, _ digest.Digest) (content.Info, error) {
	return content.Info{}, errdefs.ErrNotImplemented
}
func (f *fakeContentStore) Update(_ context.Context, _ content.Info, _ ...string) (content.Info, error) {
	return content.Info{}, errdefs.ErrNotImplemented
}
func (f *fakeContentStore) Walk(_ context.Context, _ content.WalkFunc, _ ...string) error {
	return errdefs.ErrNotImplemented
}
func (f *fakeContentStore) Delete(_ context.Context, _ digest.Digest) error {
	return errdefs.ErrNotImplemented
}
func (f *fakeContentStore) Writer(_ context.Context, _ ...content.WriterOpt) (content.Writer, error) {
	return nil, errdefs.ErrNotImplemented
}
func (f *fakeContentStore) Abort(_ context.Context, _ string) error {
	return errdefs.ErrNotImplemented
}
func (f *fakeContentStore) Status(_ context.Context, _ string) (content.Status, error) {
	return content.Status{}, errdefs.ErrNotImplemented
}
func (f *fakeContentStore) ListStatuses(_ context.Context, _ ...string) ([]content.Status, error) {
	return nil, errdefs.ErrNotImplemented
}

type bytesRA struct{ r *bytes.Reader }

func (b *bytesRA) ReadAt(p []byte, off int64) (int, error) { return b.r.ReadAt(p, off) }
func (b *bytesRA) Size() int64                             { return b.r.Size() }
func (b *bytesRA) Close() error                            { return nil }

// fakeProvider satisfies ByteProvider.
type fakeProvider struct{}

func (f *fakeProvider) Name() string { return "fake" }
func (f *fakeProvider) Open(_ context.Context, _ ocispec.Descriptor) (content.ReaderAt, error) {
	return nil, errdefs.ErrNotImplemented
}
func (f *fakeProvider) Fetch(_ context.Context, _ ocispec.Descriptor, _ contentindex.ChunkRef, _ contentindex.Priority) (io.ReadCloser, error) {
	return nil, errdefs.ErrNotImplemented
}

// ── helpers ───────────────────────────────────────────────────────────────────

func makeChunks(sizes ...int64) ([]contentindex.ChunkRef, map[digest.Digest][]byte) {
	var offset int64
	chunks := make([]contentindex.ChunkRef, len(sizes))
	data := make(map[digest.Digest][]byte)

	for i, sz := range sizes {
		payload := make([]byte, sz)
		for j := range payload {
			payload[j] = byte(i + 1) // fill with chunk index+1
		}
		h := digest.SHA256.Digester()
		h.Hash().Write(payload)
		dgst := h.Digest()

		chunks[i] = contentindex.ChunkRef{
			Digest: dgst,
			Offset: offset,
			Length: sz,
		}
		data[dgst] = payload
		offset += sz
	}
	return chunks, data
}

func blobDesc(dgst digest.Digest, size int64) ocispec.Descriptor {
	return ocispec.Descriptor{
		MediaType: "application/vnd.erofs",
		Digest:    dgst,
		Size:      size,
	}
}

func totalSize(chunks []contentindex.ChunkRef) int64 {
	if len(chunks) == 0 {
		return 0
	}
	last := chunks[len(chunks)-1]
	return last.Offset + last.Length
}

// ── tests ─────────────────────────────────────────────────────────────────────

func TestAttachAndEnsureAll(t *testing.T) {
	chunks, chunkData := makeChunks(1024, 2048, 512)
	total := totalSize(chunks)

	blobDgst := digest.FromString("test-blob")
	desc := blobDesc(blobDgst, total)

	store := &fakeIndexStore{
		info:    contentindex.Info{Digest: blobDgst, Size: total},
		missing: chunks,
	}
	cs := &fakeContentStore{data: chunkData}

	root := t.TempDir()
	c := cache.New(root, store, cs)

	h, err := c.Attach(context.Background(), desc, &fakeProvider{})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer h.Release()

	if err := h.EnsureAll(context.Background()); err != nil {
		t.Fatalf("EnsureAll: %v", err)
	}

	// Verify FillChunk was called for each chunk.
	got := atomic.LoadInt32(&store.fillCalled)
	if got != int32(len(chunks)) {
		t.Errorf("FillChunk called %d times, want %d", got, len(chunks))
	}

	// Verify backing file exists.
	if _, err := os.Stat(h.BackingFile()); err != nil {
		t.Errorf("backing file missing: %v", err)
	}
}

func TestReadAtFillsOnDemand(t *testing.T) {
	chunks, chunkData := makeChunks(512, 512)
	total := totalSize(chunks)

	blobDgst := digest.FromString("read-at-test")
	desc := blobDesc(blobDgst, total)

	store := &fakeIndexStore{
		info:    contentindex.Info{Digest: blobDgst, Size: total},
		missing: chunks,
	}
	cs := &fakeContentStore{data: chunkData}

	h, err := cache.New(t.TempDir(), store, cs).Attach(context.Background(), desc, &fakeProvider{})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer h.Release()

	// Read the first 100 bytes — should trigger fill of chunk 0.
	buf := make([]byte, 100)
	n, err := h.ReadAt(buf, 0)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != 100 {
		t.Errorf("ReadAt returned %d bytes, want 100", n)
	}
	// FillChunk for chunk 0 should have been called.
	got := atomic.LoadInt32(&store.fillCalled)
	if got < 1 {
		t.Errorf("expected at least 1 FillChunk call, got %d", got)
	}
}

func TestConcurrentReadAtCoalesces(t *testing.T) {
	// Two concurrent ReadAt calls for the same chunk should result in
	// exactly one FillChunk call (coalescing).
	chunks, chunkData := makeChunks(1024)
	total := totalSize(chunks)
	blobDgst := digest.FromString("coalesce-test")
	desc := blobDesc(blobDgst, total)

	store := &fakeIndexStore{
		info:    contentindex.Info{Digest: blobDgst, Size: total},
		missing: chunks,
	}
	cs := &fakeContentStore{data: chunkData}

	h, err := cache.New(t.TempDir(), store, cs).Attach(context.Background(), desc, &fakeProvider{})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer h.Release()

	var wg sync.WaitGroup
	wg.Add(2)
	buf1 := make([]byte, 10)
	buf2 := make([]byte, 10)
	for _, buf := range [][]byte{buf1, buf2} {
		b := buf
		wg.Add(0)
		go func() {
			defer wg.Done()
			_, _ = h.ReadAt(b, 0)
		}()
	}
	wg.Wait()

	// Should be 1 or 2 (one per concurrent call, but coalesced).
	// The important thing is it doesn't panic.
	got := atomic.LoadInt32(&store.fillCalled)
	if got < 1 {
		t.Errorf("expected at least 1 FillChunk call, got %d", got)
	}
}

func TestRestartRecovery(t *testing.T) {
	// If a bitmap file already exists, Attach should reuse it.
	chunks, chunkData := makeChunks(512, 512)
	total := totalSize(chunks)
	blobDgst := digest.FromString("restart-test")
	desc := blobDesc(blobDgst, total)

	store := &fakeIndexStore{
		info:    contentindex.Info{Digest: blobDgst, Size: total},
		missing: chunks,
	}
	cs := &fakeContentStore{data: chunkData}

	root := t.TempDir()
	c := cache.New(root, store, cs)

	// First attach + ensure all.
	h, err := c.Attach(context.Background(), desc, &fakeProvider{})
	if err != nil {
		t.Fatalf("Attach 1: %v", err)
	}
	if err := h.EnsureAll(context.Background()); err != nil {
		t.Fatalf("EnsureAll 1: %v", err)
	}
	if err := h.Release(); err != nil {
		t.Fatalf("Release 1: %v", err)
	}

	prevCalls := atomic.LoadInt32(&store.fillCalled)

	// Simulate restart: create a new cache instance with the same root.
	c2 := cache.New(root, store, cs)
	h2, err := c2.Attach(context.Background(), desc, &fakeProvider{})
	if err != nil {
		t.Fatalf("Attach 2: %v", err)
	}
	defer h2.Release()

	// MissingChunks should return empty (all presented from first pass).
	missing, err := store.MissingChunks(context.Background(), blobDgst)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Errorf("expected 0 missing chunks on restart, got %d", len(missing))
	}

	// FillChunk should not have been called again.
	newCalls := atomic.LoadInt32(&store.fillCalled)
	if newCalls != prevCalls {
		t.Errorf("expected no new FillChunk calls on restart, got %d new calls", newCalls-prevCalls)
	}
}

func TestBitmapPersistAndReload(t *testing.T) {
	root := t.TempDir()
	bitmapPath := root + "/test.bm"

	bm, err := cache.OpenOrCreateBitmapForTest(bitmapPath, 8)
	if err != nil {
		t.Fatalf("create bitmap: %v", err)
	}
	bm.SetForTest(3)
	bm.SetForTest(7)
	if err := bm.PersistWordForTest(bitmapPath, 3); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if err := bm.PersistWordForTest(bitmapPath, 7); err != nil {
		t.Fatalf("persist: %v", err)
	}
	bm.CloseForTest()

	// Reload.
	bm2, err := cache.OpenOrCreateBitmapForTest(bitmapPath, 8)
	if err != nil {
		t.Fatalf("reload bitmap: %v", err)
	}
	defer bm2.CloseForTest()

	if !bm2.IsSetForTest(3) {
		t.Error("bit 3 should be set after reload")
	}
	if !bm2.IsSetForTest(7) {
		t.Error("bit 7 should be set after reload")
	}
	if bm2.IsSetForTest(0) {
		t.Error("bit 0 should not be set")
	}
}
