# Lazy-load Mount Manager

This document covers the **mount-manager-side changes** required to
activate indexed-content entries as kernel-side mounts: the new mount type,
the kernel-side delivery options (cachefiles ondemand, loop, NBD), the
mount-manager-owned cachefiles daemon used when a shim cannot host its
own, and the transformer-pipeline updates that compose preparatory
steps (cache directory creation, cachefiles bind, dm-verity wrap) ahead
of the actual filesystem mount.

The shim-side delivery path — when a shim advertises the indexed-content
mount-type capability and runs its own in-process daemon — is covered in
the Lazy-load Shim Runtime design document.

## The `block` mount type

The indexed content store introduces one new mount type:

```
type   = "block"
source = "<blob-digest>"
options = ["target=erofs", "..."]
```

This mount entry is what `IndexedContent.Mounts()` returns alongside any
preparatory steps:

- `format/cachefiles-bind` — bind a cache directory to `/dev/cachefiles`
  before activation, so the kernel's fscache backend has somewhere to
  put backing files.
- `dm-verity` — wrap the resulting device when integrity is verified
  out-of-band (i.e. not via the inline EROFS+fscache path).

`source` is the blob digest. The mount handler resolves it by calling
the indexed content store; this is what makes the blob reachable from
mount-stack consumers without their having to know about indexed-content
internals.

The indexed content store may also emit `X-containerd.indexed-content.id=<digest>` as a
mount option so indexed-content-aware handlers can resolve cache and
provider state without re-reading metadata; the existing mount-DSL
(`X-containerd.dmverity=`, `X-containerd.mkfs.*`, `device=`,
`{{ mount N }}`) is reused everywhere it applies.

## Delivery

The cache layer (specified in [designs/cache.md](cache.md)) holds the
uncompressed image bytes in a sparse file; the mount handler picks the
kernel-side delivery mechanism based on host capability and runtime
configuration.  Missing chunks are filled by calling into the indexed content
store's `FillChunk` path (see [designs/indexed-content.md §Lazy Mode and Missing Chunks](indexed-content.md#lazy-mode-and-missing-chunks)),
which in turn drives the block provider (see [designs/block-provider.md](block-provider.md)).

### Cachefiles ondemand (primary for remote EROFS)

The kernel asks for missing ranges via `/dev/cachefiles` events; a
daemon decompresses the relevant zstd chunk(s) per the EROFS
image-format chunk index, fills the backing file, and
acknowledges the event. EROFS is mounted with
`fsid=<blob-digest>,tag=<bind>` so the kernel correlates events back
to the blob.

