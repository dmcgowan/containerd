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
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/internal/netbudget"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/log"
	"github.com/klauspost/compress/zstd"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// (foregroundCooldown was removed in 2026-06.  WarmAll runs at
// warmBackgroundConcurrency=1 and the registry provider's fgReserve
// guarantees foreground events get a slot — so a per-event cooldown
// is redundant and was actively harmful: under a busy fanotify
// workload, lastForeground updated faster than the cooldown window,
// and the warmer permanently parked.  Result: no yellow chunks in
// the visualizer; every fault appeared as a foreground "downloading"
// event.  lastForeground is still updated by ensureChunk in case a
// future caller wants to consult it for adaptive policies.)

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

	// lastForeground is the unix-nano time of the most recent
	// PriorityForeground ensureChunk call.  Kept for diagnostics and
	// future policy decisions; the active warmer no longer gates on
	// it (see the comment above the deleted foregroundCooldown const).
	lastForeground atomic.Int64

	// lastForegroundChunk is the most recently foreground-fetched
	// chunk INDEX.  Recorded for observability and possible future
	// use; WarmAll no longer consults it.  The warmer runs in
	// strict sequential order so its progress is decoupled from
	// (and visible alongside) the live fanotify access pattern —
	// see the comment on WarmAll for the full rationale.
	// Initial -1 = "no foreground activity yet"; the atomic is
	// stamped by ensureChunk on every PriorityForeground call.
	lastForegroundChunk atomic.Int64

	// warmActive guards against multiple concurrent WarmAll loops
	// for the same blobState.  Two paths spawn WarmAll today:
	//   1. cache.Warm() from the lazy-pull path (transient — the
	//      goroutine dies on daemon restart),
	//   2. newDaemonSupervisor() at mount time (bound to the
	//      mount's lifecycle — restart-safe).
	// Both call sites are correct independently; we just don't want
	// to pay the cost of two serialised warmers running in lockstep
	// when only one is needed.  A 0→1 CAS lets the first caller
	// run; subsequent calls return immediately as a no-op until the
	// first one exits (flag reset in WarmAll's defer).
	warmActive atomic.Bool

	// budget is the per-blob adaptive byte-budget tracker.  Every
	// completed fetch (foreground EnsureRange or background WarmAll
	// batch) feeds back observed (bytes, duration) so subsequent
	// batches are sized to the network's actual throughput.  See
	// internal/netbudget for the EWMA semantics.  Initialised in
	// blobState.init.
	budget *netbudget.Tracker
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

	// Open / create sparse data file.  Mode 0644 (not 0600) so that
	// out-of-process observers — notably lazy-viz, which runs as the
	// invoking user while containerd may be running as root under
	// containerd-testenv --root — can SEEK_DATA/SEEK_HOLE the sparse
	// extents to render the 4 KiB block-density strip.  Contents are
	// already publicly-derivable from the registry-fetched layer.
	f, err := os.OpenFile(bs.dataPath(), os.O_RDWR|os.O_CREATE, 0644)
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

	// Determine sparse file target size: uncompressed data size.  The
	// cache file mirrors the DECOMPRESSED EROFS image so the kernel can
	// loop-mount it directly.  Prefer info.UncompressedSize (recorded
	// from the chunk-index header at lazy-ingest time); fall back to
	// info.Size for raw (non-zstd) layers where they are equivalent.
	targetSize := info.UncompressedSize
	if targetSize == 0 {
		targetSize = info.Size
	}
	if fi.Size() == 0 && targetSize > 0 {
		if err := f.Truncate(targetSize); err != nil {
			f.Close()
			bm.close()
			return fmt.Errorf("truncate data file to %d: %w", targetSize, err)
		}
	}
	bs.dataFile = f

	// Re-stat the file so size reflects any Truncate we just did.  This
	// is the value the log line below advertises to out-of-process
	// observers (ctrdscope) — they reuse it as the SEEK_DATA/SEEK_HOLE
	// upper bound for the sparse density strip without inferring it
	// from the chunk-index.
	size := int64(0)
	if fi2, statErr := f.Stat(); statErr == nil {
		size = fi2.Size()
	}

	// Emit the cache_file_ready trace line.  This is the canonical link
	// between an indexed-content blob (named by its chunk-index entry's
	// content-store digest, info.IndexDigest) and the local cache files
	// that materialise its decompressed bytes.  Downstream observability
	// tools — notably contrib/ctrdscope — consume this line to learn
	// the exact data_path / bitmap_path / size to poll, instead of
	// inferring them by walking the cache root + applying digest→path
	// conventions.  Carrying index_digest here also gives downstream
	// tooling a stable identifier that follows the same blob across
	// cache evictions and re-attaches (the data_path is keyed on the
	// layer digest, which is unchanged, but the index_digest is what
	// uniquely identifies the chunk-index payload — the blob's "lazy
	// manifest").
	log.G(ctx).WithFields(log.Fields{
		"blob":         bs.desc.Digest.String(),
		"index_digest": info.IndexDigest.String(),
		"data_path":    bs.dataPath(),
		"bitmap_path":  bs.bitmapPath(),
		"size":         size,
		"num_chunks":   bs.numChunks,
	}).Info("[lazy-viz] cache_file_ready")
	return nil
}

