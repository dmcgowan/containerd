# Block Provider

This document specifies the **block provider** component, one of three
components that compose the chunked lazy-loading pipeline for EROFS image
layers.

The other two components are described in:

- [indexed-content.md](indexed-content.md) — the indexed content store that
  tracks chunk metadata and drives verification.
- [cache.md](cache.md) — the sparse-file cache that holds the uncompressed
  image bytes for a running mount.

The block provider is the byte source of last resort: it is invoked when a
chunk is not yet present in the content store and must be fetched from a
remote or non-local location.

---

## Role

A block provider:

1. **Serves individual chunks** from a remote source (registry, peer-to-peer
   network, cloud volume) given the chunk's on-blob byte range.
2. **Enforces global download limits**: per-host connection concurrency and an
   optional byte-rate budget are enforced internally, so callers never need to
   coordinate across providers.
3. **Supports two priority levels**: foreground (a consumer is blocked waiting)
   and background (prefetch).  Foreground requests are always issued ahead of
   background requests within the same concurrency budget.
4. **Does not cache**.  Writing fetched bytes to the content store is the
   responsibility of the indexed content store's `FillChunk` path.
   Decompression and per-chunk hash verification are also the caller's
   responsibility.

The provider is a plugin, registered under the
`io.containerd.content.index.provider.v1` namespace.  The in-tree registry
provider is the default; out-of-tree providers (P2P, cloud-volume, CDN) can be
registered without changes to the indexed content store or the cache.

---

## Interface

```go
package provider

import (
    "context"
    "io"

    contentindex "github.com/containerd/containerd/v2/core/content/index"
    ocispec        "github.com/opencontainers/image-spec/specs-go/v1"
)

// Priority indicates the urgency of a Fetch call.
type Priority int

const (
    // PriorityForeground is used when a consumer goroutine is blocked
    // waiting for the chunk.  A foreground request is always dispatched
    // ahead of background requests and is guaranteed at least one
    // concurrency slot even when the host limit is saturated.
    PriorityForeground Priority = iota

    // PriorityBackground is used for speculative prefetch.  Background
    // requests fill remaining concurrency slots after foreground requests
    // are served.
    PriorityBackground
)

// ByteProvider sources the compressed on-blob bytes of individual chunks.
//
// Providers do not decompress, verify, or cache.  Those steps are handled
// by the indexed content store after Fetch returns.
//
// Providers are registered as "io.containerd.content.index.provider.v1"
// plugins and are addressed by Name() in indexed-content metadata records.
type ByteProvider interface {
    // Name returns a stable identifier used in operator-visible records and
    // plugin registration logs (e.g. "registry:ghcr.io/containerd/alpine").
    Name() string

    // Fetch returns the raw on-blob bytes for chunk c of blob desc.
    //
    // The returned ReadCloser yields exactly the bytes in the half-open
    // interval [c.OnBlobStart, c.OnBlobEnd) of the original blob — the
    // compressed zstd frame for application/vnd.erofs+zstd layers, or the
    // raw bytes for raw application/vnd.erofs layers.  The caller decompresses
    // and verifies the returned bytes before writing them to the content store.
    //
    // priority governs queue ordering: foreground requests bypass any
    // background prefetch queue and are dispatched immediately within the
    // concurrency budget.
    //
    // Implementations MUST honour ctx cancellation and return promptly when
    // ctx is done.  A cancelled Fetch MUST NOT leave a partially-written
    // content-store entry.
    Fetch(
        ctx      context.Context,
        desc     ocispec.Descriptor,
        chunk    contentindex.ChunkRef,
        priority Priority,
    ) (io.ReadCloser, error)

    // Open downloads the full blob for eager (pull-then-run) ingest.
    // The returned ReaderAt is valid for the lifetime of ctx.
    //
    // Open is retained for compatibility with the eager ingest path
    // (designs/indexed-content.md §Producer Path).  New code that only
    // needs lazy loading should use Fetch.
    Open(
        ctx  context.Context,
        desc ocispec.Descriptor,
    ) (io.ReaderAt, int64, error)
}
```

### Priority semantics

Two priority levels are sufficient for v1:

- **Foreground** — issued when a kernel READ event (via cachefiles ondemand) or
  a blocking `ReadAt` call (via loop delivery) is waiting.  The delivery daemon
  must acknowledge the kernel event promptly; a stalled foreground fetch causes
  the reading process to block.  Foreground fetches always get at least one
  concurrency slot (see [Throughput controls](#throughput-controls) below).

- **Background** — issued by the prefetch goroutine in the cache (see
  [cache.md §Prefetching](cache.md#prefetching)).  Background fetches fill
  remaining slots after all pending foreground fetches are dispatched.

Future versions may add sub-levels (e.g. `PriorityPreemptive` for dm-verity
tree pre-fetch), introduced via additional `Priority` constants with a new
minor version of the plugin interface.

---

## Registry provider

The in-tree implementation is `core/content/index/registry/`.  It maps `Fetch`
to a single HTTP Range request against the blob URL served by the registry.

### Fetch implementation

```
GET /v2/<name>/blobs/<digest>
Range: bytes=<c.OnBlobStart>-<c.OnBlobEnd-1>
```

The response body is returned directly as the `ReadCloser`; no buffering is
done in the provider.  The indexed content store reads, decompresses, and
verifies in the `FillChunk` call that owns the `ReadCloser`.

If the registry does not support Range requests (no `Accept-Ranges: bytes`
header) the provider falls back to a full-blob fetch for the first chunk of
that blob, buffering the blob in memory and serving subsequent chunks from
the buffer.  The buffer is released once all chunks for that blob have been
fetched or the context is cancelled.

### Open implementation

`Open` issues a full-blob fetch (`GET /v2/<name>/blobs/<digest>`) and returns
a `ReaderAt` backed by the downloaded bytes, as today.  This path is used for
eager ingest only.

### Configuration

```go
type RegistryConfig struct {
    // MaxConcurrentFetches is the maximum number of simultaneous HTTP
    // requests per registry host.  Foreground requests always get at least
    // one slot; background requests share the remainder.
    // Default: 8.
    MaxConcurrentFetches int

    // MaxBytesPerSecond, if > 0, caps the aggregate download rate across
    // all concurrent fetches for this provider instance.
    // Default: 0 (unlimited).
    MaxBytesPerSecond int64

    // ForegroundReserve is the minimum number of concurrency slots kept
    // exclusively for foreground requests.
    // Default: 1.
    ForegroundReserve int
}
```

---

## Throughput controls

### Concurrency model

Each provider instance maintains:

- A **foreground semaphore** of size `ForegroundReserve`.
- A **shared semaphore** of size `MaxConcurrentFetches - ForegroundReserve`.

A foreground `Fetch` acquires one slot from either semaphore (shared first,
then foreground reserve if shared is full).  A background `Fetch` acquires
from the shared semaphore only and blocks when the shared semaphore is empty.

This model ensures:

- Background fetches never starve foreground ones.
- A burst of background prefetches does not consume all concurrency and leave
  foreground requests queueing behind them.
- Foreground requests are never queued behind background requests.

### Rate limiting

When `MaxBytesPerSecond > 0`, fetched bytes are counted against a
[token bucket][token-bucket] after the HTTP response body is read.  The token
bucket is shared across all concurrent fetches for the provider instance; each
fetch consumes tokens proportional to the number of bytes it delivers.  If the
bucket is empty, the fetch blocks before returning the `ReadCloser` to the
caller.

Rate limiting is applied **after** the remote TCP connection receives data, not
before.  This ensures the remote server does not time out waiting for the
client to read the response body.

### Context cancellation

All semaphore acquisitions respect `ctx` cancellation.  A cancelled foreground
`Fetch` releases its slot immediately so the next queued foreground request can
proceed.

---

## Credential lifecycle

Long-lived lazy blobs require credentials that survive daemon restarts and token
expiry.  The credential lifecycle is specified in [indexed-content.md §Credentials](indexed-content.md#credentials);
the provider is a consumer of that facility, not its owner.

In summary:

- The registry provider requests credentials from the credential store on each
  `Fetch` call; it does not cache tokens itself.
- If a `Fetch` returns `401 Unauthorized`, the provider signals the indexed
  content store to mark the blob as needing-refresh.  Subsequent `Fetch` calls
  block until fresh credentials arrive via the `RefreshCredential` RPC or time
  out and return an error that the delivery layer surfaces as EIO.
- The `Open` path for eager ingest is unaffected; it follows the existing
  transfer-service credential flow.

---

## External providers

Out-of-tree providers implement `ByteProvider` and register as
`io.containerd.content.index.provider.v1` plugins.  The indexed content store
looks up the provider by `Name()` when restoring lazy-ingest metadata records
after a daemon restart.

The provider plugin interface is not declared stable in v1.  A versioned,
stable SDK is future work.

Examples of anticipated external providers:

- **P2P (Dragonfly, Kraken-style)** — `Fetch` issues a per-chunk P2P fetch;
  the P2P daemon handles peer selection and bandwidth sharing.
- **Cloud volume (S3, GCS, Azure Blob)** — `Fetch` issues a signed-URL range
  request; `Open` issues a full-object download.
- **CDN with byte-range acceleration** — `Fetch` routes to the nearest CDN
  edge; fallback to origin for cache miss.

---

## Interaction with the indexed content store

The provider does not call into the indexed content store directly.  The flow
is always:

```
cache.ensureChunk(idx, PriorityForeground)
  → store.FillChunk(dgst, idx, provider, PriorityForeground)
      → provider.Fetch(desc, chunk, PriorityForeground)
          → [HTTP Range GET or equivalent]
      → decompress if +zstd
      → verify per-chunk hash
      → write to content store under chunk.Digest
  → cache reads from content store, writes to sparse file
```

The provider's only dependency is the descriptor (for URL construction) and
the `ChunkRef` (for byte-range bounds).  It has no knowledge of the content
store, the sparse-file cache, or the EROFS format.

---

## Future work

- **Multi-source parallel fetch** — try N providers simultaneously and accept
  the first successful response.  Useful for P2P + registry fallback.
- **Streaming Read over ttrpc** — a streaming variant of `Fetch` for the
  shim-hosted daemon to amortise per-call overhead (see
  [lazy-load-shim-runtime.md](lazy-load-shim-runtime.md)).
- **Provider-level content-defined chunking** — providers that understand
  rolling-hash chunk boundaries can serve arbitrary sub-ranges efficiently;
  requires a future chunk-index format.
- **Stable external provider SDK** — a versioned interface for out-of-tree
  providers to target without depending on containerd internals.
- **Sub-priority levels** — `PriorityPreemptive` for kernel-triggered reads
  that must complete within a deadline (e.g. dm-verity superblock reads at
  mount time).

[token-bucket]: https://en.wikipedia.org/wiki/Token_bucket
