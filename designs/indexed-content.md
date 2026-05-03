# Indexed Content: Chunk-indexed, Mountable Blob Storage in containerd

This document covers the **Indexed Content** milestone: a new top-level
containerd service that manages OCI blobs carrying an embedded chunk index.
The service enables kernel-side mounts without unpacking, random-access reads
at chunk granularity, chunk-level content deduplication, and optional
kernel-enforced dm-verity integrity. It is a **peer** of the existing content
store and snapshot store, not a replacement for either.

The milestone is **self-contained**: it can be reviewed, implemented, and
merged into containerd without any dependency on the lazy-loading mount
manager, shim runtime, or snapshotter Part 2 work. Those later milestones
consume this one; this one does not consume them.

The Indexed Content Store operates on **blobs with an embedded index**, not on
fixed-size disk blocks. The "chunk" is a variable-size unit defined by the
chunk-index media type annotated on the blob. For EROFS-bearing layers the
format is [`application/vnd.erofs.chunk-index.v1`][spec-22]; this is the
only chunk-index format defined by this milestone. The name reflects that the
index addresses zstd-frame chunk boundaries; the index itself is not compressed.
Future formats (content-defined chunking, path-keyed indexes) can be introduced
under additional media types without service changes.

## Summary

The milestone is composed of nine independent areas, each shippable as its own
unit:

- **Storage** — per-blob format metadata and an ordered chunk-index record
  (one row per chunk: logical offset, on-blob frame range, per-chunk hash,
  decompression hints) stored in buckets inside containerd's shared metadata
  BoltDB. Chunks are stored as content-store entries keyed by per-chunk hash.
  Concurrent writes share transactions with the wider metadata store via the
  `boltutil` context helpers, so a pull commits chunk-index records, content
  writes, and lease annotations in a single fsync. The producer's chunk hashes
  are verified at ingest.

- **Providers** — a pluggable `io.ReaderAt` abstraction over byte sources. v1
  ships three: Local Content (the existing content store), Registry (HTTP range
  requests), and External (plugin interface for out-of-tree sources). Providers
  do not cache; they are only invoked for bytes not already present in the
  content store.

- **Caching** — a layer between the provider and the kernel that holds fetched
  bytes durably so repeated reads and warm mounts do not require remote fetches.
  The cache is a mechanism-agnostic abstraction; v1 identifies three delivery
  approaches (cachefiles ondemand, loop/sparse file, and NBD/diskless) without
  prescribing which must be used. The specific choice of kernel-side delivery is
  handled by the mount layer above this milestone.

- **Garbage collection** — integration with containerd's extensible GC via a
  label-namespace extension. Core objects reference indexed-content blobs only
  through `containerd.io/indexed-content.ref/*` annotations; the extension
  expands each annotation into the set of content-store digests that GC must
  preserve. Chunk deduplication is automatic: a chunk shared across blobs is
  collected only when no annotation anywhere names a blob whose record lists it.

- **Producer path** — the `Write`/`Commit` ingest pipeline: receive blob bytes,
  locate and verify the chunk index, split into chunks, write each chunk to the
  content store under its per-chunk hash, write the chunk-index record. Integrates
  with the EROFS differ and `ctr image convert --erofs`.

- **Distribution** — eager hydration (all bytes local before container start)
  and lazy hydration (chunk-index records and dm-verity trees pulled first; data
  chunks fetched on demand). Both paths use the shared metadata transaction to
  amortize per-chunk commit costs across a pull.

- **Snapshotter coordination** — the decoupled protocol between the indexed
  content store and snapshotters. Apply records one annotation label; `Mounts()`
  calls `IndexedContent.Mounts(digest)` and splices the result. The snapshotter
  learns no knowledge of loop devices, dm-verity setup, or chunk tables.

- **Credentials** — a lifecycle solution for registry tokens that must survive
  daemon restarts and remain refreshable for the lifetime of any partially-
  hydrated blob. Credentials are held in an in-process cache backed by an
  encrypted at-rest store keyed to a process-bound or machine-bound secret.

- **API surface** — a blob-level Go interface and a mirroring gRPC/ttrpc
  service. Producers `Write` a blob; consumers call `Mounts()` or a
  range-bound read. Chunk-level addressing is internal.

## Goals

- **Direct kernel-side mount** — Mount blobs as kernel filesystems without
  unpacking, with optional dm-verity wrapping.
- **Sequential-digest preservation** — A blob's descriptor digest covers all
  bytes from offset 0 to end, including any appended chunk index and dm-verity
  merkle tree. Producers push a blob byte-for-byte to a registry; the Indexed
  Content Store stores it byte-for-byte; consumers fetch it byte-for-byte. A
  `+zstd` layer's DiffID is the digest of the decompressed image data stream,
  carried on the layer descriptor via the `org.erofs.uncompressed-digest`
  annotation; the chunk-index digest is surfaced via the
  `org.erofs.chunk-index.digest` descriptor annotation. `rootfs.diff_ids`
  serves as a legacy fallback when the annotations are absent.
- **Chunk-indexed lazy loading** — Fetch byte ranges lazily from registries or
  external sources via a `Provider` returning `io.ReaderAt`. The chunk index's
  per-chunk checksums double as the cache's content-address keys.
- **Pluggable chunk-index formats** — The service treats chunk indexes as typed
  objects identified by media type (see [erofs-image-spec §2.2][spec-22]). v1
  ships a parser for `application/vnd.erofs.chunk-index.v1`; alternative
  formats can be registered without changes to the service core.
- **Hierarchical integrity** — Producers emit dm-verity hash trees and root
  hashes; the Indexed Content Store and mount handler consume them at
  activation.
- **First-class snapshotter integration** — The existing `erofs` snapshotter
  can use the Indexed Content Store as its r/o backing by recording one
  annotation label at apply time and calling `Mounts()` at mount time.
- **Batched, transactional ingest** — A pull operation drives all ingests in
  parallel inside a single shared metadata transaction, committing the entire
  pull in a small number of fsyncs and avoiding a per-chunk transaction
  explosion.

## Non-Goals (v1)

- **R/W → r/o sealing.** Producing a hashed r/o blob from a r/w filesystem
  stays in the producer (differ, image converter, build pipeline).
- **Chunk-level public API.** Producers ingest a whole blob; consumers access
  via `Mounts()` or a blob-level read. Chunk-level reads/writes are internal.
- **Streaming `Read`/`Write` over gRPC.** Consumers access via `Mounts()` or
  range-bound blob reads; producers ingest with a discrete `Write`/finalize
  call.
- **Confidential containers signing.** The store verifies a hash tree against a
  supplied root hash; trusting that root hash is a higher-level concern.
- **Generic non-EROFS-shaped consumer.** v1 consumer is EROFS-shaped (the
  `erofs` snapshotter).
- **External provider SDK stability.** The plugin interface exists but is not
  declared stable in v1.

## Architecture