// handle is one user's reference to a blobState.
type handle struct {
	bs    *blobState
	cache *LocalCache
	// attachCtx is the context (with namespace) from the Attach call.
	// It is used by ReadAt, which cannot take a context argument due to the
	// io.ReaderAt interface contract.
	attachCtx context.Context
}

// BackingFile returns the path to the sparse data file.
func (h *handle) BackingFile() string { return h.bs.dataPath() }

// ReadAt ensures the chunks intersecting [off, off+len(p)) are present,
// then reads directly from the sparse file.
func (h *handle) ReadAt(p []byte, off int64) (int, error) {
	// Use the context from Attach so namespace/cancellation propagate correctly.
	ctx := h.attachCtx
	if ctx == nil {
		ctx = context.Background()
	}
	if err := h.EnsureRange(ctx, off, int64(len(p))); err != nil {
		return 0, err
	}
	n, err := h.bs.dataFile.ReadAt(p, off)
	if err == io.EOF && n == len(p) {
		return n, nil
	}
	return n, err
}

// EnsureRange fills every chunk whose uncompressed byte range intersects
// [off, off+length) without reading data back. The off/length arguments are
// uncompressed coordinates — positions in the cache file, which holds the
// decompressed EROFS image so the kernel can mount it directly.
//
// Contiguous missing chunks within the range are COALESCED into a single
// provider Fetch via fillBatch.  A typical foreground fanotify event
// covers tens of KiB and overlaps 1–4 chunks; batching them collapses
// what would otherwise be 4 serialised HTTP Range requests into one,
// dramatically improving foreground-fault latency.  No read-ahead
// beyond the requested range — that's the supervisor's job
// (predictive Prefetch(off+length, 1)).
func (h *handle) EnsureRange(ctx context.Context, off, length int64) error {
	bs := h.bs
	end := off + length
	// Collect intersecting chunk indices in order.
	var intersecting []int
	for i, c := range bs.chunkRefs {
		cEnd := c.Offset + c.Length
		if c.Offset >= end || cEnd <= off {
			continue
		}
		intersecting = append(intersecting, i)
	}
	if len(intersecting) == 0 {
		return nil
	}
	// Walk in maximal contiguous (on-blob) sub-runs.  A sub-run
	// breaks at the first non-contiguous chunk pair OR when the
	// sub-run already covers all intersecting chunks.
	runStart := 0
	for runStart < len(intersecting) {
		runEnd := runStart + 1
		for runEnd < len(intersecting) {
			prev := bs.chunkRefs[intersecting[runEnd-1]]
			cur := bs.chunkRefs[intersecting[runEnd]]
			if prev.OnBlobEnd != cur.OnBlobStart {
				break
			}
			runEnd++
		}
		if err := h.fillBatch(ctx, intersecting[runStart:runEnd], contentindex.PriorityForeground); err != nil {
			return fmt.Errorf("cache: EnsureRange: chunks %d..%d: %w",
				intersecting[runStart], intersecting[runEnd-1], err)
		}
		runStart = runEnd
	}
	return nil
}