Requirements:
- Linux 5.19+ EROFS with fscache support.
- Kernel `cachefiles` driver loaded and `/dev/cachefiles` accessible.
- A paired userspace daemon — see [Cachefiles ondemand
  daemon](#cachefiles-ondemand-daemon) below.

The mount manager's transformer pipeline performs the cachefiles bind,
opens the daemon fd, and starts the daemon goroutine before issuing
the actual EROFS mount; if any prerequisite step fails, the mount
manager falls back to the loop/NBD path documented next.

### Loop / NBD (fallback)

For kernels without EROFS fscache support, non-EROFS blobs,
fully-local blobs where no lazy delivery is needed, or shims without
the indexed-content capability:

- Block delivered as a userspace-backed device (loop for fully-local;
  NBD for partially-local or remote).
- Bytes pulled from the sparse-file cache (`Handle.BackingFile()` via
  `cache.Attach`); the cache eagerly fills all missing chunks before
  the loop device is set up (see [cache.md §Loop adapter](cache.md#loop-adapter-fallback)).
- Optional dm-verity device wraps the block device; the kernel
  verifies reads against the supplied root hash.

Loop is the simplest case: when a blob is fully hydrated locally
(the eager-pull case), the cache file is loop-mounted directly with no
provider fetches required.  NBD is needed when delivery must serve
ranges from a remote provider but the host kernel lacks EROFS fscache
support.

### Diskless

Bypass the cache; expose `Provider.ReaderAt` directly via NBD or a
similar mechanism. For fast-network environments where local caching
is counterproductive (e.g., dedicated remote indexed content stores with their
own caching tier).

## Cachefiles ondemand daemon

The cachefiles delivery requires a userspace daemon that consumes
`OPEN` (announce a cookie's size), `READ` (populate a range and
acknowledge), and `CLOSE` (release per-cookie state) events on
`/dev/cachefiles`. The daemon's responsibilities, at the abstraction
level:

- Map cookie keys (the blob digest carried in `fsid=`) to indexed content store
  metadata.
- Translate kernel-side READ granules into indexed-content fetch units
  (zstd chunk size), rounding outward and clipping back.
- Fetch bytes via the indexed content store's `Provider` abstraction.
- Decompress zstd chunks using the chunk index.
- Bound in-flight provider concurrency and de-duplicate overlapping
  requests so the same chunk is never fetched twice.
- Convert errors (provider failure, decompression error, integrity
  failure) into a non-zero acknowledgement so the kernel returns
  `EIO` rather than silently stalling the reader.

The daemon has two homes depending on the call site. **This document
covers the mount-manager-owned variant**; the shim-hosted variant is
documented in the Lazy-load Shim Runtime design document.

### Mount-manager-owned daemon

Used for:
- Non-runtime callers: pre-pull, image transfer apply, operator tooling
  (`ctr indexed-content ...` and similar).
- Runtime mounts where the shim does not advertise the indexed-content
  capability (older shims, out-of-tree shims, or shims that opt out).

Properties:
- Lives inside the containerd daemon process.
- Opens a single `/dev/cachefiles` fd and binds one cache directory
  (per mount-manager instance), multiplexing cookies for any number of
  blobs across the host.
- Calls `Provider.ReadAt` directly, with full access to registry
  credentials, content-store reads, and other privileged provider
  state.
- Lifecycle is tied to containerd: daemon goroutine starts at
  mount-manager initialization, stops at shutdown, with mounts
  unwinding cleanly.
- I/O and buffer accounting goes to the containerd cgroup, **not** the
  container's. This is a known trade-off versus the shim-hosted path;
  the upside is that this path works with any shim, including
  out-of-tree ones.

The resulting EROFS mount is bind-mounted into the container by the
runtime via standard `bind` mount specs, so the container sees a
regular filesystem path even though the kernel's reads on it traverse
cachefiles → mount-manager daemon → provider.

### Wire-level details

Wire-level details (event structures, `CACHEFILES_IOC_*` ioctl names,
the `copen` reply format, `OPEN`/`READ`/`CLOSE` payload layouts) are
normative in the kernel's
`Documentation/filesystems/caching/cachefiles.rst` and are out of
scope for this design.

## Activation paths

The mount manager dispatches based on the runtime's reported capability,
which is supplied by shims that advertise support for the `block` mount
type at startup. The shim-side capability advertisement is described in
the Lazy-load Shim Runtime design document;
the mount-manager side does the dispatch:

```
                 ┌────────────────────────────────────────┐
                 │  Mount manager: Activate(mount stack)  │
                 └─────┬─────────────────────────────┬────┘
                       │                             │
        Shim has support │                             │ Shim does not
        capability     │                             │ (or no shim)
                       ▼                             ▼
        ┌────────────────────────┐    ┌────────────────────────────┐
        │ Pass mount stack       │    │ Mount manager performs the │
        │ through to shim        │    │ mount in containerd's NS   │
        │ unchanged              │    │ using its own daemon       │
        └────────────────────────┘    │                            │
                                      │ Bind-mount result into     │
                                      │ container as a `bind`      │
                                      │ entry                      │
                                      └────────────────────────────┘
```

For runtime activations where the shim handles the mount, see the
Lazy-load Shim Runtime design document for what happens on the shim side.

## Transformer pipeline

The mount manager already composes pre-mount transformers — `mkfs`
(create and format a sparse file), `mkdir` (create a directory),
`format/*` (compose mount stacks). For indexed-content mounts a small
number of new transformer steps are introduced:

- `format/cachefiles-bind` — given a cache directory path and a tag,
  perform the cachefiles `dir` / `tag` / `bind ondemand` sequence on
  `/dev/cachefiles`. The transformer's output is the bound directory
  path, which subsequent steps reference.
- `format/cachefiles-cookie` — given a blob digest and the bound tag,
  prepare an EROFS mount entry with `fsid=<digest>,tag=<bind>`.

These are tiny: each translates a known input into a single ioctl or
write, and they fit cleanly into the existing transformer/handler
shape used for `mkfs` and `mkdir` (`core/mount/manager/`). The block
mount handler is the thing that sequences them and ensures the daemon
goroutine is running before the EROFS mount is issued.

For the loop/NBD fallback, the existing `LoopbackHandler`
(`core/mount/loopback_handler_linux.go:29`) and `SetupLoop`
(`core/mount/losetup_linux.go:168`) machinery is reused unchanged,
combined with the existing dm-verity wrapping path (see
`plugins/mount/erofs/plugin_linux.go:165-206` for today's setup).

## Capability detection

The mount manager probes the host at startup:

- `/dev/cachefiles` exists and is openable.
- The `cachefiles` kernel module is loaded.
- The running kernel supports EROFS fscache (Linux 5.19+).

If any check fails, the mount manager advertises only the loop/NBD
delivery path; cachefiles ondemand mounts requested via `Mounts()` are
transparently rewritten to use loop/NBD with the cache's
`io.ReaderAt`. The indexed-content-aware logic does not need to know about
this fallback — the mount-handler-level switch is invisible to upstream
callers.

The kernel has been moving capabilities forward; the design assumes
the cachefiles path will be the mainline runtime path on hosts capable
of running it, with loop/NBD as the broad-compatibility fallback.

## Future Work (post v1)

- **fanotify pre-content delivery** — fanotify HSM / pre-content
  events (`FAN_CLASS_PRE_CONTENT`, Linux 6.13+ `FAN_PRE_ACCESS`) are
  the kernel-native counterpart to cachefiles ondemand for filesystems
  without fscache integration. Useful when extending indexed-content
  delivery to non-EROFS filesystems (a future item in the Indexed
  Content Service design document).
- **eBPF-augmented delivery** — kprobe / fentry programs can publish
  prefetch hints from cachefiles emit paths into a userspace ringbuf,
  gate access via LSM hooks, and produce per-cookie observability
  without daemon overhead. eBPF cannot serve reads end-to-end (no
  helpers for arbitrary file I/O or zstd decompression) but can reduce
  daemon wakeups and add policy.
- **NBD-side enhancements** — userfaultfd-style range-aware NBD or
  `io_uring`-driven NBD for tighter integration with the cache, for
  hosts that cannot use cachefiles.