```
      Clients
  (transfer, snapshotters,
   runtime, ctr)
        │
        │ Go / gRPC
        ▼
┌─────────────────────────┐
│  Indexed Content Store  │
│  · Sidecar DB           │
│  · GC extension         │
│  · Batched sessions     │
│  · Chunk-map dispatch   │
└───┬──────────┬───────┬──┘
    │          │       │
    ▼          ▼       ▼
Providers  Content   Mounts()
           Store
```

Core objects (Image, Manifest, Snapshot, Container, Active Mount) live in
containerd's core BoltDB metadata and reference indexed-content blobs
**only through annotations**. The indexed content store registers itself
as a GC extension over a dedicated annotation prefix
(`containerd.io/indexed-content.ref/<key>=<digest>`); when containerd's GC
walks a core object it hands those annotations to the extension, which
expands them into the set of content-store digests that must be preserved.
This keeps the cross-store reference shape annotations-only and isolates the
chunk-index metadata in the indexed-content metadata buckets where the indexed
content store can manage it independently.

## v1 Consumer

The first in-tree consumer is the **existing `erofs` snapshotter** configured
to use the Indexed Content Store as its r/o backing. This represents one
concrete example of how a consumer integrates; the service is not coupled to
this snapshotter.

- **R/O backing**: instead of writing per-snapshot `layer.erofs` files, the
  snapshotter records a `containerd.io/indexed-content.ref/<key>=<digest>` label
  at apply time. At `Mounts()` it asks the Indexed Content Store for the
  corresponding mount specs and splices them into its overlay/upper stack.
- **R/W layer**: unchanged — filesystem-on-block (mkfs ext4 in a sparse file,
  used as overlay upperdir). Sealing back to r/o is not part of v1.
- **Selection**: honours `os.features=erofs` via the same transfer-plugin
  selection logic the snapshotter uses today; the indexed-content path is
  opt-in via snapshotter configuration.

## Use Cases

1. **Distribute and mount native EROFS images directly** — pulled blobs are
   mountable as-is, no per-host unpack, no second on-disk copy.
2. **Remote on-demand fetching** — the registry provider feeds chunks into the
   content store as they are fetched; the per-chunk checksums are both the
   cache content-addresses and the content-store digests.
3. **Cross-image chunk dedup at content-store granularity** — chunks shared
   between two indexed-content blobs are stored once in the content store and
   pinned by each blob's chunk-index record. Combined with content-defined
   chunking this gives strong cross-image dedup without any new storage
   primitive on the host.
4. **Aggregate blobs** — fsmerge-style flattened EROFS metadata images (one blob
   referencing original layer blobs as data devices) fit the same model. The
   aggregate blob has its own chunk index; referenced data blobs each have
   their own.
5. **Future chunk-index formats** — alternative chunk-index media types plug
   into the same service without changes to existing consumers.

## Relationship to the existing Content Store

The Indexed Content Store sits *alongside* the existing content store and uses
it as its byte-storage primitive:

- A **manifest** referencing an EROFS-bearing layer lives in the content store
  as before. The manifest's layer descriptor names a logical blob known to the
  Indexed Content Store.
- The **logical blob** has a chunk-index record in the indexed-content metadata
  buckets naming
  every chunk by its per-chunk hash; each chunk is a content-store entry under
  that digest.
- A **transfer pull** either eagerly extracts chunks into the content store or
  sets the logical blob up for lazy fetching. Either way, every materialised
  chunk is a content-store entry; no special storage area is required.
- **References from core objects to indexed-content blobs are annotations
  only.** Containerd's GC walks those annotations through the indexed content
  store's GC extension, which expands each into the chunk content-store digests
  that must be preserved.

---

## Areas

### Storage

The indexed content store's durable state lives in **buckets inside
containerd's metadata BoltDB**, under the top-level path
`v1/<namespace>/indexed-content/`. The indexed content store is a containerd
plugin running in the same process as the metadata store; it receives the
shared `metadata.DB` at construction time and uses the `boltutil` context
helpers to participate in transactions opened by the wider metadata stack.
Nothing outside the indexed-content buckets reads or writes those buckets.

#### Metadata schema

Each blob occupies one top-level record keyed by logical blob digest. The
record is composed of the following fields:

**Blob identity and descriptor**

| Field | Type | Description |
|---|---|---|
| `Digest` | `string` | Content digest of the logical blob (key). Matches the OCI descriptor digest. |
| `Size` | `int64` | Total byte length of the logical blob, including dm-verity merkle tree when present and (for `+zstd` layers) the trailing chunk-index skippable frame. |
| `MediaType` | `string` | OCI media type of the layer blob (e.g. `application/vnd.erofs+zstd`). |
| `CreatedAt` | `timestamp` | Time the record was first written. |
| `UpdatedAt` | `timestamp` | Time the record was last modified. |

**Chunk-index location and format**

| Field | Type | Description |
|---|---|---|
| `IndexOffset` | `int64` | Absolute byte offset of the chunk-index section in the blob. For `+zstd` layers this is the start of the zstd skippable frame; the 32-byte index header begins at `IndexOffset + 8`. |
| `IndexEnd` | `int64` | Exclusive end of the chunk-index section. `0` means end-of-blob. |
| `IndexDigest` | `string` | Digest of the chunk-index payload bytes (the 32-byte header and all chunk entries, excluding the 8-byte skippable-frame header for `+zstd`). Matches `org.erofs.chunk-index.digest`. Empty if the annotation was not present. |
| `IndexMediaType` | `string` | Chunk-index format identifier. Default `application/vnd.erofs.chunk-index.v1`. |
| `NumChunks` | `uint32` | Number of chunk entries from the index header. |
| `HashAlgo` | `uint8` | Hash family from the index header (`0` = none, `1` = SHA-2). |
| `HashSize` | `uint8` | Bytes per per-chunk checksum. `0` when `HashAlgo` is `0`. |
| `Flags` | `uint16` | `Flags` field from the index header (reserved; `0` in v1). |
| `UncompressedSize` | `int64` | Total uncompressed size of the image data from the index header `UncompressedSize` field. |

**dm-verity**

| Field | Type | Description |
|---|---|---|
| `DmVerityHashOffset` | `int64` | Uncompressed byte offset where the dm-verity merkle tree begins. `0` if no dm-verity. Matches `org.erofs.dmverity.hash_offset`. |
| `DmVerityRootDigest` | `string` | Root digest of the merkle tree. Empty if no dm-verity. Matches `org.erofs.dmverity.root_digest`. |
| `DmVerityBlockSize` | `uint32` | dm-verity block size in bytes. `0` when absent (consumer defaults to `4096`). Matches `org.erofs.dmverity.block_size`. |

**Provider and operational state**

| Field | Type | Description |
|---|---|---|
| `Provider` | `string` | Name of the bound provider (e.g. `"local-content"`, `"registry"`). Used to resume lazy fetching and to route chunk-miss requests. |
| `Labels` | `map[string]string` | Operator-supplied labels. Separate from GC labels on core objects; used for eviction hints, source-image back-references, and similar operational metadata. |

**Chunk-index entries** (ordered array, one entry per chunk)