// AllPresent returns true when every chunk is resident in the backing file.
func (h *handle) AllPresent() bool {
	bs := h.bs
	bs.mu.Lock()
	defer bs.mu.Unlock()
	for i := 0; i < bs.numChunks; i++ {
		if !bs.bitmap.isSet(i) {
			return false
		}
	}
	return true
}

// defaultEnsureConcurrency is the number of chunks fetched in parallel by
// EnsureAll.  Bounded to avoid overwhelming the registry with hundreds of
// simultaneous requests for large images (e.g. pytorch with 600 chunks).
// Set to 2× the number of logical CPUs, clamped to [4, 16].
var defaultEnsureConcurrency = func() int {
	n := runtime.NumCPU() * 2
	if n < 4 {
		n = 4
	}
	if n > 16 {
		n = 16
	}
	return n
}()

// EnsureAll fills every missing chunk concurrently before a loop-device
// mount.  It uses a bounded worker pool (defaultEnsureConcurrency workers)
// so that large images (hundreds of chunks) don't either serialize all
// downloads or flood the registry with unbounded parallelism.
//
// All chunks are fetched at PriorityForeground so they preempt any
// background warm goroutines already in flight.
func (h *handle) EnsureAll(ctx context.Context) error {
	bs := h.bs

	// Collect indices of chunks that still need filling.
	var missing []int
	for i := 0; i < bs.numChunks; i++ {
		bs.mu.Lock()
		already := bs.bitmap.isSet(i)
		bs.mu.Unlock()
		if !already {
			missing = append(missing, i)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	concurrency := defaultEnsureConcurrency
	if concurrency > len(missing) {
		concurrency = len(missing)
	}

	type result struct {
		idx int
		err error
	}

	work := make(chan int, len(missing))
	for _, idx := range missing {
		work <- idx
	}
	close(work)

	results := make(chan result, len(missing))
	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range work {
				err := h.ensureChunk(ctx, idx, contentindex.PriorityForeground)
				results <- result{idx, err}
				if err != nil {
					return // drain will stop once ctx cancels
				}
			}
		}()
	}
	go func() { wg.Wait(); close(results) }()

	for r := range results {
		if r.err != nil {
			return fmt.Errorf("cache: EnsureAll chunk %d: %w", r.idx, r.err)
		}
	}
	return nil
}

// warmBackgroundConcurrency is the number of parallel workers
// WarmAll spawns.  Set to 1 after the introduction of adaptive
// batched fetches (see fillBatch + netbudget).  Rationale:
//
//   - Each batch is sized to take ~1.5 s on the observed network
//     (default 16 MiB on a fresh tracker, growing on fast links,
//     shrinking on slow ones).  One batch usually saturates the
//     TCP connection's bandwidth-delay product, so additional
//     workers add HTTP framing overhead without throughput.
//   - With 4 workers × 16 MiB the cache would buffer 64 MiB of
//     in-flight bytes — at 1 worker that drops to 16 MiB,
//     reducing memory pressure on small daemons and the
//     allocator-visible spike during pull-then-run.
//   - Foreground (fanotify) events are NOT throttled by this
//     concurrency cap: they take the provider's fgReserve slot,
//     which is independent of the worker count.
//
// pickWarmBatch still reorders candidates dynamically: when a
// foreground fanotify event has fired recently, the next batch
// starts at the missing chunk CLOSEST to the foreground anchor —
// preempting the cold-tail fill to pull the warmer's focus toward
// where the workload is reading.  When foreground activity goes
// idle, the picker falls back to sequential 0..N-1.
const warmBackgroundConcurrency = 1

// warmPreemptWindow is how long after a foreground event the
// warmer's picker keeps biasing toward chunks near the foreground
// anchor.  After this window the picker reverts to strict
// sequential.  Empirically 2 s is enough to cover a single
// fanotify burst (multiple events from one container-start phase),
// while letting the warmer drain the cold tail during quiet
// periods.
const warmPreemptWindow = 2 * time.Second

