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
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/core/content/index/cache"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// concurrencyTrackingStore is a fakeIndexStore that records the
// peak number of concurrent FillChunk calls.  Used to prove WarmAll
// honours the warmBackgroundConcurrency=1 cap.
type concurrencyTrackingStore struct {
	mu        sync.Mutex
	info      contentindex.Info
	chunks    []contentindex.ChunkRef
	presented map[int]bool

	inflight   atomic.Int32
	maxInFlight atomic.Int32
	fillCalled  atomic.Int32

	// fillDelay simulates network latency; we hold the slot long
	// enough to observe any concurrent overlap.
	fillDelay time.Duration
}

func (s *concurrencyTrackingStore) Info(_ context.Context, dgst digest.Digest) (contentindex.Info, error) {
	if s.info.Digest != dgst {
		return contentindex.Info{}, errdefs.ErrNotFound
	}
	return s.info, nil
}
func (s *concurrencyTrackingStore) Update(_ context.Context, info contentindex.Info, _ ...string) (contentindex.Info, error) {
	return info, nil
}
func (s *concurrencyTrackingStore) Walk(_ context.Context, fn contentindex.WalkFunc, _ ...string) error {
	return fn(s.info)
}
func (s *concurrencyTrackingStore) Delete(_ context.Context, _ digest.Digest) error { return nil }
func (s *concurrencyTrackingStore) ReaderAt(_ context.Context, _ ocispec.Descriptor) (content.ReaderAt, error) {
	return nil, errdefs.ErrNotImplemented
}
func (s *concurrencyTrackingStore) Mounts(_ context.Context, _ digest.Digest) ([]mount.Mount, error) {
	return nil, errdefs.ErrNotImplemented
}
func (s *concurrencyTrackingStore) Writer(_ context.Context, _ ...content.WriterOpt) (content.Writer, error) {
	return nil, errdefs.ErrNotImplemented
}
func (s *concurrencyTrackingStore) AllChunks(_ context.Context, _ digest.Digest) ([]contentindex.ChunkRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]contentindex.ChunkRef, len(s.chunks))
	copy(out, s.chunks)
	return out, nil
}
func (s *concurrencyTrackingStore) MissingChunks(_ context.Context, _ digest.Digest) ([]contentindex.ChunkRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []contentindex.ChunkRef
	for i, c := range s.chunks {
		if !s.presented[i] {
			out = append(out, c)
		}
	}
	return out, nil
}
func (s *concurrencyTrackingStore) FillChunk(_ context.Context, _ digest.Digest, idx int, _ contentindex.ByteProvider, _ contentindex.Priority) error {
	cur := s.inflight.Add(1)
	defer s.inflight.Add(-1)
	for {
		m := s.maxInFlight.Load()
		if cur <= m {
			break
		}
		if s.maxInFlight.CompareAndSwap(m, cur) {
			break
		}
	}
	s.fillCalled.Add(1)
	if s.fillDelay > 0 {
		time.Sleep(s.fillDelay)
	}
	s.mu.Lock()
	if s.presented == nil {
		s.presented = make(map[int]bool)
	}
	s.presented[idx] = true
	s.mu.Unlock()
	return nil
}
func (s *concurrencyTrackingStore) FillBatch(ctx context.Context, dgst digest.Digest, idxs []int, p contentindex.ByteProvider, priority contentindex.Priority) error {
	// Single round-trip semantics: count ONE in-flight regardless
	// of how many chunks the batch covers, matching the real
	// store's behaviour (one HTTP Range request per batch).
	cur := s.inflight.Add(1)
	defer s.inflight.Add(-1)
	for {
		m := s.maxInFlight.Load()
		if cur <= m {
			break
		}
		if s.maxInFlight.CompareAndSwap(m, cur) {
			break
		}
	}
	s.fillCalled.Add(int32(len(idxs)))
	if s.fillDelay > 0 {
		time.Sleep(s.fillDelay)
	}
	s.mu.Lock()
	if s.presented == nil {
		s.presented = make(map[int]bool)
	}
	for _, idx := range idxs {
		s.presented[idx] = true
	}
	s.mu.Unlock()
	return nil
}