| Field | Type | Description |
|---|---|---|
| `Index` | `uint32` | Zero-based chunk position within the blob's image data. |
| `LogicalOffset` | `int64` | Offset of the chunk's first byte within the decompressed image data stream. |
| `LogicalLength` | `int32` | Decompressed byte length of this chunk. Equal to `entry[i+1].UncompressedOffset - entry[i].UncompressedOffset` for all but the last chunk; `UncompressedSize - entry[N-1].UncompressedOffset` for the last. |
| `BlobOffset` | `int64` | On-blob byte offset of the chunk's compressed zstd frame (for `+zstd` layers) or raw bytes. |
| `BlobLength` | `int32` | On-blob byte length of the compressed frame or raw bytes. Equal to `entry[i+1].BlobOffset - entry[i].BlobOffset` for all but the last chunk. |
| `ContentDigest` | `string` | Per-chunk hash from the chunk index. This is the content-store digest under which the chunk's bytes are stored. |
| `EntryFlags` | `uint8` | Reserved; always `0` in v1. Future flag bits may accompany additional per-entry fields introduced in later versions. |

#### Scale

A 500 MiB blob at 4 MiB chunks has approximately 125 chunks. Per-blob
indexed-content metadata overhead:

| Component | Approximate size |
|---|---|
| Blob identity and descriptor | ~256 bytes |
| Chunk-index location and format fields | ~128 bytes |
| dm-verity fields | ~96 bytes |
| Provider binding and labels | ~256 bytes |
| Chunk-index entries (125 × ~96 bytes) | ~12 KiB |
| **Total per blob** | **~13 KiB** |

A populated host with 1,000 distinct indexed-content blobs across all images
uses approximately 13 MiB of indexed-content bucket storage — small relative to
the chunk content itself and comparable to existing content-store metadata.

#### Why share the metadata DB

The indexed content store is a containerd plugin; it runs in the same process
as the metadata store. That co-location makes three things possible that a
separate database cannot offer:

1. **Transaction sharing eliminates fsyncs.** Every BoltDB write transaction
   ends in an fsync. On many storage backends (network-attached volumes,
   overlayfs, spinning media with write-back disabled) that fsync dominates
   per-chunk latency. When the indexed content store participates in the same
   `bolt.Tx` as the surrounding content write and lease update, the fsync
   count drops from O(chunks) to O(sub-batches). The `boltutil.WithTransaction`
   / `boltutil.Transaction` helpers in the existing metadata package are the
   mechanism: a caller pushes a `*bolt.Tx` onto the context; the indexed
   content store's internal `view` / `update` helpers join it instead of
   opening a new transaction.

2. **Single-database crash consistency.** With one BoltDB file there is no
   window in which a chunk-index record exists without a corresponding content
   write or vice versa. A partial write rolls back in its entirety; no
   startup-time reconciliation between two independent databases is needed.

3. **Schema evolution stays bounded.** Chunk-index records live under their
   own bucket path; the rest of the metadata schema is unaffected. Future
   record extensions bump a version key inside the indexed-content bucket only.
   Recovery from blob-embedded chunk indexes (re-parsing the raw index bytes
   from the content store) remains possible regardless of bucket version.

#### Transaction sharing

Naive ingest commits one metadata transaction per chunk. A 500 MiB layer at
4 MiB chunks (~125 chunks) would drive ~125 BoltDB write transactions with
their own fsyncs, turning the pull pipeline into a transaction-rate bottleneck.

The indexed content store avoids this by following the same pattern as the
core metadata package (`core/metadata/bolt.go`):

```go
// Inside the indexed content store package.
// Mirrors core/metadata/bolt.go exactly.

func view(
    ctx context.Context,
    db  Transactor,
    fn  func(*bolt.Tx) error,
) error {
    tx, ok := boltutil.Transaction(ctx)
    if !ok {
        return db.View(fn)
    }
    return fn(tx)
}

func update(
    ctx context.Context,
    db  Transactor,
    fn  func(*bolt.Tx) error,
) error {
    tx, ok := boltutil.Transaction(ctx)
    if !ok {
        return db.Update(fn)
    }
    if !tx.Writable() {
        return fmt.Errorf(
            "read-only tx in context: %w",
            errbolt.ErrTxNotWritable,
        )
    }
    return fn(tx)
}

// Transactor mirrors metadata.Transactor.
// Satisfied by *metadata.DB directly.
type Transactor interface {
    View(func(*bolt.Tx) error) error
    Update(func(*bolt.Tx) error) error
}
```

A bulk caller (transfer/pull, `ctr image convert`) batches writes by opening
one `db.Update` around the entire operation and pushing the transaction onto
the context with `boltutil.WithTransaction`:

```go
err = db.Update(func(tx *bolt.Tx) error {
    ctx := boltutil.WithTransaction(ctx, tx)
    // All Store method calls below join
    // this transaction automatically.
    for _, layer := range layers {
        if err := store.Write(
            ctx, ref, desc,
        ); err != nil {
            return err
        }
    }
    // Lease annotations and content writes
    // all land in the same tx; one fsync.
    return nil
})
```

Callers choose their own sub-batch boundaries. The tradeoff is symmetric: a
transaction held open too long blocks BoltDB readers; a transaction committed
too often wastes fsyncs. Recommended guidance: split very large pulls into
sub-batches of ~1,024 chunks or ~256 MiB of pending bytes, committing each
as its own `Update`. The indexed content store imposes no limit itself.

If a caller does not push a transaction onto the context, every `Store` method
opens its own — providing correct single-operation semantics at the cost of
one fsync per call. Recovery on restart scans for orphaned ingest state and
either resumes or removes it, matching existing content-store semantics.

#### Verification at ingest (Hasher)

Producers supply the chunk-index per-chunk checksums; the indexed content store
does not re-derive them, but **verifies** them at ingest:

- Each chunk's on-blob bytes are hashed and compared against the entry's
  `ContentDigest` on first ingest. A mismatch is treated as a corrupt blob.
- The chunk-index payload bytes are hashed and compared against
  `org.erofs.chunk-index.digest` (when the annotation is present) before any entries
  are read.
- The descriptor digest is verified over the full blob byte stream.
- The dm-verity root digest is verified at mount activation against the runtime
  kernel target.

Subsequent reads from chunk content-store entries reuse the content store's
existing verification semantics with no additional indexed content store work.

---

### Providers

A provider returns an `io.ReaderAt` over a blob's bytes and any metadata needed
to populate the store on ingest. Providers do not cache; they are the byte
source of last resort when a chunk is not already present in the content store.

v1 defines three providers:

- **Local Content Provider** — wraps the existing content store. Reuses
  already-fetched content entries without copying. Short-circuits any chunk
  that is already present in the content store from a previous pull, so a
  second copy is never staged.
- **Registry Provider** — HTTP range requests directly to a registry blob URL.
  The v1 path for remote on-demand fetch. Holds registry credentials via the
  credential lifecycle described in the Credentials area.
- **External Provider** — plugin interface for out-of-tree sources (cloud
  volumes, P2P systems, dragonfly-style peers). Present in v1 but not declared
  stable.

The provider interface:

```go
// Provider sources blob bytes.
// Providers do not cache.
type Provider interface {
    Name() string
    Open(
        ctx  context.Context,
        dgst digest.Digest,
    ) (io.ReaderAt, int64, error)
}
```