// WarmAll fills every missing chunk at PriorityBackground using
// warmBackgroundConcurrency parallel workers and adaptive batched
// fetches (pickWarmBatch + fillBatch):
//
//   - When foreground (fanotify) activity is recent — i.e. the
//     last PriorityForeground ensureChunk/EnsureRange call landed
//     within warmPreemptWindow — the next batch starts at the
//     missing chunk numerically closest to the foreground
//     anchor.  This is the PREEMPTION: the warmer's normal
//     sequential progression is interrupted to pull chunks near
//     where the workload is reading, so adjacent chunks are
//     warm-loaded before the next fanotify fault hits them.
//   - Otherwise the next batch starts at the lowest missing index
//     (strict sequential 0..N-1).  This is the cold-tail fill
//     that makes the warmer's progress predictable and visible
//     during idle periods.
//   - From the starting index, the batch is extended forward
//     across contiguous (on-blob) missing chunks until the byte
//     budget (blobState.budget.Budget()) is reached.  Each batch
//     is ONE HTTP Range request via fillBatch.
//   - The picker SKIPS chunks already in blobState.inflight so
//     concurrent workers (if concurrency > 1) don't pile up on
//     the same chunk.
//
// Foreground EnsureRange runs on the registry provider's
// fgReserve slot (separate from the shared pool the warmer
// workers contend for), so foreground fanotify events never wait
// for a warmer slot — they preempt at the network layer.
func (h *handle) WarmAll(ctx context.Context) error {
	// Idempotency gate: only one WarmAll dispatcher per blobState
	// at a time.  Pull-time cache.Warm() and supervisor-time spawn
	// paths both call here; the second caller observes
	// warmActive==true and returns nil immediately, leaving the
	// first dispatcher to finish driving its worker pool.
	if !h.bs.warmActive.CompareAndSwap(false, true) {
		return nil
	}
	defer h.bs.warmActive.Store(false)

	var wg sync.WaitGroup
	errOnce := sync.Once{}
	var firstErr error

	for w := 0; w < warmBackgroundConcurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if err := ctx.Err(); err != nil {
					return
				}
				batch, ok := h.pickWarmBatch()
				if !ok {
					// No claimable chunks right now.  Either
					// the bitmap is fully set or every still-
					// missing chunk is in flight elsewhere.
					return
				}
				if err := h.fillBatch(ctx, batch, contentindex.PriorityBackground); err != nil {
					errOnce.Do(func() {
						firstErr = fmt.Errorf("cache: WarmAll batch %d..%d: %w",
							batch[0], batch[len(batch)-1], err)
					})
					return
				}
			}
		}()
	}
	wg.Wait()
	return firstErr
}