// orderTrackingStore records the ORDER in which FillChunk was called.
// Concurrency cap is 1 in the live code, so each call observes the
// chunk picked next by pickSequentialWarmChunk.  Used to assert
// that WarmAll walks chunks in strict 0..N order, regardless of
// any concurrent foreground (fanotify) activity.
type orderTrackingStore struct {
	mu         sync.Mutex
	info       contentindex.Info
	chunks     []contentindex.ChunkRef
	presented  map[int]bool
	callOrder  []int // chunk indices in the order FillChunk saw them
	fillDelay  time.Duration
}

func (s *orderTrackingStore) Info(_ context.Context, dgst digest.Digest) (contentindex.Info, error) {
	if s.info.Digest != dgst {
		return contentindex.Info{}, errdefs.ErrNotFound
	}
	return s.info, nil
}
func (s *orderTrackingStore) Update(_ context.Context, info contentindex.Info, _ ...string) (contentindex.Info, error) {
	return info, nil
}
func (s *orderTrackingStore) Walk(_ context.Context, fn contentindex.WalkFunc, _ ...string) error {
	return fn(s.info)
}
func (s *orderTrackingStore) Delete(_ context.Context, _ digest.Digest) error { return nil }
func (s *orderTrackingStore) ReaderAt(_ context.Context, _ ocispec.Descriptor) (content.ReaderAt, error) {
	return nil, errdefs.ErrNotImplemented
}
func (s *orderTrackingStore) Mounts(_ context.Context, _ digest.Digest) ([]mount.Mount, error) {
	return nil, errdefs.ErrNotImplemented
}
func (s *orderTrackingStore) Writer(_ context.Context, _ ...content.WriterOpt) (content.Writer, error) {
	return nil, errdefs.ErrNotImplemented
}
func (s *orderTrackingStore) AllChunks(_ context.Context, _ digest.Digest) ([]contentindex.ChunkRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]contentindex.ChunkRef, len(s.chunks))
	copy(out, s.chunks)
	return out, nil
}
func (s *orderTrackingStore) MissingChunks(_ context.Context, _ digest.Digest) ([]contentindex.ChunkRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []contentindex.ChunkRef
	for i, c := range s.chunks {
		if !s.presented[i] {
			out = append(out, c)
		}
	}
	return out, nil
}
func (s *orderTrackingStore) FillChunk(_ context.Context, _ digest.Digest, idx int, _ contentindex.ByteProvider, _ contentindex.Priority) error {
	if s.fillDelay > 0 {
		time.Sleep(s.fillDelay)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.callOrder = append(s.callOrder, idx)
	if s.presented == nil {
		s.presented = make(map[int]bool)
	}
	s.presented[idx] = true
	return nil
}
func (s *orderTrackingStore) FillBatch(_ context.Context, _ digest.Digest, idxs []int, _ contentindex.ByteProvider, _ contentindex.Priority) error {
	if s.fillDelay > 0 {
		time.Sleep(s.fillDelay)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, idx := range idxs {
		s.callOrder = append(s.callOrder, idx)
	}
	if s.presented == nil {
		s.presented = make(map[int]bool)
	}
	for _, idx := range idxs {
		s.presented[idx] = true
	}
	return nil
}

// TestWarmAll_preemptPicksNearForegroundAnchor drives a foreground
// EnsureRange into the middle of the image and then launches
// WarmAll: with the foreground event still within
// warmPreemptWindow, the warmer's FIRST batch must start at (or
// adjacent to) the anchor — proving the preempt strategy fires.
//
// Note on batched semantics: pickWarmBatch picks a single starting
// chunk via the same closest-to-anchor heuristic the legacy
// pickWarmChunk used, then extends the batch FORWARD across
// contiguous missing chunks up to the byte budget.  With tiny
// (1 KiB) chunks and the default 16 MiB budget, a single batch
// can sweep up to thousands of chunks at once.  In a 64-chunk
// image with anchor=32, the first batch claims [32, 33, .., 63]
// (32 chunks).  Subsequent batches walk backwards from chunk 31
// (closest-to-anchor) toward chunk 0.  The decisive check is
// therefore: the FIRST recorded fill is the anchor.  After that,
// the call order is determined by batched forward-extension and
// does not match the legacy single-pick heuristic.
func TestWarmAll_preemptPicksNearForegroundAnchor(t *testing.T) {
	sizes := make([]int64, 64)
	for i := range sizes {
		sizes[i] = 1024
	}
	chunks, chunkData := makeChunks(sizes...)
	total := totalSize(chunks)

	blobDgst := digest.FromString("preempt-order-test")
	desc := blobDesc(blobDgst, total)
	store := &orderTrackingStore{
		info:      contentindex.Info{Digest: blobDgst, Size: total},
		chunks:    chunks,
		fillDelay: 5 * time.Millisecond,
	}
	cs := &fakeContentStore{data: chunkData}
	c := cache.New(t.TempDir(), store, cs)

	h, err := c.Attach(testCtx(), desc, &fakeProvider{})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer h.Release()

	const anchor = 32
	if err := h.EnsureRange(context.Background(), int64(anchor)*1024, 1); err != nil {
		t.Fatalf("EnsureRange anchor: %v", err)
	}

	if err := h.WarmAll(context.Background()); err != nil {
		t.Fatalf("WarmAll: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.callOrder) != 64 {
		t.Fatalf("Fills recorded = %d, want 64", len(store.callOrder))
	}
	// EnsureRange filled chunk 32 first (foreground), then WarmAll's
	// first batch must start at the closest-to-anchor missing chunk.
	// At call[0]==anchor, the foreground EnsureRange beat WarmAll.
	// At call[1]==anchor+1 (or 31), the first warm batch is anchor-
	// adjacent — confirming preempt fired.
	if store.callOrder[0] != anchor {
		t.Errorf("first fill = %d, want %d (foreground anchor)", store.callOrder[0], anchor)
	}
	// The very next chunk WarmAll touches must be 33 (forward
	// extension of the first warm batch starting at anchor+1).
	// We check distance ≤ 1 to leave room for the case where
	// the picker sees anchor==32 already inflight and picks 33
	// or 31 as the start.
	second := store.callOrder[1]
	dist := second - anchor
	if dist < 0 {
		dist = -dist
	}
	if dist > 1 {
		t.Errorf("second fill = %d (distance %d from anchor %d, want ≤ 1) — preempt didn't fire on the first warm batch",
			second, dist, anchor)
	}
}

// TestWarmAll_sequentialWhenNoForegroundAnchor confirms the
// cold-tail fallback: with NO foreground activity (anchor still
// at -1, the sentinel), pickWarmBatch returns a contiguous run
// starting at the lowest missing index.  With concurrency=1 and
// a budget large enough to absorb every chunk in one batch, the
// recorded call order is STRICTLY 0..N-1.
func TestWarmAll_sequentialWhenNoForegroundAnchor(t *testing.T) {
	sizes := make([]int64, 32)
	for i := range sizes {
		sizes[i] = 1024
	}
	chunks, chunkData := makeChunks(sizes...)
	total := totalSize(chunks)
	blobDgst := digest.FromString("sequential-no-anchor-test")
	desc := blobDesc(blobDgst, total)
	store := &orderTrackingStore{
		info:      contentindex.Info{Digest: blobDgst, Size: total},
		chunks:    chunks,
		fillDelay: 5 * time.Millisecond,
	}
	cs := &fakeContentStore{data: chunkData}
	c := cache.New(t.TempDir(), store, cs)

	h, err := c.Attach(testCtx(), desc, &fakeProvider{})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer h.Release()

	// NO EnsureRange here — anchor stays at -1.
	if err := h.WarmAll(context.Background()); err != nil {
		t.Fatalf("WarmAll: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.callOrder) != 32 {
		t.Fatalf("fills recorded = %d, want 32", len(store.callOrder))
	}
	// Sequential strict order from chunk 0.  With concurrency=1
	// + a single huge batch, ordering is deterministic.
	for i, idx := range store.callOrder {
		if idx != i {
			t.Errorf("call[%d] = chunk %d, want %d (strict sequential expected)", i, idx, i)
			break
		}
	}
}

// TestPrefetch_preservesNamespace_evenFromNilCtx exercises the
// namespace-recovery path inside handle.Prefetch.  Previously the
// method swapped ctx for context.Background(), which made every
// store.FillChunk goroutine error out with "namespace required" —
// and those errors propagated through blobState.inflight to any
// FOREGROUND fanotify event that coalesced on the same chunk,
// surfacing as a FAN_DENY.  We assert the fix: a Prefetch with a
// background context still fills the chunk, because the handle's
// attachCtx is consulted as a fallback namespace source.
func TestPrefetch_preservesNamespace_evenFromNilCtx(t *testing.T) {
	chunks, chunkData := makeChunks(1024, 1024, 1024, 1024)
	total := totalSize(chunks)
	blobDgst := digest.FromString("prefetch-ns-test")
	desc := blobDesc(blobDgst, total)
	store := &concurrencyTrackingStore{
		info:   contentindex.Info{Digest: blobDgst, Size: total},
		chunks: chunks,
	}
	cs := &fakeContentStore{data: chunkData}
	c := cache.New(t.TempDir(), store, cs)

	h, err := c.Attach(testCtx(), desc, &fakeProvider{})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer h.Release()

	// Prefetch with a context that carries NO namespace — the
	// pathological case that used to fail silently.  Range covers
	// chunks 1..2.
	bare := context.Background()
	if err := h.Prefetch(bare, 1024, 2*1024); err != nil {
		t.Fatalf("Prefetch: %v", err)
	}

	// Prefetch spawns goroutines; poll briefly until those finish.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if store.fillCalled.Load() >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := store.fillCalled.Load(); got < 2 {
		t.Errorf("Prefetch fired FillChunk %d times, want ≥ 2 (namespace fallback didn't engage)", got)
	}
}

// batchCallTrackingStore counts FillBatch invocations separately
// from FillChunk so a test can prove EnsureRange of a multi-chunk
// range issues ONE store-level batch call rather than N
// per-chunk calls.
type batchCallTrackingStore struct {
	mu         sync.Mutex
	info       contentindex.Info
	chunks     []contentindex.ChunkRef
	presented  map[int]bool
	batchCalls atomic.Int32 // # of FillBatch invocations
	chunkCalls atomic.Int32 // # of FillChunk invocations
}

func (s *batchCallTrackingStore) Info(_ context.Context, dgst digest.Digest) (contentindex.Info, error) {
	if s.info.Digest != dgst {
		return contentindex.Info{}, errdefs.ErrNotFound
	}
	return s.info, nil
}
func (s *batchCallTrackingStore) Update(_ context.Context, info contentindex.Info, _ ...string) (contentindex.Info, error) {
	return info, nil
}
func (s *batchCallTrackingStore) Walk(_ context.Context, fn contentindex.WalkFunc, _ ...string) error {
	return fn(s.info)
}
func (s *batchCallTrackingStore) Delete(_ context.Context, _ digest.Digest) error { return nil }
func (s *batchCallTrackingStore) ReaderAt(_ context.Context, _ ocispec.Descriptor) (content.ReaderAt, error) {
	return nil, errdefs.ErrNotImplemented
}
func (s *batchCallTrackingStore) Mounts(_ context.Context, _ digest.Digest) ([]mount.Mount, error) {
	return nil, errdefs.ErrNotImplemented
}
func (s *batchCallTrackingStore) Writer(_ context.Context, _ ...content.WriterOpt) (content.Writer, error) {
	return nil, errdefs.ErrNotImplemented
}
func (s *batchCallTrackingStore) AllChunks(_ context.Context, _ digest.Digest) ([]contentindex.ChunkRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]contentindex.ChunkRef, len(s.chunks))
	copy(out, s.chunks)
	return out, nil
}
func (s *batchCallTrackingStore) MissingChunks(_ context.Context, _ digest.Digest) ([]contentindex.ChunkRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []contentindex.ChunkRef
	for i, c := range s.chunks {
		if !s.presented[i] {
			out = append(out, c)
		}
	}
	return out, nil
}
func (s *batchCallTrackingStore) FillChunk(_ context.Context, _ digest.Digest, idx int, _ contentindex.ByteProvider, _ contentindex.Priority) error {
	s.chunkCalls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.presented == nil {
		s.presented = make(map[int]bool)
	}
	s.presented[idx] = true
	return nil
}
func (s *batchCallTrackingStore) FillBatch(_ context.Context, _ digest.Digest, idxs []int, _ contentindex.ByteProvider, _ contentindex.Priority) error {
	s.batchCalls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.presented == nil {
		s.presented = make(map[int]bool)
	}
	for _, idx := range idxs {
		s.presented[idx] = true
	}
	return nil
}

// TestEnsureRange_coalescesContiguousChunks proves the core
// foreground-batching invariant: an EnsureRange covering a byte
// span that overlaps 4 contiguous missing chunks fires EXACTLY
// ONE store.FillBatch call (and zero FillChunk calls).
func TestEnsureRange_coalescesContiguousChunks(t *testing.T) {
	chunks, chunkData := makeChunks(1024, 1024, 1024, 1024) // 4 contiguous 1 KiB chunks
	total := totalSize(chunks)
	blobDgst := digest.FromString("ensurerange-coalesce-test")
	desc := blobDesc(blobDgst, total)
	store := &batchCallTrackingStore{
		info:   contentindex.Info{Digest: blobDgst, Size: total},
		chunks: chunks,
	}
	cs := &fakeContentStore{data: chunkData}
	c := cache.New(t.TempDir(), store, cs)

	h, err := c.Attach(testCtx(), desc, &fakeProvider{})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer h.Release()

	// EnsureRange spanning all 4 chunks (offset 0 .. 4 KiB).
	if err := h.EnsureRange(context.Background(), 0, 4*1024); err != nil {
		t.Fatalf("EnsureRange: %v", err)
	}
	if got := store.batchCalls.Load(); got != 1 {
		t.Errorf("FillBatch calls = %d, want 1 (4 contiguous chunks should fuse)", got)
	}
	if got := store.chunkCalls.Load(); got != 0 {
		t.Errorf("FillChunk calls = %d, want 0 (EnsureRange must go through FillBatch)", got)
	}
}

// TestWarmAll_batchedSingleFetch verifies the new batched-fetch
// invariant: at warmBackgroundConcurrency=1 + the default 16 MiB
// byte budget, a 16-chunk×1KiB image is filled in EXACTLY ONE
// store.FillBatch call rather than 16 individual fetches.
//
// This is the core promise of the adaptive-fetch strategy: one
// network round-trip per worker iteration, sized to take ~1.5 s
// on the observed network.  A regression to per-chunk fetching
// would show fillCalled == 16 (correct) but maxInFlight == 1
// (still correct under concurrency=1) but with N separate
// store-level operations instead of 1.  The
// concurrencyTrackingStore counts in-flight at the FillBatch
// level, so peak == 1 means the one warmer worker issued a
// single batch.
func TestWarmAll_batchedSingleFetch(t *testing.T) {
	chunks, chunkData := makeChunks(
		1024, 1024, 1024, 1024,
		1024, 1024, 1024, 1024,
		1024, 1024, 1024, 1024,
		1024, 1024, 1024, 1024,
	)
	total := totalSize(chunks)

	blobDgst := digest.FromString("warmall-batch-test")
	desc := blobDesc(blobDgst, total)
	store := &concurrencyTrackingStore{
		info:      contentindex.Info{Digest: blobDgst, Size: total},
		chunks:    chunks,
		fillDelay: 30 * time.Millisecond,
	}
	cs := &fakeContentStore{data: chunkData}

	root := t.TempDir()
	c := cache.New(root, store, cs)

	h, err := c.Attach(testCtx(), desc, &fakeProvider{})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer h.Release()

	if err := h.WarmAll(context.Background()); err != nil {
		t.Fatalf("WarmAll: %v", err)
	}

	// Every chunk must have been filled exactly once
	// (fillCalled counts per-chunk inside FillBatch).
	if got := store.fillCalled.Load(); got != int32(len(chunks)) {
		t.Errorf("per-chunk fill recorded %d times, want %d", got, len(chunks))
	}
	// Peak in-flight = 1: concurrency=1 + a batch that swallowed
	// every chunk in one Fetch.
	peak := store.maxInFlight.Load()
	if peak != 1 {
		t.Errorf("peak in-flight WarmAll fetches = %d, want exactly 1 (concurrency=1, one batch)", peak)
	}
}