The Local Content Provider short-circuits ingest when the source is already
present in the content store, making chunk extraction a metadata-only copy
operation on filesystems that support `copy_file_range` or reflink clones.

---

### Caching

The caching layer sits between a provider and the kernel-side filesystem mount.
Its role is to hold fetched bytes durably so that warm reads and container
restarts do not require remote fetches. The cache is an abstraction; the
specific mechanism used to back the cache and serve bytes to the kernel is
determined by host capability and runtime configuration at mount time and is
therefore handled by the mount layer (a later milestone). This area defines
the storage model and interface that the mount layer consumes.

#### Cache storage model

Fetched chunks are stored as **content-store entries** keyed by their
per-chunk hash. The indexed content store does not maintain a separate chunk
file area: writing a chunk means writing a content-store entry under a digest
equal to the chunk's per-chunk checksum, and reading a chunk means opening a
content-store reader at that digest. This reuses the content store's on-disk
layout, deduplication, hash verification, leases, and GC primitives. A chunk
shared between two indexed-content blobs occupies one content-store entry
referenced by both blobs' chunk-index records.

Three delivery-side representations are anticipated, though only the first is
required for this milestone:

- **Default: content-store entries as files.** Chunk presence is content-store
  entry presence. Reading a chunk is a content-store `ReaderAt` open. The
  fetch daemon writes a chunk to the content store the moment it is fetched and
  verified, so chunk availability and content-store presence are the same fact.
- **Cachefiles backing directory.** When delivering via fscache, a cachefiles
  backing directory is layered over the chunk content-store entries (chunks are
  bind-mounted, hard-linked, or reflink-cloned into the cachefiles tree
  depending on filesystem support). The kernel's `cachefiles` driver tracks
  presence via the page cache and the backing file's extent map.
- **Sparse file.** For blobs that need to expose a contiguous byte stream but
  were ingested chunkwise, a sparse file is materialised on demand by walking
  the chunk-index record and writing chunks at their logical offsets. Useful as
  a fallback for loop-mount or NBD delivery, or on hosts where cachefiles is
  unavailable.

Other mechanisms (NBD-backed, object-store-backed, in-memory for diskless
environments) are valid future extensions; the abstraction does not preclude
them. The decision of which mechanism to use for a given activation is
left to the mount layer.

#### Two-level granularity

- **Fetch unit** = chunk size (from the chunk index, typically a few MiB) —
  whole chunks are fetched, decompressed if `+zstd`, and ingested into the
  content store under the per-chunk hash.
- **Verify unit** = dm-verity block size (default 4 KiB; configurable via the
  `org.erofs.dmverity.block_size` annotation per [erofs-image-spec §2.3][spec-23])
  — the kernel verifies at this granularity using the appended merkle tree once
  chunks are visible to it.

#### Cache interface

```go
// Cache wraps a Provider with local
// persistence; itself an io.ReaderAt.
type Cache interface {
    ReaderAt(
        ctx  context.Context,
        dgst digest.Digest,
        p    Provider,
    ) (io.ReaderAt, error)
    Evict(
        ctx  context.Context,
        dgst digest.Digest,
    ) error
}
```

#### Eviction

Eviction is pluggable; the default is no eviction. An optional size-limited LRU
eviction policy runs at content-store granularity, leaning on the indexed
content store's GC extension to ensure chunks pinned by an active
indexed-content blob are not evicted. Cache state (cachefiles cookies, sparse
bitmaps, daemon in-flight maps) is derived from per-chunk content-store entries
and is rebuilt from the chunk-index record when a blob is reactivated; it is
torn down when the underlying chunks are collected.

---

### Lazy Mode and Missing Chunks