// pickWarmBatch returns the next contiguous run of missing,
// not-already-in-flight chunk indices for the warmer.  The
// starting chunk follows the same hybrid strategy as the old
// pickWarmChunk:
//
//   - Preempt mode (foreground event within warmPreemptWindow):
//     start at the missing chunk numerically closest to the
//     foreground anchor.
//   - Sequential fallback: start at the lowest missing index.
//
// From the start, the batch grows forward by ONE chunk at a time
// as long as:
//   - the next index is the immediate successor (idx+1),
//   - it is missing (bitmap not set),
//   - it is not in flight,
//   - it is on-blob-contiguous with the previous chunk
//     (chunkRefs[prev].OnBlobEnd == chunkRefs[next].OnBlobStart),
//   - including its bytes would not exceed the current byte
//     budget (blobState.budget.Budget()) — unless the batch is
//     empty, in which case we always include the first chunk
//     even if it alone exceeds the budget (so we make progress).
//
// All claimed chunks are atomically marked in blobState.inflight
// under bs.mu so concurrent workers won't pick overlapping
// ranges.  The corresponding entries in bs.inflight are populated
// with FRESH per-chunk channels that fillBatch (via ensureChunk's
// machinery, indirectly) will close on completion — keeping the
// per-chunk inflight semantics intact for the foreground EnsureRange
// coalescing path.
//
// Returns (nil, false) when nothing is claimable — either the
// bitmap is fully set OR every still-missing chunk is in flight
// by another worker.
func (h *handle) pickWarmBatch() ([]int, bool) {
	bs := h.bs
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if bs.bitmap == nil || bs.numChunks == 0 {
		return nil, false
	}

	// Choose starting chunk.
	start := -1
	lastFgNs := bs.lastForeground.Load()
	if lastFgNs > 0 && time.Since(time.Unix(0, lastFgNs)) < warmPreemptWindow {
		anchor := bs.lastForegroundChunk.Load()
		if anchor >= 0 {
			bestIdx := -1
			var bestDist int64 = -1
			for i := 0; i < bs.numChunks; i++ {
				if bs.bitmap.isSet(i) {
					continue
				}
				if _, inflight := bs.inflight[i]; inflight {
					continue
				}
				d := int64(i) - anchor
				if d < 0 {
					d = -d
				}
				if bestIdx == -1 || d < bestDist {
					bestIdx = i
					bestDist = d
				}
			}
			if bestIdx >= 0 {
				start = bestIdx
			}
		}
	}
	if start < 0 {
		// Sequential fallback.
		for i := 0; i < bs.numChunks; i++ {
			if bs.bitmap.isSet(i) {
				continue
			}
			if _, inflight := bs.inflight[i]; inflight {
				continue
			}
			start = i
			break
		}
	}
	if start < 0 {
		return nil, false
	}

	// Grow the batch forward up to the budget.
	budget := bs.budget.Budget()
	batch := []int{start}
	usedBytes := bs.chunkRefs[start].OnBlobEnd - bs.chunkRefs[start].OnBlobStart
	for next := start + 1; next < bs.numChunks; next++ {
		if bs.bitmap.isSet(next) {
			break
		}
		if _, inflight := bs.inflight[next]; inflight {
			break
		}
		prevRef := bs.chunkRefs[next-1]
		curRef := bs.chunkRefs[next]
		if prevRef.OnBlobEnd != curRef.OnBlobStart {
			break // physical gap — would force a second HTTP request anyway
		}
		nextSize := curRef.OnBlobEnd - curRef.OnBlobStart
		if usedBytes+nextSize > budget {
			break
		}
		batch = append(batch, next)
		usedBytes += nextSize
	}

	// Reservation (installing inflight channels) is deferred to
	// fillBatch, which acquires bs.mu again to filter out chunks
	// that became present or in-flight between this pick and the
	// fill.  This is safe because warmBackgroundConcurrency == 1
	// today; if it grows, two workers could double-pick the same
	// run and fillBatch's bitmap-and-inflight recheck handles it
	// (one becomes a waiter on the other's per-chunk gate).
	return batch, true
}




// Prefetch fires background fills for chunks whose uncompressed byte range
// intersects [off, off+length).
//
// The passed-in ctx is used so the caller's namespace propagates into
// store.FillChunk (it requires NamespaceRequired).  Earlier this method
// discarded ctx and substituted context.Background(), which made every
// goroutine error out with "namespace required" — and worse, that error
// would be cached on blobState.inflight and propagated to any
// FOREGROUND fanotify event that coalesced on the same chunk, surfacing
// as a FAN_DENY (EIO) to the kernel.  We now preserve the caller's
// context; nil/no-namespace fallback uses the attach context.
func (h *handle) Prefetch(ctx context.Context, off, length int64) error {
	if ctx == nil {
		ctx = h.attachCtx
	}
	if _, hasNs := namespaces.Namespace(ctx); !hasNs {
		// Caller didn't provide a namespace.  Recover from the attach
		// context so background goroutines don't error out at the
		// indexed-content store boundary.
		if h.attachCtx != nil {
			if ns, ok := namespaces.Namespace(h.attachCtx); ok {
				ctx = namespaces.WithNamespace(ctx, ns)
			}
		}
	}
	// Detach cancellation/timeout: the request that triggered Prefetch
	// (e.g. a fanotify event handler) may complete and tear down its
	// ctx long before the background fill finishes.  Background fills
	// must run to completion regardless.
	ctx = detachContext(ctx)
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
		// The cache is keyed purely by digest.
		h.cache.evict(bs.desc.Digest.String())
	}
	return nil
}

