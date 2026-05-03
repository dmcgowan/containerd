# Sparse-File Cache

This document specifies the **sparse-file cache** component, one of three
components that compose the chunked lazy-loading pipeline for EROFS image
layers.

The other two components are described in:

- [indexed-content.md](indexed-content.md) — the indexed content store that
  tracks chunk metadata, drives verification, and exposes `FillChunk`.
- [block-provider.md](block-provider.md) — the block provider that fetches
  chunk bytes from remote sources.

---

## Role

The sparse-file cache exposes the **uncompressed image bytes** of one
indexed-content blob as a single contiguous file on the host filesystem.  The
file is:

- **Sparse**: regions that have not yet been filled are filesystem holes;
  filled regions occupy real disk blocks.
- **Uncompressed**: the cache holds the decompressed image data regardless of
  whether the underlying blob is `application/vnd.erofs` (raw) or
  `application/vnd.erofs+zstd`.  For raw blobs the cache file is byte-for-byte
  identical to the original blob's image data section.
- **Transient**: the cache file exists only for the lifetime of active mounts
  using it.  When no mount, container, lease, or image holds a reference to the
  blob, containerd's GC deletes the file.  The cache can always be recreated
  from the chunks in the content store plus a provider for missing chunks.

The cache file is used in two ways:

1. **Cachefiles ondemand** (primary, Linux 5.19+): the mount manager binds the
   cache file as the cachefiles backing file for a kernel fscache EROFS mount.
   The kernel reads through the EROFS mount; the cachefiles daemon fills holes
   by calling into the cache's fill path.
2. **Loop mount** (fallback): the sparse file is loop-mounted; all chunks must
   be fully present before the mount is issued (loop does not handle holes at
   read time).  The cache performs an eager fill of any missing chunks as part
   of the activation sequence.

---

## Storage layout

### File locations

```
<state-root>/index-cache/<blob-digest-hex>/
    data          -- sparse file, size = UncompressedSize from chunk-index header
    present.bm    -- chunk presence bitmap (see below)
```

`<state-root>` is the containerd state root (typically
`/var/lib/containerd/io.containerd.content.index.v1/cache`).
The directory is created on first `Attach`; it is owned by the indexed content
store plugin.

### Sparse file (`data`)

- Created with `fallocate(2)` or `truncate(2)` to exactly `UncompressedSize`
  bytes.  No blocks are allocated by the creation call.
- Filled chunk by chunk via `pwrite(2)` at the chunk's uncompressed byte
  offset.  After writing, `fdatasync` is called on the file descriptor to
  ensure the filled region survives a crash before the bitmap is updated.
- Read directly by the delivery adapter (cachefiles backing-file path, or a
  loop-mounted block device).

The sparse file does **not** include the dm-verity merkle tree.  The merkle
tree is appended to the EROFS image in the blob itself; it does not appear in
the decompressed image data that the cache holds.  The dm-verity device is set
up by the mount manager over the loop device or the cachefiles-backed EROFS
mount, using the `org.erofs.dmverity.*` annotations on the descriptor.

### Chunk presence bitmap (`present.bm`)

The bitmap records, for each chunk in the chunk-index, whether that chunk's
uncompressed bytes have been written to the sparse file.

**Format:**

```
+--------+--------+--     --+--------+
| uint64 | uint64 |  ....   | uint64 |
+--------+--------+--     --+--------+
  word 0   word 1             word ⌈N/64⌉-1
```

- Little-endian `uint64` words, packed.
- Bit `i` (0-indexed from LSB of word 0) corresponds to chunk `i`.
- `1` = chunk is present in the sparse file; `0` = hole.
- Total file size: `⌈NumChunks / 64⌉ × 8` bytes.

**Persistence:**

After each chunk fill (and after `fdatasync` on the sparse file), the cache
atomically updates the bitmap:

1. Set bit `i` in the in-memory bitmap.
2. `pwrite` the updated `uint64` word to `present.bm` at byte offset
   `(i / 64) × 8`.
3. `fdatasync` the bitmap file.

Step 2–3 is not atomic with the sparse-file `fdatasync`, but the ordering
guarantees that a crash after step 2–3 always leaves both files in a consistent
state: if the bitmap says chunk `i` is present, the sparse file's bytes at
`chunk[i].Offset` are valid.  If the bitmap says chunk `i` is absent, the
sparse file at that range may be stale (from a previous attempt) but will be
overwritten on the next fill.

**Recovery:**

On `Attach` to an existing cache file (e.g. after a daemon restart), the cache:

1. Reads the bitmap file into memory.
2. Validates that its size equals `⌈NumChunks / 64⌉ × 8`.  If not (corrupted
   or truncated), rebuilds from a content-store presence probe (see
   [Recovery](#recovery-after-daemon-restart) below).
3. Treats the in-memory bitmap as authoritative.  No `SEEK_HOLE`/`SEEK_DATA`
   scan is needed.

---

## In-memory state

While a cache file is active (at least one mount holds a reference), the cache
maintains per-blob state:

```go
type blobState struct {
    desc      ocispec.Descriptor   // descriptor including chunk-index annotations
    chunks    []contentindex.ChunkRef  // from the chunk-index entry
    bitmap    []uint64             // in-memory presence bitmap
    bmFile    *os.File             // open fd on present.bm for updates
    dataFile  *os.File             // open fd on data for pwrite
    inflight  map[int]chan struct{} // chunkIdx → close-when-done gate
    mu        sync.Mutex           // guards bitmap, inflight
    refs      int                  // active-mount refcount
    provider  ByteProvider         // source for missing chunks
}
```

The `inflight` table coalesces concurrent fill attempts for the same chunk:
the goroutine that first notices a chunk is missing creates a `chan struct{}`;
subsequent goroutines for the same chunk wait on the channel.  When the fill
completes (or fails), the first goroutine closes the channel so all waiters
unblock.

---

## Interface

```go
package cache

import (
    "context"
    "io"

    contentindex "github.com/containerd/containerd/v2/core/content/index"
    "github.com/containerd/containerd/v2/core/content/index/provider"
    ocispec        "github.com/opencontainers/image-spec/specs-go/v1"
)

// Cache manages sparse-file cache entries for indexed-content blobs.
//
// The implementation lives in core/content/index/cache/.
// It is instantiated once per containerd daemon and shared by the indexed
// content store plugin and the mount manager.
type Cache interface {
    // Attach opens (or creates) the cache file for the blob described by
    // desc.  If a cache file already exists (e.g. after a daemon restart or
    // from a previous activation), it is reused.  The refcount is incremented.
    //
    // The provider p is used to fetch missing chunks.  It is retained for
    // the lifetime of the returned Handle.
    //
    // Attach loads the chunk-index entry (from the indexed content store),
    // seeds the in-memory bitmap from the presence bitmap file, and starts
    // the background prefetch goroutine if PrefetchConfig.Enable is set.
    Attach(
        ctx  context.Context,
        desc ocispec.Descriptor,
        p    provider.ByteProvider,
    ) (Handle, error)
}

// Handle is a reference to the cache file for one indexed-content blob.
// It is valid until Release is called.
type Handle interface {
    io.ReaderAt

    // BackingFile returns the absolute path to the sparse file.
    // Passed to the cachefiles bind (primary delivery) or the
    // loop-mount setup (fallback delivery).
    BackingFile() string

    // Prefetch enqueues background fills for all chunks that intersect
    // the uncompressed byte range [off, off+length).  Returns immediately;
    // fills run asynchronously in the background prefetch goroutine.
    //
    // If off+length > UncompressedSize, it is silently clamped.
    Prefetch(ctx context.Context, off, length int64) error

    // Release decrements the refcount for this handle.  When the refcount
    // reaches zero the in-memory blobState is eligible for eviction.
    // The on-disk cache file and bitmap are NOT deleted by Release; deletion
    // is driven by the GC (see §Lifecycle and GC).
    Release() error
}
```

---

## Fill path

`ensureChunk(idx, priority)` is the internal function called both by
`Handle.ReadAt` (for foreground fills) and by the background prefetch goroutine
(for background fills):

```
ensureChunk(idx int, priority Priority):
    lock mu
    if bitmap[idx] == 1:
        unlock mu; return nil
    if ch, ok := inflight[idx]; ok:
        // another goroutine is already fetching this chunk
        unlock mu
        wait on ch
        return (error from that fill, if any)
    ch = make(chan struct{})
    inflight[idx] = ch
    unlock mu

    defer:
        lock mu
        delete(inflight, idx)
        close(ch)  // unblocks all waiters
        unlock mu

    // Ask the indexed content store to fetch the chunk bytes into
    // the content store (verifies per-chunk hash).
    err = store.FillChunk(ctx, desc.Digest, idx, provider, priority)
    if err: return err

    // Open the chunk's content-store entry and read its bytes.
    ra, err = contentStore.ReaderAt(ctx, ocispec.Descriptor{Digest: chunks[idx].Digest})
    if err: return err
    defer ra.Close()

    // Decompress if +zstd; for raw layers use as-is.
    uncompressed, err = decompress(ra, desc.MediaType)
    if err: return err

    // Write to the sparse file at the chunk's uncompressed offset.
    _, err = pwrite(dataFile, uncompressed, chunks[idx].Offset)
    if err: return err

    // Sync the data before updating the bitmap.
    err = dataFile.Sync()
    if err: return err

    // Update in-memory bitmap.
    lock mu
    setBit(bitmap, idx)
    unlock mu

    // Persist bitmap update atomically.
    err = persistBitmapWord(bmFile, idx)
    return err
```

### ReadAt semantics

`Handle.ReadAt(p []byte, off int64)` maps the byte range to the set of
chunk indices that cover `[off, off+len(p))`.  For each missing chunk it
calls `ensureChunk(idx, PriorityForeground)`.  Once all required chunks
are present it satisfies the read directly from the sparse file using
`pread(2)` (via `os.File.ReadAt`).

Callers do not need to call `Prefetch` before `ReadAt`; `ReadAt` issues
foreground fills automatically.  `Prefetch` is an optimisation: it schedules
background fills for regions the caller knows will be needed soon.

---

## Prefetching

When `Attach` is called with prefetch enabled (controlled by `PrefetchConfig`),
a background goroutine walks the chunk list in chunk-index order and calls
`ensureChunk(idx, PriorityBackground)` for each absent chunk.

The prefetch goroutine:

- Respects context cancellation (stops immediately when ctx is done).
- Yields to foreground fills: `ensureChunk` uses the provider's two-level
  priority queue so a foreground request always runs ahead of the prefetch.
- Stops automatically when all chunks are present.

Callers can request prefetch of a specific range ahead of time via
`Handle.Prefetch(ctx, off, length)`, which enqueues background fills for
the chunks intersecting that range at the front of the background queue.

---

## Kernel-side delivery adapters

The cache exposes its sparse file to the kernel via two adapters.  The mount
manager (see [lazy-load-mount-manager.md](lazy-load-mount-manager.md)) picks
the adapter based on host capability at activation time.

### Cachefiles ondemand adapter (primary)

Used when `/dev/cachefiles` is available and the kernel supports EROFS fscache
(Linux 5.19+).

The mount manager:

1. Opens `/dev/cachefiles`.
2. Writes `dir <backing-dir>`, `tag <blob-digest>`, `bind ondemand` to the fd.
3. Issues an EROFS fscache mount with `fsid=<blob-digest>,tag=<binding>`.

The cachefiles daemon (running inside containerd or in the shim per
[lazy-load-shim-runtime.md](lazy-load-shim-runtime.md)) services `OPEN`,
`READ`, and `CLOSE` events on the cachefiles fd:

- **OPEN**: record the cookie; call `Attach` to get a Handle.  Use
  `Handle.BackingFile()` as the cachefiles backing file path so the kernel can
  map the cookie to the sparse file directly.
- **READ**: call `ensureChunk` for the chunks covering the requested range
  (foreground priority), then acknowledge the kernel event.  The kernel reads
  from the backing file.
- **CLOSE**: call `Handle.Release()`.

For the cachefiles path the kernel reads data from the backing file (the sparse
file) after the `READ` acknowledgement.  The cache never returns data to the
daemon; it only ensures the sparse file is populated at the requested range.

### Loop adapter (fallback)

Used when cachefiles is unavailable (older kernels, container environments
without `/dev/cachefiles`).

Loop delivery requires a fully populated sparse file; the kernel cannot handle
holes in a loop-mounted EROFS image.  The activation sequence:

1. Call `Attach` to get a Handle.
2. Call `Handle.Prefetch(ctx, 0, UncompressedSize)` to enqueue background fills
   for all chunks, then wait for completion (block until `MissingChunks` returns
   empty or the context times out).
3. Set up a loop device over `Handle.BackingFile()`.
4. Mount EROFS over the loop device with any required dm-verity wrapping.

Because loop delivery is eager, it is suitable for:

- Fully-hydrated blobs (all chunks already in the content store — fill completes
  from the content store without any provider fetch).
- Fallback on hosts without cachefiles, accepting the "fill-then-mount" latency.

---

## GC integration

Cache files are referenced by mount records via an annotation in the
`containerd.io/gc.ref.content.index.cache` namespace:

```
containerd.io/gc.ref.content.index.cache/<key> = <blob-digest>
```

This annotation lives on the snapshot record or the active-mount record created
by the snapshotter at `Apply` time (alongside the existing
`containerd.io/gc.ref.content.index/<key>` annotation for the chunk content-store
entries).

A GC label-namespace extension registered by the cache plugin expands
`containerd.io/gc.ref.content.index.cache/<key>=<blob-digest>` into:

1. The cache directory path (`<state-root>/index-cache/<blob-digest>/`), flagged
   for deletion when no annotation refers to the blob.
2. No content-store digests are returned (the cache file is derived, not a
   content-store entry itself).

The lifecycle is then:

- **Created**: on first `Attach` call.
- **Active**: while a mount, container, lease, or image record carries the
  annotation.
- **Deleted**: when the GC determines no annotation refers to the blob.  The
  GC extension deletes the cache directory atomically (rename-over-tombstone
  then unlink) and removes the annotation.

Because the cache file is derived from the content-store chunks, deleting it
loses no data: a subsequent `Attach` recreates the sparse file and fills it
from the content store (zero provider fetches if all chunks are locally present).

---

## Recovery after daemon restart

On restart the cache plugin enumerates `<state-root>/index-cache/` and, for
each `<blob-digest>/` subdirectory:

1. Checks whether the blob's metadata record exists in the indexed content
   store.  If not, the cache directory is orphaned and is scheduled for removal.
2. Reads `present.bm` and validates its size against `NumChunks`.
   - **Valid**: loads the bitmap into memory; the cache is ready to service
     fills without a content-store presence probe.
   - **Invalid or missing**: probes the content store for each per-chunk digest
     (via `MissingChunks` on the indexed content store) and rebuilds the bitmap
     from the probe result.  The rebuilt bitmap is written to `present.bm`.
3. Does **not** recreate the sparse file if it is missing.  The sparse file is
   created on the next `Attach` call, which is issued by the mount manager when
   the first mount activation for the blob occurs after restart.

Active mounts that were alive before the restart are resumed by the snapshotter
and the mount manager via the existing containerd task-recovery path; their
`Attach` calls re-enter the per-blob in-memory state naturally.

---

## Failure modes

| Failure | Detection | Recovery |
|---|---|---|
| **Disk full during fill** | `pwrite` returns `ENOSPC` | `ensureChunk` returns error; delivery layer surfaces EIO; no partial state in bitmap (write failed before `fdatasync + bitmap update`) |
| **Provider fetch error** | `store.FillChunk` returns error | `ensureChunk` returns error; channel closed so waiters unblock with same error; chunk remains absent; retry on next access |
| **Sparse file deleted externally** | `pwrite` returns `EBADF` or `EIO` | Cache detects stale fd; re-creates the file and replays fills from the content store |
| **Bitmap file corrupted** | Size validation on `Attach` fails | Rebuild from `MissingChunks` content-store probe; overwrite `present.bm` |
| **Daemon restart mid-fill** | Chunk absent in bitmap (fill was interrupted) | Next `ensureChunk` for that chunk re-fetches from content store; no double-write |
| **Content-store entry missing** | `ReaderAt` returns not-found | Re-trigger via provider: `ensureChunk` calls `store.FillChunk` which calls `provider.Fetch`; chunk is re-downloaded and re-verified |

---

## Package placement

Implementation lives in `core/content/index/cache/`.  The package exports
only the `Cache` and `Handle` interfaces and a constructor:

```go
// New returns a Cache rooted at stateRoot.
// store is used for MissingChunks and FillChunk calls.
// cs is used to read chunk content-store entries after a fill.
func New(stateRoot string, store contentindex.Store, cs content.Store) Cache
```

The `Cache` value is instantiated once by the indexed-content-store plugin at
startup and passed to the mount manager and the cachefiles daemon.

---

## Future work

- **fanotify pre-content delivery** — Linux 6.13+ `FAN_PRE_ACCESS` events allow
  the kernel to notify userspace before a read is serviced, without going
  through the cachefiles protocol.  The cache's fill path can be driven by
  fanotify events directly, enabling EROFS lazy loading without the cachefiles
  ondemand protocol.
- **Block-manager interface** — a generic interface for delivery mechanisms
  that need to "request fill of block range X" rather than file-level
  `ReadAt`.  Allows the cache to be used by NBD-backed or io_uring-backed
  delivery in the future.
- **Compression in the sparse file** — for workloads where disk space matters
  more than read latency, a future cache variant could hold compressed chunks
  and decompress on `ReadAt`.  Requires a different file format and cannot be
  used directly by cachefiles.
- **Cross-blob dedup at cache level** — chunks shared between two blobs occupy
  one content-store entry already; a future cache variant could use reflinks
  (`FICLONE`) to avoid re-writing identical data into two sparse files.
- **LRU retention after Release** — keep released cache files on disk for a
  configurable duration or size budget to accelerate container restarts,
  evicted under disk pressure by a background sweep.
