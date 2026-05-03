# Lazy-load Shim Runtime

This document covers the **shim-side changes** required to host the
cachefiles ondemand daemon inside the per-container shim, so that
runtime mounts of indexed-content entries get correct cgroup accounting,
per-container failure isolation, and clean lifecycle binding.

The mount-manager fallback (used when a shim does not advertise the
indexed-content capability) is documented in the Lazy-load Mount Manager
design document.

## Why the shim

The cachefiles ondemand daemon is the userspace half of the kernel's
fscache backend: every cold read on a blob goes through a daemon
goroutine that fetches bytes (possibly remote, possibly compressed)
and fills the backing file the kernel reads from. Where this daemon
runs matters:

- **cgroup accounting** — I/O issued by the daemon falls in whatever
  cgroup the daemon's process is in. If it runs in containerd, all
  byte-fetch I/O is accounted to containerd, not the container the
  bytes are for. If it runs in the shim — which is in the container's
  cgroup — accounting is correct.
- **memory accounting** — buffers held while decompressing or while
  in-flight reads pile up come from the daemon's process memory; same
  cgroup logic applies.
- **failure isolation** — a wedged provider for one container's blobs
  shouldn't stall reads for another. Per-shim daemons localize
  failure to the affected container.
- **lifecycle binding** — when the shim exits, its `/dev/cachefiles`
  fd is closed automatically, which closes cookies and unwinds the
  mount cleanly. A containerd-hosted daemon would need explicit
  unmount sequencing on container teardown.

These properties are why the design positions the shim as the
preferred home for the daemon, with the mount-manager fallback handling
shims that can't (or don't want to) carry that responsibility.

## Capability advertisement

Shims advertise support for the `block` mount type at startup, as part
of the existing shim service Info handshake.

```proto
// Sketch — extend the existing shim Info response with mount-type
// capabilities. Real surface lands in api/runtime/task/v3 or wherever
// shim Info currently lives.

message InfoResponse {
    // ... existing fields ...
    repeated string supported_mount_types = N;  // e.g. "block"
}
```

The mount manager queries the shim via this surface during container
creation. If `"block"` is in the list, the mount manager dispatches
block-typed mount entries to the shim; otherwise it falls back to its
own daemon per the Lazy-load Mount Manager design document.

A shim opts in once and the mount manager respects the choice for the
lifetime of the container; no per-mount negotiation is needed.

### Compatibility

- **Shims that do not advertise** — the mount manager handles the mount
  itself in containerd's namespace, then bind-mounts the result into
  the container. Works with any shim, including out-of-tree shims that
  predate this proposal.
- **Old shims that do not understand the capability field** — treated
  as "does not advertise"; same fallback path.
- **Shims that advertise but fail to mount** — the shim returns an
  error from its mount call; the mount manager may retry via the
  fallback path or surface the error, depending on configuration.

## Shim-hosted daemon

The shim's responsibilities when handling a `block` mount:

1. **Cachefiles bind**: open `/dev/cachefiles`, write `dir <path>`,
   `tag <name>`, `bind ondemand`. The bound directory lives in
   shim-owned storage tied to the container's data directory.
2. **Mount**: perform the EROFS fscache mount with
   `fsid=<blob-digest>,tag=<bind>` so the kernel's fscache backend
   correlates events back to the blob.
3. **Daemon goroutine**: spawn a goroutine pool that consumes
   `OPEN`/`READ`/`CLOSE` events on the cachefiles fd. The same
   responsibilities apply as in the mount-manager-owned daemon
   (cookie lookup, granule translation, decompression, bounding,
   error → EIO ack) but with one important difference: byte fetches
   go back to containerd over ttrpc rather than calling Provider
   implementations directly.

### Why ttrpc to containerd, not direct provider calls

Providers can hold privileged state — registry credentials, content
store handles, KMS bindings, helper-process invocations. The shim is
not the right place to hold any of that:

- Credentials should not be replicated into per-container processes
  for blast-radius and refresh-timing reasons.
- Content store access from the shim would require shim-side BoltDB
  read access and content directory access, which today the shim does
  not have.
- Cross-container provider state (caches, dedup, refcounts) would
  fragment if each shim called providers itself.

Routing fetches via ttrpc keeps providers in containerd, the daemon in
the shim, and the data plane between them small and well-defined.

## ttrpc protocol

The shim calls back to containerd's indexed content store over the existing
ttrpc surface (the same channel containerd already uses for shim ↔
runtime communication; see `core/runtime/v2/`). One new ttrpc service
is added:

```proto
syntax = "proto3";

package containerd.services.indexed_content.shim.v1;

// Sketch — minimal RPC the shim needs to serve cachefiles events.
service IndexedContentShim {
    // Open registers the shim's interest in a blob. Returns the
    // blob.s metadata and the chunk-index summary the shim needs to
    // translate kernel READ ranges into provider fetches.
    rpc Open(OpenRequest) returns (OpenResponse);

    // Read fetches a range of bytes from the blob via the
    // configured Provider. Containerd handles credentials,
    // content-store reads, and any decompression.
    rpc Read(ReadRequest) returns (ReadResponse);

    // Close releases the shim's reference on the blob.
    rpc Close(CloseRequest) returns (.google.protobuf.Empty);
}

message OpenRequest {
    string blob_digest = 1;
}

message OpenResponse {
    int64  size                   = 1;
    repeated ChunkOffset chunks   = 2;  // for shim-side range planning; per-chunk
                                        // lengths derived from successive
                                        // uncompressed_off deltas (spec §3.4.2)
    bytes  dmverity_root          = 3;  // root hash, for shim verification awareness
    int64  dmverity_off           = 4;  // uncompressed offset of merkle tree (spec §3.5)
    uint32 dmverity_block_size    = 5;  // defaults to 4096 when absent (spec §2.3)
}

message ChunkOffset {
    int64  uncompressed_off = 1;
    int64  compressed_off   = 2;
    int64  compressed_len   = 3;
}

message ReadRequest {
    string blob_digest = 1;
    int64  offset       = 2;
    int64  length       = 3;
}

message ReadResponse {
    bytes data = 1;
}
```