// ChunksInRange returns metadata for every chunk whose uncompressed byte
// range intersects [off, off+length).  The slice is in ascending chunk-
// index order.  Each entry includes the cache-file offset/length, the
// on-blob (registry-side) byte range, the chunk content digest, and a
// snapshot of whether the chunk is already resident in the sparse data
// file at call time.
//
// Used by the daemon fanotify supervisor to emit one structured trace
// line per fanotify event that names the exact chunk digests the kernel
// asked for — the raw material for an image's empirical load-order
// profile, parsable into a deterministic warm-up sequence.
func (h *handle) ChunksInRange(off, length int64) []ChunkInfo {
	bs := h.bs
	if length <= 0 {
		return nil
	}
	end := off + length
	bs.mu.Lock()
	defer bs.mu.Unlock()
	var out []ChunkInfo
	for i, c := range bs.chunkRefs {
		cEnd := c.Offset + c.Length
		if c.Offset >= end {
			break
		}
		if cEnd <= off {
			continue
		}
		out = append(out, ChunkInfo{
			Index:       i,
			Digest:      c.Digest,
			Offset:      c.Offset,
			Length:      c.Length,
			OnBlobStart: c.OnBlobStart,
			OnBlobEnd:   c.OnBlobEnd,
			Present:     bs.bitmap != nil && bs.bitmap.isSet(i),
		})
	}
	return out
}

// ResidentRanges returns the uncompressed byte ranges that are currently
// resident in the cache backing file.  It reads the in-memory bitmap and
// maps consecutive present chunks to contiguous ByteRange intervals.
//
// Called by the blockcache service at stream open time so the shim can seed
// its page-presence bitmap and skip redundant Fill RPCs for already-present
// data.
func (h *handle) ResidentRanges() []ByteRange {
	bs := h.bs
	bs.mu.Lock()
	defer bs.mu.Unlock()

	if bs.bitmap == nil || len(bs.chunkRefs) == 0 {
		return nil
	}

	var ranges []ByteRange
	var cur *ByteRange

	for i, c := range bs.chunkRefs {
		if bs.bitmap.isSet(i) {
			if cur == nil {
				ranges = append(ranges, ByteRange{Start: c.Offset, End: c.Offset + c.Length})
				cur = &ranges[len(ranges)-1]
			} else if cur.End == c.Offset {
				// Extend the current run.
				cur.End = c.Offset + c.Length
			} else {
				// Gap between present chunks.
				ranges = append(ranges, ByteRange{Start: c.Offset, End: c.Offset + c.Length})
				cur = &ranges[len(ranges)-1]
			}
		} else {
			cur = nil
		}
	}

	return ranges
}