The eager ingest path (see [Producer Path](#producer-path)) downloads and
verifies all chunk bytes before the blob becomes available to consumers.
Lazy mode inverts this: the indexed content store records the full chunk-index
metadata — every chunk's uncompressed offset, length, on-blob range, and
per-chunk hash — without fetching any chunk bytes.  A running consumer (the
sparse-file cache; see [designs/cache.md](cache.md)) then pulls chunks on
demand through a provider as the kernel reads into them.

The block provider design is specified in [designs/block-provider.md](block-provider.md).
The sparse-file cache is specified in [designs/cache.md](cache.md).

#### Lazy-ingest entry point

Lazy ingest is triggered by calling `Writer` with a `WriterOpt` that carries the
bound `ByteProvider` and requests lazy mode.  The writer:

1. Fetches **only** the chunk-index section from the provider (the byte range
   named by `org.erofs.chunk-index.range` on the descriptor).
2. Verifies the chunk-index payload against `org.erofs.chunk-index.digest`.
3. Parses all chunk entries to extract offsets, lengths, on-blob ranges, and
   per-chunk checksums.
4. Writes the chunk-index payload to the content store under its digest
   (identical to the eager path).
5. Records the metadata entry — `IndexDigest`, ordered per-chunk digest list,
   extras, and the bound provider name — but writes **zero** chunk content-store
   entries.  The per-chunk digest list still records every digest; a digest with
   no corresponding content-store entry is what "missing chunk" means at query
   time.
6. Commits the metadata record.  From this point the blob is known to the
   indexed content store and its chunks are GC-pinned by reference, even though
   the chunk bytes have not yet been written.

Lazy ingest is atomic: if the writer is abandoned before `Commit` the metadata
record is not written and no partial state is left behind.

#### Missing-chunk view

```go
// MissingChunks returns the ChunkRefs whose bytes are not yet present in the
// content store, in chunk-index order.  A nil or empty slice means the blob
// is fully hydrated.
//
// MissingChunks is cheap: it reads the ordered per-chunk digest list from
// the metadata record and queries the content store for each digest without
// opening the chunk-index entry.
MissingChunks(ctx context.Context, dgst digest.Digest) ([]ChunkRef, error)
```

`MissingChunks` is the seeding call used by the cache when it attaches to a
blob.  The cache builds its presence bitmap from this result (all returned
chunks are absent; all others are present) rather than from an `lseek` scan,
so the bitmap is correct from the first read even if the sparse file does not
yet exist on disk.

#### Chunk fill path

```go
// FillChunk fetches one chunk through provider p and writes its bytes to the
// content store under the chunk's per-chunk hash.
//
// priority is forwarded verbatim to p.Fetch; use PriorityForeground for reads
// that have a waiting consumer, PriorityBackground for prefetch.
//
// Concurrent FillChunk calls for the same (dgst, chunkIdx) are coalesced:
// the second caller waits until the first call's content-store write is
// complete, then returns without issuing a second fetch.
//
// FillChunk verifies the fetched bytes against the chunk's per-chunk hash
// before writing to the content store; a mismatch returns an error and leaves
// the content store entry absent.
//
// After a successful call, MissingChunks will no longer return the named
// chunk and the chunk's content-store entry is present for the cache to read.
FillChunk(ctx context.Context, dgst digest.Digest, chunkIdx int,
          p ByteProvider, priority Priority) error
```

The coalescing behaviour is important for correctness under the cachefiles
ondemand delivery path, where the kernel may issue multiple concurrent READ
events that map to the same chunk.  The per-`(blob, chunkIdx)` coalescing gate
is an `errgroup`-style in-flight table held in the store's in-process state; it
is not persisted across restarts (a restart simply re-fetches).

`FillChunk` deliberately does **not** decompress the chunk or write anything to
the sparse-file cache.  Decompression and cache population are the
responsibility of the cache layer after `FillChunk` returns.

#### ReaderAt semantics under lazy mode

The existing `ReaderAt(ctx, desc)` method is unchanged in API but gains lazy
semantics when the blob is partially hydrated: on a read into an unfetched chunk
it issues a foreground `FillChunk` internally, waits for it to complete, then
proceeds.  This makes `ReaderAt` a simple blocking interface for callers that do
not want to coordinate with a separate cache.

Callers that need non-blocking reads (e.g. the cachefiles daemon, which must
acknowledge kernel events promptly) should use `MissingChunks` + `FillChunk`
directly rather than going through `ReaderAt`.

#### Provider binding in the metadata record

The provider name stored in `Info.Provider` is a hint for daemon restarts: on
startup the indexed content store re-binds each metadata record to the named
provider (if one is registered) so that lazy fetches can resume without the
original caller.  If the named provider is not available (e.g. credentials have
expired), `FillChunk` returns an error and the cache signals the delivery layer
to surface it as EIO.

Removing the provider binding (e.g. after full hydration) is permitted by
calling `Update` with the `provider` fieldpath and an empty string.  Once the
binding is removed, `FillChunk` requires an explicit provider argument.

---

### Garbage Collection

The indexed content store does not implement its own GC traversal. It plugs
into containerd's existing leases and label-based GC through a **GC
label-namespace extension** that resolves indexed-content annotations into
content-store digests at GC time. This keeps every GC object in the shared metadata BoltDB
while allowing chunks to be tracked individually as content-store entries.

#### Reference graph

The graph has three layers:

1. **Core objects** in core BoltDB (Image, Manifest, Snapshot, Container,
   Active Mount) reference indexed-content blobs through
   `containerd.io/indexed-content.ref/<key>=<digest>` annotations only.
   These are plain string-string entries on existing core records — no schema
   change to core BoltDB beyond honouring a new label namespace.
2. **The indexed-content GC extension** registers itself as the resolver for
   the `containerd.io/indexed-content.ref/*` namespace. When core GC encounters
   one of these annotations, it hands the `<digest>` to the extension; the
    extension reads the named blob's chunk-index record from the indexed-content
    metadata buckets and returns the set of content-store digests that record
    names (the chunks, plus the contiguous-blob entry if one was retained).
3. **The content store** treats those returned digests as ordinary GC roots.
   Chunks shared across multiple indexed-content blobs are reachable as long as
   any annotation pins any blob whose record names them; once the last such pin
   is released, the chunk falls out of the reachability set and is collected by
   content-store GC on its normal schedule.

```
┌──────────────────────────┐
│  Image                   │
└────────────┬─────────────┘
             │ (descriptor digest)
             ▼
┌──────────────────────────┐
│  Manifest                │
│  (in content store)      │
└────────────┬─────────────┘
             │ annotation:
             │ indexed-content
             │   .ref/<key>=<dgst>
             ▼
┌──────────────────────────┐
│  GC Extension            │
│  · reads chunk-index     │
│  · emits chunk digests   │
└────────────┬─────────────┘
             │ content-store digests
             ▼
┌──────────────────────────┐
│  Content Store           │
│  · per-chunk entries     │
│  · optional whole-blob   │
│  · collected by GC when  │
│    unreferenced          │
└──────────────────────────┘
```

Active mounts follow the same pattern. An **active mount** records:

- A snapshot reference (existing core mechanism).
- One or more `containerd.io/indexed-content.ref/<key>=<digest>` annotations
  naming the blobs it serves bytes from.

The annotations are resolved through the same GC extension.

#### What the GC extension does

When containerd's GC enumerates references on a core object, it encounters an
annotation under the registered namespace. The extension hook receives
`(annotationKey, annotationValue)` and returns the set of content-store digests
that should be treated as reachable. The implementation:

1. **Looks up the chunk-index record** keyed by `annotationValue` in the
   indexed-content metadata buckets.
2. **Returns** every content-store digest the record names: the chunk content-
   store digests in chunk-index order, plus the contiguous blob's digest if a
   whole-blob entry was retained at ingest.
3. **Does not** recurse — a chunk is a leaf from GC's perspective.

The extension is also called when an annotation is removed. It does no
collection work itself: removing the last annotation referencing a blob removes
the blob's chunks from the GC reachability set, and content-store GC takes care
of the actual delete on its next pass. The indexed-content metadata record is
deleted once GC has confirmed no annotation anywhere points at the blob.

#### Why this is safe and finite

- **Reachability stays in core GC.** Every digest that pins a chunk is a
  content-store digest. If the extension is unavailable (e.g. during an
  upgrade), GC pauses on that namespace and completes once it is back; nothing
  is collected unsafely.
- **Per-blob extension cost is bounded by the chunk-index record size.** A
  500 MiB blob at 4 MiB chunks has ~125 chunks; the extension returns ~125
  digests per call.
- **No new global locks.** The indexed-content buckets are read-only during a
  GC pass (the GC extension runs under a read transaction); ingest writes happen
  under leases in separate write transactions.
- **Cache state follows chunks.** Cachefiles backings, sparse-file caches, and
  any other on-host cache state are derived from per-chunk content-store
  entries. When a chunk is collected, its derived cache state is collected with
  it.

#### What is not a GC object

- **Indexed-content metadata records** are not GC roots. A record is created at
  ingest, read on every GC pass that hits the blob, and deleted by the
  extension's removal hook once the blob is unreferenced by any core annotation.
- **Cache state** (cachefiles cookies, sparse bitmaps, daemon in-flight maps)
  is host-local operational state. It is rebuilt from the chunk-index record
  when a blob is reactivated and torn down when the underlying chunks are
  collected.

#### Cross-store reference shape (annotation-only)

| Annotation | Carried on | Meaning |
|---|---|---|
| `containerd.io/indexed-content.ref/<key>=<digest>` | Manifest content-store entry, snapshot record, container record, active-mount record | The named blob digest is an indexed-content blob this object depends on. The `<key>` segment is producer-chosen and lets a single object name multiple indexed-content blobs (one per layer, plus aggregates). |

There is no other kind of cross-store edge. Code outside the indexed-content
plugin does not read or write the indexed-content metadata buckets directly.

#### GC traversal cost

GC traversal cost is **O(annotations on reachable core objects)** in the
extension, plus content-store GC's normal cost of **O(content-store entries)**.
The extension does a constant number of bucket lookups per annotation; it does
not iterate all indexed-content records. Per-blob extension work is one bucket
read plus a fixed enumeration of chunk digests, regardless of chunk count.

#### Lease semantics

Standard containerd lease behaviour applies:

- A lease holding an image or snapshot keeps its referenced indexed-content
  blobs and therefore their chunks alive transitively, because the lease pins
  the core object whose annotations are resolved through the GC extension.
- Active mounts hold their own annotations; they keep their blob and chunks
  alive for as long as the mount is activated.
- Direct blob leases are supported by adding a lease that names a core object
  carrying the right annotation. The resolution path is identical.
- When the last reference is released, GC removes the core object, the
  extension's removal hook fires, the indexed-content metadata record is
  deleted, and content-store GC reclaims the chunks on its normal schedule.

---

### Producer Path

In v1 the in-tree producers are the **EROFS differ** (`plugins/diff/erofs/`)
and **`ctr image convert --erofs`**. Both write produced EROFS layers (with
optional inline merkle tree per [erofs-image-spec §3.5][spec-3]) to the indexed
content store via `Write`, attaching EROFS image-format annotations on the
descriptor. They run in-process when embedded; otherwise they invoke the gRPC
proxy. The indexed content store does not synthesise hash trees itself;
producers compute and supply them along with the chunk-index per-chunk
checksums that become content-store digests.

#### Ingest pipeline

A `Write` of a complete blob proceeds as follows:

1. **Receive bytes.** The producer streams the blob into the indexed content
   store. The store hashes the byte stream as it arrives so it can verify
   against the producer-supplied descriptor digest at `Commit` time.
2. **Locate the chunk index.** The chunk index is at the byte range named by
   `org.erofs.chunk-index.range`. The store reads the index into memory; for
   `application/vnd.erofs.chunk-index.v1` this is the 32-byte header
   plus N chunk entries (N = `NumChunks`), each carrying its on-blob
   offset (`BlockOffset`), decompressed-stream offset
   (`UncompressedOffset`), and per-chunk checksum when present.
3. **Verify the chunk index.** The chunk-index bytes are hashed and compared
   against `org.erofs.chunk-index.digest` when present.
4. **Split into chunks.** Walk the chunk-index entries in order; for each
   entry:
   - Slice the chunk's on-blob byte range from the streamed input (or, when
     the source is a content-store-backed reader, from the source entry).
   - Hash the on-blob chunk bytes and verify the result matches the entry's
     per-chunk checksum.
   - Write the chunk to the content store **under the per-chunk checksum
     digest**. If the content store already has an entry under that digest
     (cross-blob dedup), the write is a metadata no-op.
5. **Record the chunk-index record.** With every chunk's content-store digest
   in hand, the store writes a chunk-index record into the indexed-content
   metadata bucket keyed by the logical blob digest. This record is the
   canonical mapping the GC extension reads later, the reader uses to assemble
   byte streams, and the cache uses to populate kernel-side backing. The
   authoritative source of the chunk-index payload digest is the
   `org.erofs.chunk-index.digest` descriptor annotation (spec §2.3); consumers
   verify the index using that annotation before relying on the chunk entries.
6. **Optionally retain a whole-blob entry.** By default the contiguous blob
   bytes are not duplicated into a content-store entry; the byte stream is
   reproducible by streaming the chunks in offset order. A producer can opt
   into a whole-blob entry (for example to support pushing the blob unchanged
   back to a registry) via a `WriteOpt`.

The eager ingest path above is triggered by calling `Write` without
`WithLazyProvider`.  To perform a **lazy ingest** — recording metadata without
fetching chunk bytes — pass `WithLazyProvider(p)` in the `WriteOpt` list;
the writer then follows the reduced flow described in
[§Lazy Mode and Missing Chunks](#lazy-mode-and-missing-chunks).

The chunk-extraction pass at step 4 is throughput-critical. The chunk-index
record gives every chunk's exact byte range up front, so the ingest pipeline
can be parallelised: chunks read from the input in arbitrary order, hash-
verified, and written to the content store concurrently. On filesystems that
support reflink / `copy_file_range` between the source content-store entry
(when the producer is operating over an already-fetched blob) and the
destination chunk entries, chunk extraction is a metadata copy with no
byte-level read-write traffic.

The metadata commits at steps 4 and 5 join any `bolt.Tx` on the context (see
§Transaction sharing in the Storage area); when the caller holds a shared
transaction open, all chunk-index and content writes land in a single fsync.

#### Differ integration

The EROFS differ already detects EROFS-eligible layers, runs
`MountsToLayer`, and writes the produced layer file (see
`plugins/diff/erofs/differ.go:100-245`). The indexed-content-aware path
short-circuits the file write: instead of writing `layer.erofs` into the
snapshotter directory, it opens an `IndexedContent.Writer`, streams the
produced bytes, and on `Commit` the indexed content store extracts the chunks
per the ingest pipeline above. The differ then attaches
`containerd.io/indexed-content.ref/<key>=<digest>` to the snapshot's labels via
the existing snapshotter Update call.

#### Why chunks must be produced quickly

GC extension cost scales with the work the extension does to expand annotations
into reachability sets. Chunk extraction must therefore be a fixed cost paid
once, at ingest:

- The chunk index names every chunk by its on-blob hash, so computing the
  chunk's content-store digest is a single lookup into the chunk-index record.
  No re-hashing of producer-supplied chunks is required.
- Each chunk is splittable from the original blob with a fixed byte-range read
  driven by the chunk-index record. The differ produces chunks contiguously,
  so this is a streaming pass.
- Verification is one hash computation per chunk on first ingest; subsequent
  reads from the chunk content store reuse the content store's existing
  verification semantics with no additional indexed content store work.
- The chunk-index record itself is materialised once at ingest and read-mostly
  thereafter. A GC pass walks the record sequentially per blob; the record fits
  in tens of KiB and is served from BoltDB's page cache in memory.

The dominant GC cost therefore remains O(blobs reachable from core objects)
and not O(chunks).

#### Batched ingest at scale

A 500 MiB layer at 4 MiB chunks produces ~125 chunk content-store entries; a
1 GiB image with 8 such layers produces ~1,000. Without transaction sharing,
each becomes a separate BoltDB write transaction with its own fsync. By
passing a shared transaction on the context (see §Transaction sharing in the
Storage area), concurrent chunk writers stream bytes to disk in parallel and
the entire layer's chunk-index and content writes land in one metadata
transaction. Combined with reflink-friendly chunk extraction, the per-chunk
cost approaches the cost of the underlying byte write itself with no
per-chunk transaction tax.

---

### Distribution

The indexed content store integrates with image transfer along two paths.

#### Eager hydration

For "pull then run" semantics where every byte is local before the container
starts:

1. Transfer pulls manifests, configs, and layer descriptors and discovers which
   layers are indexed-content-eligible. With the descriptors in hand it knows
   the full set of objects the pull will produce.
2. Transfer opens a metadata write transaction (`db.Update`) for the pull and
   pushes it onto the context with `boltutil.WithTransaction`. All indexed
   content and content-store writes below share this transaction.
3. For each indexed-content layer, transfer launches a parallel ingest: the
   EROFS differ (or a direct chunk-extraction path) reads the layer bytes and
   writes per-chunk ingests per the ingest pipeline. The Local Content Provider
   short-circuits any chunk already present (cross-image dedup), so a second
   copy is never staged.
4. When all layers finish writing, the shared transaction is committed. Every
   staged chunk content-store entry, every blob's chunk-index record, and every
   `containerd.io/indexed-content.ref/<key>=<digest>` annotation land in the
   same fsync — or in a small number of fsyncs if the caller chose to split into
   sub-batches by size.
5. Once the transaction commits, every chunk is present in the content store
   under its per-chunk hash and the blob is fully reachable.

The eager path matches today's non-indexed-content experience: pull completes
when bytes are local; runs do not depend on registry availability.

#### Lazy hydration

For images that must start before bytes are local:

1. Transfer pulls manifests, configs, and the small metadata blobs needed
   before activation: the dm-verity merkle tree and the chunk index (located
   via the `org.erofs.chunk-index.range` annotation).
2. Transfer opens a metadata write transaction and pushes it onto the context.
   For each lazy-eligible layer it writes one chunk-index record into the
   indexed-content metadata buckets — carrying every chunk's per-chunk digest,
   the registry-provider binding, and the credential handle — and finalises
   the transaction once the manifest is committed. No chunk bytes are fetched.
3. The blob exists in the indexed-content metadata buckets with all chunk
   digests known but most chunks not yet present in the content store.
4. On first activation, the delivery daemon walks the chunk-index record to
   translate kernel read events into per-chunk fetches. Concurrent fetches for
   the same blob share a single write transaction pushed on the activation
   context, so a burst of adjacent-chunk reads commits in one metadata
   transaction.

The lazy path turns the **chunk-index record** into the durable hydration plan
and the **content store** into the durable chunk cache: chunk presence in the
content store is the same fact as chunk availability for kernel reads, and
there is no separate "populated cache" abstraction with its own lifecycle.

A hybrid is natural: pull chunk tables and dm-verity trees eagerly (small and
required before any read), pull data chunks lazily, and optionally prefetch hot
chunks. Each phase opens its own transaction scope.

#### Selection

Image transfer's existing `os.features` selection drives differ and snapshotter
choice. Indexed-content-store eligibility additionally checks host capability
(kernel features, EROFS fscache support) and falls back to non-indexed-content
paths when unavailable, so a single image can run on hosts with mixed capability
levels.

---

### Snapshotter Coordination

The indexed content store and snapshotters are deliberately decoupled. The
snapshotter does not need to know about loop devices, dm-verity setup, fscache
cookies, or chunk tables — those concerns live in the blob store and the
Lazy-load Mount Manager (a later milestone).

**What the snapshotter records on apply:**

- A label associating the just-applied layer with a blob digest:
  `containerd.io/indexed-content.ref/<key>=<digest>`.
- That label is sufficient for `Mounts()` to resolve the blob's mount specs
  later. No copy of layer bytes lives in the snapshotter directory when the
  indexed-content path is used.

**What happens at `Mounts()`:**

1. The snapshotter consults its per-snapshot state to determine which blobs
   make up the layer chain.
2. For each blob, it calls `IndexedContent.Mounts(digest)` and collects the
   returned `[]mount.Mount`.
3. It composes the result with its own mount specs (overlay upperdir, workdir,
   fsmerge metadata, uid/gid mapping, etc.) and returns the full mount stack.

An existing snapshotter therefore needs only:

- How to record a blob digest at commit (one new label).
- How to call `IndexedContent.Mounts()` and splice the result into its mount
  output.

It does not need to learn cachefiles, dm-verity, loopback, or any
kernel-specific delivery. New delivery mechanisms become available without
snapshotter changes.

---

### Credentials

Lazy hydration creates a credential lifecycle problem the content store does
not have: bytes may be fetched hours or days after the original pull command
returns. Registry credentials must remain available and refreshable for the
lifetime of any blob that has not been fully hydrated locally.

#### Storage model

- **In-memory cache.** Credentials in current use are held in a containerd
  in-process cache, indexed by registry hostname.
- **At-rest store.** The cache is durably backed by an encrypted blob in
  containerd's metadata directory. The encryption key is managed outside the
  metadata store:
  - *Daemon-restart durability*: key derived from a process-bound secret kept
    in the kernel keyring (`add_key(2)` / `KEY_SPEC_PROCESS_KEYRING` or
    systemd's per-service keyring). Survives `systemctl restart containerd`
    because the keyring is preserved across restarts of the same service unit.
  - *Reboot durability* (optional, configurable): key derived from a
    machine-bound secret — TPM-sealed key, persistent kernel keyring, or an
    external KMS.
- **Refresh path.** A `RefreshCredential(host, blob)` RPC lets the original
  transfer caller re-supply credentials after a daemon restart or token expiry.
  The Registry Provider blocks fetches on a credential miss until refresh
  arrives, with a configurable timeout.
- **Delegation.** When an external credential helper is configured, the indexed
  content store records a reference to the helper invocation rather than the
  credential itself. The helper is invoked on demand; no credential ever touches
  containerd's disk.

#### Defaults

- *Daemon-restart durability*: on by default (kernel-keyring-bound key).
- *Reboot durability*: off by default.
- *Helper delegation*: opt-in via configuration.

#### Failure modes

- **Credential expiry mid-fetch**: the Registry Provider returns an
  authentication error; the delivery daemon turns this into an EIO ack so the
  kernel returns EIO rather than hanging the reader. The indexed content store
  marks the host as needing-refresh.
- **No refresh forthcoming**: configurable — fail activation hard, or fall back
  to a less-privileged provider (the Local Content Provider if the layer is
  also in the content store) until refresh arrives.
- **Helper unavailable**: same handling as expiry. Helper invocations are
  bounded with a per-call timeout.

#### Out of scope for v1

- Cross-host credential federation.
- Per-namespace credential isolation beyond what the existing transfer service
  already provides.

---

### API Surface

The indexed content store is exposed as a Go interface; a gRPC service proxies
that interface in the typical containerd architecture (in-process for embedded
clients, gRPC for cross-process). Producers and consumers call the same
surface.

All `Store` methods inspect the context for an existing `*bolt.Tx` via
`boltutil.Transaction`. When one is present they join it; otherwise they open
their own transaction. Callers that want to batch multiple writes into a single
fsync push a transaction onto the context with `boltutil.WithTransaction`
before calling into the store (see §Transaction sharing).

#### Go interface

```go
package indexedcontent

import (
    "context"
    "io"
    "time"

    mount   "…/containerd/core/mount"
    digest  "…/go-digest"
    ocispec "…/image-spec/specs-go/v1"
)

// Info is the per-blob metadata record.
type Info struct {
    Digest     digest.Digest
    Size       int64
    Descriptor ocispec.Descriptor
    NumChunks  uint32
    DmVerity   *DmVerityInfo
    Provider   string
    Labels     map[string]string
    CreatedAt  time.Time
    UpdatedAt  time.Time
}

// DmVerityInfo mirrors format annotations.
type DmVerityInfo struct {
    RootDigest digest.Digest
    Offset     int64
    BlockSize  uint32
}

// Priority indicates the urgency of a chunk fetch.
// See designs/block-provider.md for the full specification.
type Priority int

const (
    // PriorityForeground is used when a consumer is blocked waiting
    // for the chunk.  Foreground requests bypass any prefetch queue.
    PriorityForeground Priority = iota
    // PriorityBackground is used for prefetch; it fills remaining
    // provider concurrency slots after foreground requests are served.
    PriorityBackground
)

// Store is the top-level interface.
// The gRPC service mirrors it.
// All methods join a *bolt.Tx on the
// context if present (boltutil.Transaction),
// opening their own otherwise.
type Store interface {
    Info(
        ctx  context.Context,
        dgst digest.Digest,
    ) (Info, error)

    // Stat omits provider/cache state.
    Stat(
        ctx  context.Context,
        dgst digest.Digest,
    ) (Info, error)

    List(
        ctx     context.Context,
        filters ...string,
    ) ([]Info, error)

    Delete(
        ctx  context.Context,
        dgst digest.Digest,
    ) error

    Update(
        ctx        context.Context,
        info       Info,
        fieldpaths ...string,
    ) (Info, error)

    // Mounts returns specs to expose the
    // blob as a mountable device.
    Mounts(
        ctx  context.Context,
        dgst digest.Digest,
    ) ([]mount.Mount, error)

    // Write ingests a new blob.
    // Pass WithLazyProvider(p) in opts to perform a lazy ingest:
    // only the chunk-index section is fetched; chunk bytes are deferred.
    Write(
        ctx  context.Context,
        ref  string,
        desc ocispec.Descriptor,
        opts ...WriteOpt,
    ) (Writer, error)

    // MissingChunks returns the ChunkRefs whose bytes are not yet
    // present in the content store, in chunk-index order.
    // A nil slice means the blob is fully hydrated.
    // Cheap: reads only the ordered per-chunk digest list from the
    // metadata record and probes the content store for each.
    MissingChunks(
        ctx  context.Context,
        dgst digest.Digest,
    ) ([]ChunkRef, error)

    // FillChunk fetches one chunk through provider p, verifies its
    // per-chunk hash, and writes it to the content store.
    // priority is forwarded to p.Fetch.
    // Concurrent calls for the same (dgst, chunkIdx) are coalesced.
    FillChunk(
        ctx      context.Context,
        dgst     digest.Digest,
        chunkIdx int,
        p        ByteProvider,
        priority Priority,
    ) error
}

// Writer is the producer-side ingest handle.
type Writer interface {
    io.WriteCloser
    Digest() digest.Digest
    Commit(
        ctx      context.Context,
        expected digest.Digest,
        opts     ...CommitOpt,
    ) error
}

// ByteProvider sources the bytes of individual chunks from a non-local
// location (registry, peer-to-peer source, cloud volume).
// The full interface is specified in designs/block-provider.md.
// The Open method is retained for eager blob-level ingest (pull-then-run);
// Fetch is the per-chunk interface used by lazy loading.
type ByteProvider interface {
    Name() string
    // Open downloads the full blob for eager ingest.
    // Deprecated in favour of Fetch for lazy paths.
    Open(
        ctx  context.Context,
        desc ocispec.Descriptor,
    ) (io.ReaderAt, int64, error)
    // Fetch downloads exactly one chunk's on-blob bytes.
    // The returned ReadCloser yields the raw (possibly compressed)
    // on-blob bytes for chunk.  Caller decompresses and verifies.
    Fetch(
        ctx      context.Context,
        desc     ocispec.Descriptor,
        chunk    ChunkRef,
        priority Priority,
    ) (io.ReadCloser, error)
}
```

#### gRPC service

```proto
syntax = "proto3";

package containerd.services.indexed_content.v1;

import "google/protobuf/timestamp.proto";
import "google/protobuf/field_mask.proto";
import "containerd/types/mount.proto";
import "containerd/types/descriptor.proto";

// Write is discrete (not streaming).
service Blocks {
    rpc Info(InfoRequest)
        returns (InfoResponse);
    rpc Stat(StatRequest)
        returns (StatResponse);
    rpc List(ListRequest)
        returns (stream ListResponse);
    rpc Delete(DeleteRequest)
        returns (.google.protobuf.Empty);
    rpc Update(UpdateRequest)
        returns (UpdateResponse);
    rpc Mounts(MountsRequest)
        returns (MountsResponse);
    rpc Write(WriteRequest)
        returns (WriteResponse);
}

message BlockInfo {
    string       digest     = 1;
    int64        size       = 2;
    types.Descriptor desc   = 3;
    uint32       num_chunks = 4;
    DmVerityInfo dmverity   = 5;
    string       provider   = 6;
    map<string, string> labels = 7;
    google.protobuf.Timestamp
        created_at = 8;
    google.protobuf.Timestamp
        updated_at = 9;
}

message DmVerityInfo {
    string root_digest = 1;
    int64  offset      = 2;
    uint32 block_size  = 3;
}

message MountsResponse {
    repeated types.Mount mounts = 1;
}
```

Streaming `Read` is intentionally absent in v1: consumers obtain blobs via
`Mounts()` and read through the resulting kernel device or file.

gRPC handlers always open a fresh transaction per RPC; the shared-transaction
benefit described in §Transaction sharing applies only to in-process callers
that hold a `*bolt.Tx` on the context. Cross-process callers accept one fsync
per call as the baseline cost, matching the existing content-store gRPC
behaviour.

---

## Future Work (post v1)

- **R/W → r/o sealing** — taking a writable filesystem-on-block and registering
  it as a new r/o blob with computed hash tree, including chunk extraction and
  chunk-index record creation.
- **Streaming `Read`/`Write` over gRPC** — for callers that want byte-stream
  access instead of mount-based access.
- **Stable external provider plugin SDK** — a versioned interface out-of-tree
  providers can target.
- **Confidential containers signing and trust roots** — verifying root hashes
  against signed expectations and managing trust policy.
- **Block clone / reflink primitive** — replacing raw file copies of writable
  layer images with an indexed-content-mediated clone when the underlying
  filesystem supports it.
- **P2P providers** (dragonfly-style) as first-class providers, surfacing
  per-chunk fetches as the natural unit of P2P transfer.

- **Cross-namespace chunk dedup accounting** — surfacing the per-chunk fan-in
  (how many indexed-content blobs and namespaces reference each chunk) as
  observability output, useful for capacity-planning hosts that pull many similar
  images.
- **Alternative caching mechanisms** — the caching abstraction deliberately
  avoids prescribing a kernel-side delivery approach; future work includes
  first-class support for NBD-backed caches, object-store-backed caches,
  in-memory diskless delivery for ephemeral workloads, and user-space page-cache
  management via io_uring.
- **Alternative chunk-index media types** — registering parsers for content-
  defined or path-keyed chunk indexes without changes to existing consumers; see
  [erofs-image-spec §2.2][spec-22] for the extensibility model. The current
  format is scoped to `+zstd` layers; future non-zstd formats would be
  registered under separate media types.

[spec-22]: erofs-image-spec/spec.md#22-applicationvnderofschunk-indexv1
[spec-23]: erofs-image-spec/spec.md#23-annotations
[spec-3]:  erofs-image-spec/spec.md#35-dm-verity-merkle-tree
