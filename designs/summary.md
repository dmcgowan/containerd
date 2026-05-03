# EROFS Image Layer Support — Milestone Summary

This document provides a high-level overview of the work required to
deliver EROFS-based container image distribution and runtime in
containerd, from the on-wire format specification through to a
lazy-loading shim implementation. It is intended as a planning
reference; each milestone has a companion design document linked above
or in progress.

## Summary

EROFS layers replace tar-based OCI layers with read-only EROFS
filesystem images that the kernel can mount directly from the layer
blob, without unpacking. The on-wire format preserves the OCI manifest,
image-index, descriptor, and image-configuration shape; only the layer
payload bytes, layer media types, and the interpretation of
`rootfs.diff_ids` change. Per-layer content-addressable metadata is
carried on layer descriptors via annotations: `org.erofs.uncompressed-digest`
(the digest of the decompressed image data — the layer's DiffID) and
`org.erofs.chunk-index.digest` (the digest of the embedded chunk-index
payload). The `rootfs.diff_ids` image-configuration field becomes optional
when `org.erofs.uncompressed-digest` is present on every compressed layer;
it is retained as a legacy fallback for consumers that do not yet recognize
the annotation.

The work spans six milestone areas. Several proceed in parallel: the
specification, the indexed content store, the initial snapshotter
improvements, and the image conversion tooling can all advance
concurrently. Later milestones compose these building blocks into
non-lazy and lazy mount paths, culminating in a full end-to-end
lazy-loading shim based on [nerdbox](https://github.com/containerd/nerdbox).

## Release targets

| Milestone | Target |
|---|---|
| Specification — draft approval | - |
| Snapshotter Part 1 | containerd 2.3 (backport) |
| Indexed content store | containerd 2.4 |
| Image conversion | containerd 2.4 |
| Snapshotter Part 2 | containerd 2.4 |
| Non-lazy-loading mount | containerd 2.4 |
| Lazy-loading mount | containerd 2.4 |
| Nerdbox shim (fscache) | [containerd/nerdbox](https://github.com/containerd/nerdbox), aligned with containerd 2.4 |

containerd 2.4 is targeted for August; development releases will ship
components as they become available.

**Note:** containerd 2.3 is the latest LTS release so will get format support backported, but no new features backported.

## Milestones

### 1. Specification — *Draft review*

The normative specification of the EROFS image layer format, currently 
at [erofs-image-spec](https://github.com/dmcgowan/erofs-image-spec).
It defines two media types (`application/vnd.erofs[+zstd]` for any valid
EROFS filesystem image, and `application/vnd.erofs.chunk-index.v1` for the
binary chunk index), the blob structure, the dm-verity merkle-tree layout,
the standard-form DiffID rules, the full descriptor annotation set
(`org.erofs.uncompressed-digest`, `org.erofs.chunk-index.*` including
`org.erofs.chunk-index.target`, `org.erofs.dmverity.*`, `org.erofs.role`),
the three composition roles (`overlay-lower`, `overlay-data`, `device`),
and the producer and consumer conformance requirements. `rootfs.diff_ids`
is optional when `org.erofs.uncompressed-digest` is present on every
compressed layer descriptor.

The specification is versioned independently of OCI or containerd. A draft
must be finalized before spec support can be backported to containerd 2.3
release and before we consider pushing images to public registries based
on this format. At a minimum, we expect spec review and tentative approval
from build team, trusted images team, runtime team as well as select
containerd and erofs maintainers. OCI approval is not a requirement but
maintainers of OCI are aware and free to comment.

**Status:** *Draft review.*

**Dependencies:**
- None.

**Unblocks:**
- All other milestones.

---

### 2. Indexed content store — *In development*

A new content-store service inside containerd that understands
EROFS-aware layer blobs. It ingests layers, parses their chunk indexes,
records per-chunk byte ranges and digests in a sidecar database, and
exposes per-chunk reads through a content-addressed `ReaderAt` API.
It integrates with containerd's GC and labelling infrastructure so that
layers and their individual chunks are reclaimed correctly. The indexed
content store is the foundation for chunk-level lazy loading and for
content-addressed block-store use cases.

See [indexed-content.md](indexed-content.md) for the full design.

**Status:** *In development.*

**Dependencies:**
- Specification (milestone 1).

**Unblocks:**
- Snapshotter Part 2, non-lazy mount, lazy-loading mount, nerdbox shim.

**Runs in parallel with:** Snapshotter Part 1, image conversion.

---

### 3a. Snapshotter Part 1 — *In progress*

Improvements to the in-tree EROFS snapshotter that do not depend on the
indexed content store, scoped to be backportable to containerd 2.3.
This part focuses on correctness and completeness of the EROFS mount path
as it exists today: materialising `device.<N>.raw` companion blobs for
layers carrying `org.erofs.role: device`, assembling multi-device EROFS
mounts for merged layers, and cleaning up the per-snapshot lifecycle
around data-device files. These changes make the non-lazy mount path
reliable for users who pull full layer blobs before container start.

The EROFS snapshotter (`plugins/snapshots/erofs/`), differ
(`plugins/diff/erofs/`), and dm-verity helper (`internal/erofsutils/`)
are in tree with device-role and raw-device handling tested under
`integration/erofs/`. Remaining scope for the 2.3 backport is wiring
the consumer obligations from spec §8.2: recognising
`org.erofs.uncompressed-digest`, falling back to `rootfs.diff_ids`, and
enforcing the §3.8 layer-ordering rules (device attaches to the first
subsequent non-device EROFS layer).

**Status:** *In progress.*

**Dependencies:**
- Specification (milestone 1).

**Unblocks:**
- Non-lazy-loading mount, Snapshotter Part 2.

**Release target:** containerd 2.3 (backport), 2.4.

**Runs in parallel with:** Indexed content store, image conversion.

---

### 3b. Snapshotter Part 2 — *In planning*

Integration of the EROFS snapshotter with the indexed content store.
This part replaces the legacy diff-and-snapshot dataflow with one driven
by the indexed content store, enabling chunk-level data delivery during
materialisation and forming the basis for the lazy-loading mount path.
It also surfaces per-layer `org.erofs.uncompressed-digest` and
`org.erofs.chunk-index.digest` annotation values during verification.

**Status:** *In planning.*

**Dependencies:**
- Indexed content store (milestone 2).
- Snapshotter Part 1 (milestone 3a).

**Unblocks:**
- Lazy-loading mount.

---

### 4. Image conversion — *Implemented*

Tooling and library support for converting existing OCI tar-based images
into EROFS layer images, producing all annotations (`org.erofs.uncompressed-digest`,
`org.erofs.chunk-index.*`, `org.erofs.dmverity.*`) and the optional
`rootfs.diff_ids` fallback the specification requires (spec §5.2, §8.1).
This milestone also defines the producer-side choices — chunk size,
chunking algorithm, deterministic image generation, and split-data layout
for cross-image deduplication — that future image builders and build-time
optimisers should follow. The conversion work is developed alongside the
indexed content store and Snapshotter Part 1 so that the format is
exercised end-to-end during development; it also serves as a reference for
tooling authors building EROFS-native producers outside of containerd.

The converter (`core/images/converter/erofs/`) supports raw, zstd,
chunked, split-data, dm-verity, and merge modes and is surfaced by
`ctr image convert --erofs[--merge|--dmverity|--replace] --parallelism`.
End-to-end tests live in `integration/erofs/convert_linux_test.go`.

**Status:** *Implemented.*

**Dependencies:**
- Specification (milestone 1).

**Unblocks:**
- Acts as a reference producer for all mount-path milestones; not a
  hard blocker but informs integration testing.

**Runs in parallel with:** Indexed content store, Snapshotter Part 1.

---

### 5. Non-lazy-loading mount in containerd — *In planning*

End-to-end support in containerd for pulling and mounting EROFS layers
before the container starts, without requiring lazy loading. The full
layer blob is fetched at pull time; the snapshotter materialises it on
disk; containerd issues a native EROFS mount with `loop` and any `device=`
options for data-device sources. dm-verity can optionally be configured
from descriptor annotations when integrity enforcement is required.
This milestone makes EROFS images usable in production for workloads that
prefer full prefetch over lazy loading.

**Status:** *In planning.*

**Dependencies:**
- Snapshotter Part 1 (milestone 3a).

**Unblocks:**
- Practical adoption of EROFS images without lazy loading.

---

### 6. Lazy-loading mount in containerd — *In planning*

Extends the non-lazy mount path to allow containers to start before all
layer bytes are locally available. Missing chunks are fetched on demand
by a userspace daemon as the kernel reads them, using the chunk-addressed
delivery protocol backed by the indexed content store. The daemon verifies
per-chunk checksums before delivering bytes to the kernel. The lazy-loading
state is surfaced through the snapshotter and task lifecycle so that the
rest of containerd is unaware of the deferral. This milestone defines the
protocol contract that the nerdbox shim exercises in milestone 7.

The milestone is composed of three sub-components, each with its own design
document:

- **Indexed content store — lazy mode** ([indexed-content.md §Lazy Mode and Missing Chunks](indexed-content.md#lazy-mode-and-missing-chunks)):
  Extends the eager-ingest store with lazy-ingest entry point, `MissingChunks`
  query, and `FillChunk` that drives per-chunk provider fetches with
  coalescing and two-level priority.

- **Block provider** ([block-provider.md](block-provider.md)):
  Chunk-level `Fetch(desc, chunk, priority)` interface; in-tree registry
  implementation using HTTP Range requests; per-host concurrency limits and
  foreground/background priority queues; plugin model for out-of-tree providers.
  Package: `core/content/index/provider/`.

- **Sparse-file cache** ([cache.md](cache.md)):
  Per-blob sparse file holding uncompressed image data for a running mount;
  chunk presence bitmap with sidecar persistence; fill path (content-store
  → decompress → pwrite); cachefiles ondemand adapter (primary) and loop
  adapter (fallback); GC-managed lifetime.
  Package: `core/content/index/cache/`.

See [lazy-load-mount-manager.md](lazy-load-mount-manager.md) for the
delivery-side mount manager design.

**Status:** *In planning.*

**Dependencies:**
- Indexed content store (milestone 2).
- Snapshotter Part 2 (milestone 3b).

**Unblocks:**
- Nerdbox shim (milestone 7).

---

### 7. Nerdbox shim with lazy loading via fscache — *In planning*

A containerd shim in [containerd/nerdbox](https://github.com/containerd/nerdbox)
that provides the first full end-to-end consumer of the EROFS lazy-loading
path. The shim uses fscache as the kernel-side delivery mechanism,
coordinating with the indexed content store to serve chunk fetches as the
kernel issues them. It is the milestone that demonstrates the complete
pipeline — registry pull, mount before all bytes are local, container
start, and kernel-driven chunk delivery — working together.

**This milestone is implemented in
[containerd/nerdbox](https://github.com/containerd/nerdbox), not in the
containerd repository.** It is listed here because its completion
represents full EROFS lazy-loading support for the project as a whole.

See [lazy-load-shim-runtime.md](lazy-load-shim-runtime.md) for the full
design.

**Status:** *In planning.*

**Dependencies:**
- Lazy-loading mount in containerd (milestone 6).

---

## Dependencies

- **Specification** has no dependencies and unblocks everything.
- **Indexed content store**, **Snapshotter Part 1**, and **image conversion** depend only on the specification and run in parallel with each other.
- **Snapshotter Part 2** depends on the indexed content store and Snapshotter Part 1.
- **Non-lazy-loading mount** depends on Snapshotter Part 1.
- **Lazy-loading mount** depends on the indexed content store and Snapshotter Part 2.
- **Nerdbox shim** depends on the lazy-loading mount.