// ensureChunk coalesces concurrent fills for the same chunk index.
func (h *handle) ensureChunk(ctx context.Context, idx int, priority contentindex.Priority) error {
	bs := h.bs
	// Track the most recent foreground activity so WarmAll can
	// pick the next chunk to fill adaptively (closest to recent
	// access).  Stamp on the early-present fast path too — if a
	// blob is fully warmed-on-restart, EnsureRange still counts
	// as access for picking which chunks deserve attention next.
	if priority == contentindex.PriorityForeground {
		bs.lastForeground.Store(time.Now().UnixNano())
		bs.lastForegroundChunk.Store(int64(idx))
	}
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
// the uncompressed bytes into the sparse file.  Used by EnsureAll's
// per-chunk path (eager fallback when fanotify isn't available).
//
// The lazy-loading hot paths (EnsureRange foreground, WarmAll background)
// flow through fillBatch instead — see the doc on h.fillBatch for the
// batched-fetch strategy.
func (h *handle) fillChunk(ctx context.Context, idx int, priority contentindex.Priority) error {
	bs := h.bs
	// Observe budget on this single-chunk fetch too.  We approximate
	// "bytes" as the chunk's on-blob size; "duration" wraps just the
	// store.FillChunk call (= the network fetch).
	fetchStart := time.Now()
	if err := bs.store.FillChunk(ctx, bs.desc.Digest, idx, bs.provider, priority); err != nil {
		return fmt.Errorf("fill chunk %d: %w", idx, err)
	}
	fetchDur := time.Since(fetchStart)
	if idx < len(bs.chunkRefs) {
		c := bs.chunkRefs[idx]
		bs.budget.Observe(c.OnBlobEnd-c.OnBlobStart, fetchDur)
	}
	return h.persistFilledChunk(ctx, idx)
}

// fillBatch is the multi-chunk equivalent of fillChunk: a single
// provider Fetch covers every chunk in `indices`, the response is
// split + verified + ingested into the content store via
// store.FillBatch, and each chunk's uncompressed bytes are written
// to the sparse cache file.
//
// `indices` MAY contain chunks that have already become present or
// have been claimed by a peer fill between the picker and this
// call.  fillBatch filters those out:
//   - already-present chunks are dropped silently.
//   - peer-inflight chunks are awaited at the end; we do NOT re-fetch.
//
// After filtering, the remaining indices are split into maximal
// contiguous (on-blob) sub-runs.  Each sub-run becomes one
// store.FillBatch call (= one provider Fetch).  The cache's
// per-blob byte-budget tracker observes (bytes, duration) for
// every successful sub-run so subsequent calls adapt to the
// network.
//
// Foreground priority forwarded verbatim to the store layer.
func (h *handle) fillBatch(ctx context.Context, indices []int, priority contentindex.Priority) error {
	if len(indices) == 0 {
		return nil
	}
	bs := h.bs

	// Stamp foreground anchor on the first call into a foreground
	// batch (matches ensureChunk's behaviour for single-chunk fills).
	if priority == contentindex.PriorityForeground {
		bs.lastForeground.Store(time.Now().UnixNano())
		bs.lastForegroundChunk.Store(int64(indices[0]))
	}

	// Phase 1: filter indices into "to-fetch" + "wait".  Install
	// inflight channels for the to-fetch set so peer EnsureRange /
	// peer fillBatch calls coalesce on us.
	type waiter struct {
		idx int
		ch  chan error
	}
	type owned struct {
		idx int
		ch  chan error
	}
	var toFetch []owned
	var waiters []waiter

	bs.mu.Lock()
	for _, idx := range indices {
		if bs.bitmap.isSet(idx) {
			continue
		}
		if ch, ok := bs.inflight[idx]; ok {
			waiters = append(waiters, waiter{idx: idx, ch: ch})
			continue
		}
		ch := make(chan error, 1)
		bs.inflight[idx] = ch
		toFetch = append(toFetch, owned{idx: idx, ch: ch})
	}
	bs.mu.Unlock()

	// Helper to release an owned gate with a given error.
	releaseOwned := func(o owned, err error) {
		bs.mu.Lock()
		delete(bs.inflight, o.idx)
		bs.mu.Unlock()
		o.ch <- err
		close(o.ch)
	}

	if len(toFetch) > 0 {
		// Phase 2: split into contiguous sub-runs.  Two consecutive
		// owned entries form a contiguous run iff their chunkRefs
		// are physically adjacent on the blob.  We honour the byte
		// budget here too, in case the caller passed a range larger
		// than what one fetch should carry (e.g. EnsureRange of a
		// big mmap'd directory inode).
		runStart := 0
		for runStart < len(toFetch) {
			runEnd := runStart + 1
			usedBytes := bs.chunkRefs[toFetch[runStart].idx].OnBlobEnd -
				bs.chunkRefs[toFetch[runStart].idx].OnBlobStart
			budget := bs.budget.Budget()
			for runEnd < len(toFetch) {
				prevRef := bs.chunkRefs[toFetch[runEnd-1].idx]
				curRef := bs.chunkRefs[toFetch[runEnd].idx]
				if prevRef.OnBlobEnd != curRef.OnBlobStart {
					break
				}
				nextSize := curRef.OnBlobEnd - curRef.OnBlobStart
				if usedBytes+nextSize > budget {
					break
				}
				usedBytes += nextSize
				runEnd++
			}
			run := toFetch[runStart:runEnd]
			runIdxs := make([]int, len(run))
			for i, o := range run {
				runIdxs[i] = o.idx
			}

			fetchStart := time.Now()
			err := bs.store.FillBatch(ctx, bs.desc.Digest, runIdxs, bs.provider, priority)
			fetchDur := time.Since(fetchStart)

			if err != nil {
				// Release this sub-run's gates with the error;
				// release remaining unfetched sub-runs too so
				// nobody blocks forever.
				for _, o := range run {
					releaseOwned(o, err)
				}
				for j := runEnd; j < len(toFetch); j++ {
					releaseOwned(toFetch[j], err)
				}
				return fmt.Errorf("cache: fillBatch chunks %d..%d: %w",
					run[0].idx, run[len(run)-1].idx, err)
			}

			// Observe throughput for the budget tracker.  We count
			// only the bytes of THIS sub-run, not all of toFetch.
			bs.budget.Observe(usedBytes, fetchDur)

			// Phase 3: for each chunk in the sub-run, read back
			// from the content store, decompress if zstd, write
			// to the sparse cache file, mark bitmap, persist
			// bitmap word.  This per-chunk loop mirrors the
			// fillChunk tail logic.  If any per-chunk step
			// fails, release that chunk's gate with the error
			// and propagate.
			for _, o := range run {
				if perr := h.persistFilledChunk(ctx, o.idx); perr != nil {
					releaseOwned(o, perr)
					// Release remaining owned in this run + future runs.
					for _, x := range run[len(run):] { // empty; just kept for clarity
						_ = x
					}
					for j := runEnd; j < len(toFetch); j++ {
						releaseOwned(toFetch[j], perr)
					}
					return perr
				}
				releaseOwned(o, nil)
			}
			runStart = runEnd
		}
	}

	// Phase 4: await peer-owned gates.
	for _, w := range waiters {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-w.ch:
			if err != nil {
				return fmt.Errorf("cache: fillBatch: peer fill of chunk %d failed: %w", w.idx, err)
			}
		}
	}
	return nil
}