Four notes:

- **Read returns bytes, not chunks.** Containerd is responsible for
  decompression; the shim writes plain bytes into the cachefiles
  backing file. This keeps zstd state out of the shim and matches
  how providers expose `io.ReaderAt`.
- **Open returns the chunk table.** The shim uses it to align
  kernel-side READ ranges to chunk boundaries before calling Read,
  so containerd does not have to repeat this work for every kernel
  READ. Per-chunk lengths are derived from successive
  `ChunkOffset.uncompressed_off` deltas, following spec §3.4.2.
- **Read is bounded.** The shim must not request unbounded ranges;
  the cache's two-level granularity (chunk for fetch, blob for
  verify) bounds the maximum useful Read size to one chunk
  (typically a few MiB). Containerd may chunk Read responses
  internally.
- **Per-chunk integrity is verified containerd-side.** Containerd
  verifies each chunk's on-blob bytes against the
  `org.erofs.chunk-index.digest`-authenticated chunk-index entry
  at ingest (spec §3.4.5). The shim writes pre-verified bytes into
  the cachefiles backing file; no separate checksum verification is
  required in the shim.

The protocol is **discrete-call** rather than streaming, matching the
discrete-call style used in the rest of the Indexed Content Service API.
A streaming Read variant is Future Work.

## Activation flow

```
1. Client requests container start with image whose layers are indexed-content blobs.
2. Containerd runtime asks the shim for its capabilities; shim
   advertises "block" in supported_mount_types.
3. Containerd asks the snapshotter for Mounts(); snapshotter calls
   IndexedContent.Mounts() per blob; result includes `block` mount entries.
4. Containerd hands the full mount stack to the shim.
5. Shim sees `block` entries:
   a. Calls IndexedContentShim.Open(digest) for each → gets blob metadata.
   b. Performs cachefiles bind on its data directory.
   c. Spawns daemon goroutine on the cachefiles fd.
   d. Performs EROFS fscache mount (fsid=<digest>, tag=<bind>).
6. Container proceeds; reads through the mount trigger
   READ events the daemon services via IndexedContentShim.Read.
7. Container exit:
   a. Shim unmounts; cachefiles fd close releases cookies.
   b. Shim calls IndexedContentShim.Close(digest) for each blob.
   c. Shim exits; mount manager records mount as deactivated.
```

Step 5 is where the shim does the work documented in
[Shim-hosted daemon](#shim-hosted-daemon). Steps 6 and 7 are normal
runtime lifecycle, with the cachefiles fd's lifetime acting as the
mount lifetime.

## Lifecycle and failure modes

**Shim crash mid-container**: cachefiles fd closes with the shim
process; the kernel returns EIO on outstanding READs; container reads
fail with EIO. Containerd's existing shim-restart logic applies; on
restart the shim performs the bind and mount again, hydrating the
cache from where it left off (the backing file's extent map records
what was fetched).

**Provider failure mid-fetch**: the shim's daemon turns the error
into a non-zero `READ` ack so the kernel returns EIO. The shim does
not need provider-specific recovery — the next fetch attempt will
re-call IndexedContentShim.Read, and containerd will retry or surface the
error per its provider's policy.

**Containerd restart**: shim and its daemon survive (containerd is
not in the data path of warm reads). Cold reads block on
IndexedContentShim.Read until containerd is back; the shim's ttrpc client
handles reconnection. The shim's pre-fetched blob metadata
(returned by Open) survives the restart because it is in shim memory.

**Credential expiry on the containerd side**: the IndexedContentShim.Read
call returns an authentication error; the shim's daemon turns it
into EIO. The indexed content store records the host as needing-refresh; once
refresh arrives via the standard credential refresh path described in the
Indexed Content Service design document, subsequent reads succeed.

## Out of scope

- **Shim-side Provider implementations.** The shim never holds
  provider state directly. If, in the future, certain providers want
  to live on the shim side (e.g., a P2P provider that requires
  per-host networking state), that's an extension; v1 keeps providers
  in containerd.
- **Per-shim cachefiles bind sharing.** Each shim has its own bind;
  cookies are not shared across shims. Sharing would couple shim
  lifetimes and is not a v1 goal.
- **Streaming Read over ttrpc.** Discrete `Read` requests; if benchmarks
  show the per-call overhead matters, a streaming variant is Future
  Work.

## Future Work (post v1)

- **Streaming Read over ttrpc** to amortize per-call overhead for hot
  blobs.
- **Direct provider hand-off** for providers safe to delegate (e.g.,
  signed-URL fetchers with no rotating credentials) so the shim can
  bypass the ttrpc round trip.
- **Per-container observability** — the shim is the right place to
  expose per-container fetch latency, hit rate, and bandwidth metrics
  through standard runtime metrics surfaces.
- **VM-runtime integration** — Kata-style shims can either run the
  daemon on the host (current design) or proxy events into the guest
  for guest-local cachefiles; the latter is a future option for
  confidential-container scenarios where the host cannot see plaintext
  bytes.