// persistFilledChunk reads the just-ingested chunk back from the
// content store, decompresses it if the blob is +zstd, writes the
// uncompressed bytes into the sparse cache file at the chunk's
// uncompressed offset, and marks the in-memory + on-disk bitmap.
// Shared between fillChunk (legacy single-chunk path) and fillBatch.
func (h *handle) persistFilledChunk(ctx context.Context, idx int) error {
	bs := h.bs
	if idx >= len(bs.chunkRefs) {
		// Chunk not in our missing-refs list (already present before Attach).
		bs.mu.Lock()
		bs.bitmap.set(idx)
		bs.mu.Unlock()
		return nil
	}
	c := bs.chunkRefs[idx]
	chunkLen := c.OnBlobEnd - c.OnBlobStart
	ra, err := bs.cs.ReaderAt(ctx, ocispec.Descriptor{
		Digest: c.Digest,
		Size:   chunkLen,
	})
	if err != nil {
		return fmt.Errorf("open content store entry for chunk %d: %w", idx, err)
	}
	defer ra.Close()

	raw := make([]byte, chunkLen)
	if _, err := ra.ReadAt(raw, 0); err != nil && err != io.EOF {
		return fmt.Errorf("read chunk %d from content store: %w", idx, err)
	}
	data := raw
	if contentindex.IsZstdMediaType(bs.desc.MediaType) {
		dec, derr := zstd.NewReader(bytes.NewReader(raw))
		if derr != nil {
			return fmt.Errorf("zstd decoder for chunk %d: %w", idx, derr)
		}
		decoded, derr := io.ReadAll(dec)
		dec.Close()
		if derr != nil {
			return fmt.Errorf("zstd decode chunk %d: %w", idx, derr)
		}
		data = decoded
	}
	if _, err := bs.dataFile.WriteAt(data, c.Offset); err != nil {
		return fmt.Errorf("write chunk %d to sparse file: %w", idx, err)
	}
	if err := bs.dataFile.Sync(); err != nil {
		return fmt.Errorf("sync sparse file after chunk %d: %w", idx, err)
	}
	bs.mu.Lock()
	bs.bitmap.set(idx)
	wordOff, wordVal, wordOk := bs.bitmap.snapshotWord(idx)
	bs.mu.Unlock()
	if wordOk {
		if err := bs.bitmap.writeWord(bs.bitmapPath(), wordOff, wordVal); err != nil {
			return fmt.Errorf("persist bitmap after chunk %d: %w", idx, err)
		}
	}
	return nil
}

// Compile-time assertion.
var _ Handle = (*handle)(nil)
