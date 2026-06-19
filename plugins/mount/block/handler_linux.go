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

// Package block provides a containerd mount handler for the "block" mount type.
//
// A "block" mount entry has:
//
//	type    = "block"
//	source  = "<local-sparse-file-path>"  // e.g. /var/lib/containerd/.../data
//	options = ["blockid=sha256:abc…",      // cache lookup key
//	           "fill=sparse",              // backing file may have holes (optional)
//	           "target=erofs",             // filesystem type (default: erofs)
//	           "ro"]
//
// The daemon-side handler ALWAYS fully populates the backing file (EnsureAll)
// before mounting.  On-demand fill via the BlockCache stream is handled
// exclusively by shims that advertise the "block" mount capability:
// they receive "fill=sparse" and use the BlockCache ttrpc service to fill
// holes on demand, which avoids keeping a supervisor goroutine in the
// daemon process.  This handler is used for non-shim consumers (ctr, tests)
// and as a fallback when the shim does not advertise "block" capability.
package block

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sys/unix"

	"github.com/containerd/containerd/v2/core/content/index/cache"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/core/content/index/provider"
	coremount 	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/internal/dmverity"
	"github.com/containerd/containerd/v2/internal/erofsmeta"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
)

// Handler implements coremount.Handler for the "block" mount type.
type Handler struct {
	store contentindex.Store
	cache cache.Cache

	mu          sync.Mutex
	handles     map[string]*activeBlock     // keyed by mountpoint
	supervisors map[string]*sharedSupervisor // keyed by blockID
	// prefilled tracks blockIDs whose EROFS superblock + metadata region
	// has already been EnsureRange'd in this process.  A single image
	// activation triggers two Mount() calls (spec-build + container-run),
	// and re-parsing the SB / re-issuing EnsureRange on the second one is
	// wasted work — the bitmap already records the chunks as present, but
	// the SB read from disk and metadata-range computation are not free.
	prefilled map[string]struct{}
}

type activeBlock struct {
	handle  cache.Handle
	blockID string

	// Loop-mount path (privileged, eager EnsureAll path).
	loopDev string

	// fileBacked is true when this mount used EROFS file-backed mounting
	// (lazy fanotify path).  No loop device is involved; Unmount only needs
	// to unmount the filesystem and decrement the supervisor refcount.
	fileBacked bool

	// usesSupervisor is true when this mount references a shared supervisor
	// in Handler.supervisors[blockID].  On Unmount, the refcount is dropped
	// and the supervisor is torn down when no other mounts reference it.
	usesSupervisor bool

	// dmVerityName, when non-empty, is the name of the /dev/mapper/<name>
	// dm-verity device created for this mount.  Unmount calls
	// dmverity.Close(name) after unix.Unmount.  The associated loop
	// device(s) are auto-cleared by the kernel when the verity device
	// is removed (see internal/dmverity.Open).
	dmVerityName string

	// FUSE-mount path (unprivileged).  Exactly one of loopDev, fuseCmd, or
	// fileBacked is set depending on which mount strategy succeeded.
	fuseCmd *exec.Cmd
	fuseMP  string // mountpoint owned by the FUSE process
}

// sharedSupervisor is one fanotify supervisor shared across all active mounts
// of the same backing blob.  The fanotify mark is installed once on the
// backing-file inode, kept active for the lifetime of the blob's first mount
// through the last mount's unmount.  This prevents the kernel from caching
// stale sparse-zero data during the brief window between Unmount() of one
// activation and Mount() of the next (e.g. spec-build vs container-run).
type sharedSupervisor struct {
	sup      *daemonSupervisor
	refCount int
}

// NewHandler returns a Handler that uses store to look up blobs and c as the
// sparse-file cache.
func NewHandler(store contentindex.Store, c cache.Cache) *Handler {
	return &Handler{
		store:       store,
		cache:       c,
		handles:     make(map[string]*activeBlock),
		supervisors: make(map[string]*sharedSupervisor),
		prefilled:   make(map[string]struct{}),
	}
}

// acquireSupervisor returns the shared supervisor for blockID, creating it
// if needed.  Increments the refcount.  The caller must call
// releaseSupervisor(blockID) when its mount is unmounted.
func (h *Handler) acquireSupervisor(ctx context.Context, mp, backingFile, blockID string, handle cache.Handle) (*daemonSupervisor, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ss, ok := h.supervisors[blockID]; ok {
		ss.refCount++
		log.G(ctx).WithFields(log.Fields{
			"blob":      blockID,
			"refcount":  ss.refCount,
		}).Info("[lazy-viz-debug] supervisor reuse")
		return ss.sup, nil
	}
	sup, err := newDaemonSupervisor(ctx, mp, backingFile, blockID, handle)
	if err != nil {
		return nil, err
	}
	h.supervisors[blockID] = &sharedSupervisor{sup: sup, refCount: 1}
	log.G(ctx).WithFields(log.Fields{
		"blob":     blockID,
		"refcount": 1,
	}).Info("[lazy-viz-debug] supervisor created")
	return sup, nil
}

// releaseSupervisor decrements the refcount for blockID's supervisor.
// When refcount hits zero, the supervisor is stopped and removed.
func (h *Handler) releaseSupervisor(blockID string) {
	h.mu.Lock()
	ss, ok := h.supervisors[blockID]
	if !ok {
		h.mu.Unlock()
		return
	}
	ss.refCount--
	if ss.refCount > 0 {
		log.G(context.Background()).WithFields(log.Fields{
			"blob":     blockID,
			"refcount": ss.refCount,
		}).Info("[lazy-viz-debug] supervisor refcount-- (still alive)")
		h.mu.Unlock()
		return
	}
	delete(h.supervisors, blockID)
	// Drop the pre-fill memo too: a future Mount() may run against a
	// freshly-attached cache (e.g. blob removed and re-pulled while the
	// process keeps running) whose bitmap no longer has the metadata
	// chunks set.  Clearing the flag forces the next Mount() to re-run
	// EnsureRange for the SB + metadata region, which is cheap when
	// chunks are already present.
	delete(h.prefilled, blockID)
	h.mu.Unlock()
	log.G(context.Background()).WithField("blob", blockID).
		Info("[lazy-viz-debug] supervisor refcount=0, stopping")
	ss.sup.stop()
}

// runHSMSelfTest verifies that EROFS's filp_open of the backing file (at
// mount time) successfully latched FMODE_FSNOTIFY_HSM into its struct file.
// If so, runtime fileio reads will fire FAN_PRE_ACCESS for unfilled folios.
//
// Mechanism: walk the EROFS mount root and read a few bytes from the first
// regular file we find.  Reading a regular file's content goes through
// erofs_fileio_read_folio → vfs_iocb_iter_read(backing_file) →
// rw_verify_area → fsnotify_pre_content — the hook path.  Directory dirent
// reads go through the metabuf path (no hook), so they are NOT a valid
// self-test; we explicitly skip directory reads.
//
// If a file read fires an event, FMODE_FSNOTIFY_HSM is latched and lazy
// loading will work.  If no event fires (despite the file being read), the
// kernel is not engaging the hook — lazy fills will fail silently.
//
// This is a diagnostic; we do NOT fail Mount() on a missed event, because:
//   - the file may be small enough to be entirely inlined in the inode
//     (tail-packed), in which case the read is served entirely from the
//     metabuf path and no event fires,
//   - we may not find a regular file in the first few dirents we list.
// A WARN log flags a real HSM failure in the field; absence of WARN does
// not by itself prove failure (only the converse — the presence of an
// event proves HSM works).
func (h *Handler) runHSMSelfTest(ctx context.Context, blockID, mp string, handle cache.Handle) {
	h.mu.Lock()
	ss, ok := h.supervisors[blockID]
	h.mu.Unlock()
	if !ok || ss == nil || ss.sup == nil {
		return
	}
	before := ss.sup.eventsReceived.Load()

	// Walk the EROFS mount root for a regular file to read.
	root, err := os.Open(mp)
	if err != nil {
		log.G(ctx).WithError(err).WithField("block", blockID).
			Warn("[lazy-viz-debug] HSM self-test: open(mp) failed")
		return
	}
	names, _ := root.Readdirnames(0)
	root.Close()

	var probedPath string
	for _, n := range names {
		p := mp + "/" + n
		st, err := os.Stat(p)
		if err == nil && st.Mode().IsRegular() && st.Size() > 4096 {
			probedPath = p
			break
		}
		// If not a regular file, look one level deeper (e.g. /bin/*).
		if err == nil && st.IsDir() {
			if sub, err := os.Open(p); err == nil {
				subNames, _ := sub.Readdirnames(20)
				sub.Close()
				for _, sn := range subNames {
					sp := p + "/" + sn
					if sst, err := os.Stat(sp); err == nil && sst.Mode().IsRegular() && sst.Size() > 4096 {
						probedPath = sp
						break
					}
				}
				if probedPath != "" {
					break
				}
			}
		}
	}

	if probedPath == "" {
		log.G(ctx).WithField("block", blockID).
			Info("[lazy-viz-debug] HSM self-test: no regular file found to probe")
		return
	}

	pf, err := os.Open(probedPath)
	if err != nil {
		log.G(ctx).WithError(err).WithField("block", blockID).
			Warn("[lazy-viz-debug] HSM self-test: open(probe) failed")
		return
	}
	buf := make([]byte, 4096)
	_, _ = pf.Read(buf) // triggers erofs_fileio_read_folio → fsnotify hook
	pf.Close()

	// Give the supervisor up to ~50 ms to process the event.
	after := ss.sup.eventsReceived.Load()
	for i := 0; i < 10 && after == before; i++ {
		time.Sleep(5 * time.Millisecond)
		after = ss.sup.eventsReceived.Load()
	}

	if after > before {
		log.G(ctx).WithFields(log.Fields{
			"block":         blockID,
			"probe":         probedPath,
			"events_before": before,
			"events_after":  after,
		}).Info("[lazy-viz-debug] HSM self-test PASSED — FMODE_FSNOTIFY_HSM is latched")
	} else {
		log.G(ctx).WithFields(log.Fields{
			"block":         blockID,
			"probe":         probedPath,
			"events_before": before,
			"events_after":  after,
		}).Warn("[lazy-viz-debug] HSM self-test: no event fired after reading a regular file — FMODE_FSNOTIFY_HSM may NOT be latched (or file was tail-packed inline)")
	}
}

// Mount activates a "block" mount entry.
//
// When the kernel supports FAN_CLASS_PRE_CONTENT (Linux ≥ 6.13) and not all
// chunks are present, Mount uses the lazy path: it fills only chunk 0 (which
// contains the EROFS superblock), mounts the loop device immediately, and
// installs a fanotify supervisor that fills remaining chunks on demand as the
// container accesses files.  This lets containers start without waiting for a
// full image download.
//
// On older kernels, or when all chunks are already resident, Mount falls back
// to an eager EnsureAll before mounting.
func (h *Handler) Mount(ctx context.Context, m coremount.Mount, mp string, _ []coremount.ActiveMount) (coremount.ActiveMount, error) {
	if m.Type != "block" {
		return coremount.ActiveMount{}, errdefs.ErrNotImplemented
	}

	// Parse options: target, blockid, fill (informational for daemon), ro,
	// and the dmverity-* trio (root_hash, hash_offset, block_size) that
	// triggers the verity branch.  Anything else passes through as the
	// fs data string.
	//
	// "ro" is a generic VFS flag (we set MS_RDONLY below) and is NOT a
	// valid EROFS-specific data option; passing it as fs data makes the
	// EROFS file-backed mount path reject with EINVAL.  Filter it out —
	// MS_RDONLY covers it.
	target := "erofs"
	blockID := ""
	var verityRoot string
	var verityHashOffset uint64
	var verityBlockSize uint32
	var extraOpts []string
	for _, opt := range m.Options {
		if v, ok := strings.CutPrefix(opt, "target="); ok {
			target = v
			continue
		}
		if v, ok := strings.CutPrefix(opt, "blockid="); ok {
			blockID = v
			continue
		}
		if strings.HasPrefix(opt, "fill=") {
			// fill= is advisory (shim-side); daemon chooses its own strategy.
			continue
		}
		if v, ok := strings.CutPrefix(opt, "dmverity-roothash="); ok {
			verityRoot = v
			continue
		}
		if v, ok := strings.CutPrefix(opt, "dmverity-hashoffset="); ok {
			n, perr := strconv.ParseUint(v, 10, 64)
			if perr != nil {
				return coremount.ActiveMount{}, fmt.Errorf("block: parse dmverity-hashoffset=%q: %w", v, perr)
			}
			verityHashOffset = n
			continue
		}
		if v, ok := strings.CutPrefix(opt, "dmverity-blocksize="); ok {
			n, perr := strconv.ParseUint(v, 10, 32)
			if perr != nil {
				return coremount.ActiveMount{}, fmt.Errorf("block: parse dmverity-blocksize=%q: %w", v, perr)
			}
			verityBlockSize = uint32(n)
			continue
		}
		if opt == "ro" || opt == "rw" {
			// Generic VFS flags — set via mount(2) flags arg, not data string.
			continue
		}
		extraOpts = append(extraOpts, opt)
	}

	if blockID == "" {
		return coremount.ActiveMount{}, fmt.Errorf("block: blockid= option required")
	}

	// Verity hard-fail policy (Q4 secure default): when verity params are
	// present, they MUST be honoured.  Any short-circuit to an unverified
	// mount would silently downgrade security; we surface the failure to
	// the caller (snapshotter → mount manager → user) instead.  An
	// inconsistent verity option set (e.g. roothash without hashoffset)
	// is also rejected here, before any cache/handle resources are
	// allocated.
	verityRequested := verityRoot != ""
	if verityRequested && verityHashOffset == 0 {
		return coremount.ActiveMount{}, fmt.Errorf("block: dmverity-roothash given without dmverity-hashoffset (blockid=%s)", blockID)
	}
	if verityRequested {
		if supported, serr := dmverity.IsSupported(); !supported {
			if serr == nil {
				serr = fmt.Errorf("dm_verity kernel module not loaded")
			}
			return coremount.ActiveMount{}, fmt.Errorf("block: dmverity requested but unavailable: %w (blockid=%s)", serr, blockID)
		}
	}

	dgst, err := digest.Parse(blockID)
	if err != nil {
		return coremount.ActiveMount{}, fmt.Errorf("block: parse blockid %q: %w", blockID, err)
	}

	info, err := h.store.Info(ctx, dgst)
	if err != nil {
		return coremount.ActiveMount{}, fmt.Errorf("block: get blob info for %s: %w", blockID, err)
	}
	desc := ocispec.Descriptor{
		MediaType: info.MediaType,
		Digest:    info.Digest,
		Size:      info.Size,
	}

	// Get the provider for this blob from the global registry.
	p, err := provider.Global.Get(info.Provider)
	if err != nil {
		return coremount.ActiveMount{}, fmt.Errorf("block: get provider %q for block %s: %w",
			info.Provider, blockID, err)
	}

	// Attach to the cache.  The cache decides where to materialize the
	// backing file.
	handle, err := h.cache.Attach(ctx, desc, p)
	if err != nil {
		return coremount.ActiveMount{}, fmt.Errorf("block: cache attach %s: %w", blockID, err)
	}
	backingFile := handle.BackingFile()

	// ── Lazy path: pre-fill SB + metadata, file-backed mount, then fanotify ─
	//
	// This kernel supports CONFIG_EROFS_FS_BACKED_BY_FILE: EROFS can mount a
	// regular file directly via VFS (no loop device).  At runtime, container
	// reads of unfilled chunks fire FAN_PRE_ACCESS on the backing-file inode,
	// which the supervisor responds to by filling the chunk on demand.
	//
	// IMPORTANT: mount-time reads (superblock at offset 1024, root inode at
	// meta_blkaddr) DO NOT fire FAN_PRE_ACCESS — the kernel's mount-time SB
	// fetch bypasses the filemap_read_folio path that hosts the hook.  We
	// must therefore PRE-FILL the SB chunk (chunk 0) and the metadata region
	// before calling mount(2).  Only file-data reads from the running
	// container (a separate process) fire FAN_PRE_ACCESS and get filled
	// lazily by the supervisor.
	//
	// Container start cost: 1 chunk (~12 MiB, SB+inline metadata of small
	// files like /bin/sh) + metadata region (~5–20 MiB depending on inode
	// count) — typically 15–35 MiB out of multi-GiB images.
	// ── Lazy path: fanotify supervisor + file-backed EROFS mount ────────
	//
	// CRITICAL ORDERING (verified against kernel v6.14 fsnotify code):
	//
	// `FMODE_FSNOTIFY_HSM` is set on a `struct file` exactly once, by
	// `file_set_fsnotify_mode_from_watchers()` at the time the file is
	// opened.  Later, `fsnotify_file_area_perm()` (called from
	// `rw_verify_area`) only calls `fsnotify_pre_content()` when this flag
	// is set.  Kernel comment:
	//   "fsnotify permission hooks do not check if there are permission
	//    event watches, but that there were permission event watches AT
	//    OPEN TIME."
	//
	// EROFS opens the backing file (`sbi->dif0.file`) during `mount(2)`
	// via `filp_open`.  Therefore the fanotify mark on the backing file's
	// inode MUST exist BEFORE `unix.Mount` runs, so the resulting struct
	// file has `FMODE_FSNOTIFY_HSM` latched.  If we install the mark after
	// mount, the flag is never set and no runtime read ever fires a
	// pre-content event — even though the mark is "active" on the inode.
	//
	// Two read paths through EROFS:
	//   1. metabuf reads (SB at offset 1024, all inodes at meta_blkaddr):
	//      go through `read_mapping_folio(btrfs_mapping)` — BYPASS the
	//      `rw_verify_area` hook entirely.  These cannot be served by
	//      fanotify on this kernel; we MUST pre-fill them.
	//   2. fileio reads (file content + directory dirents): go through
	//      `erofs_fileio_rq_submit` → `vfs_iocb_iter_read(backing_file)` →
	//      `rw_verify_area` → fires FAN_PRE_ACCESS (with HSM mode set).
	//
	// Sequence:
	//   1. Pre-fill chunk 0 (SB at offset 1024).
	//   2. Pre-fill the metadata region (all inodes/xattrs/dir-tail).
	//   3. Drop the page cache for the backing file (kill any stale zero
	//      folios cached by failed earlier attempts).
	//   4. Acquire the supervisor (installs fanotify mark on backing inode).
	//   5. unix.Mount (EROFS filp_opens the backing file → HSM mode latched).
	//   6. Runtime reads now fire FAN_PRE_ACCESS; supervisor fills on demand.
	//
	// If any fanotify setup step fails (no kernel support, mark fails,
	// mount fails), release the supervisor and fall through to EnsureAll.
	if isFanotifyPreContentSupported() && !handle.AllPresent() {
		log.G(ctx).WithField("block", blockID).Info("[lazy-viz] block_mount_lazy_start")

		// Steps 1–2 (SB + metadata pre-fill) are run once per blockID per
		// process lifetime.  A single image activation triggers two Mount()
		// calls (spec-build via withReadonlyFS, then container-run); the
		// second activation's pre-fill is purely redundant since the
		// bitmap is already marked.  Skipping the re-parse + EnsureRange
		// also avoids an unnecessary SB read from disk.
		h.mu.Lock()
		_, alreadyPrefilled := h.prefilled[blockID]
		h.mu.Unlock()

		if !alreadyPrefilled {
			// Step 1: pre-fill chunk 0 (contains EROFS superblock at offset 1024).
			// Mount-time SB read uses the metabuf path which bypasses fsnotify.
			if err := handle.EnsureRange(ctx, 0, 1); err != nil {
				_ = handle.Release()
				return coremount.ActiveMount{}, fmt.Errorf("block: ensure SB chunk for %s: %w", blockID, err)
			}

			// Step 2: parse the SB to find meta_blkaddr (inode-table start),
			// then pre-fill the entire metadata region.  EROFS metabuf reads
			// (root inode at mount, every other inode at runtime lookup) go
			// through the btrfs mapping directly and do NOT fire FAN_PRE_ACCESS.
			// All inodes must be resident on disk before mount.
			metaOff, metaLen := erofsMetadataRange(backingFile)
			log.G(ctx).WithFields(log.Fields{
				"block":    blockID,
				"meta_off": metaOff,
				"meta_len": metaLen,
			}).Info("[lazy-viz] block_erofs_meta_range")
			if metaOff > 0 {
				if err := handle.EnsureRange(ctx, metaOff, metaLen); err != nil {
					if verityRequested {
						_ = handle.Release()
						return coremount.ActiveMount{}, fmt.Errorf("block: verity-mode metadata pre-fill failed for %s: %w", blockID, err)
					}
					log.G(ctx).WithError(err).WithFields(log.Fields{
						"block":    blockID,
						"meta_off": metaOff,
					}).Warn("block: lazy metadata EnsureRange failed — will fall back to EnsureAll")
					goto eagerFallback
				}
			}

			// Step 2b (verity only): pre-fill the dm-verity superblock +
			// merkle tree region [hashOffset, fileEnd).  dm-verity reads
			// the superblock during Open() and the interior tree nodes
			// on every block verification; both reads come from the
			// kernel's verity machinery and may bypass the fanotify
			// filemap hook the way EROFS metabuf reads do.  Holes here
			// would mean the verity device hashes zeros and rejects
			// blocks with permanent EIO — far worse than the silent
			// wrong-data outcome the plain lazy path tolerates.
			// (Q2: pre-fill the dmverity metadata whenever provided.)
			if verityRequested {
				fi, statErr := os.Stat(backingFile)
				if statErr != nil {
					_ = handle.Release()
					return coremount.ActiveMount{}, fmt.Errorf("block: verity-mode stat backing file %s: %w", backingFile, statErr)
				}
				fileEnd := fi.Size()
				if fileEnd <= 0 {
					_ = handle.Release()
					return coremount.ActiveMount{}, fmt.Errorf("block: verity-mode: backing file is empty for %s", blockID)
				}
				if int64(verityHashOffset) >= fileEnd {
					_ = handle.Release()
					return coremount.ActiveMount{}, fmt.Errorf("block: verity-mode: hashOffset %d ≥ file size %d for %s",
						verityHashOffset, fileEnd, blockID)
				}
				treeLen := fileEnd - int64(verityHashOffset)
				log.G(ctx).WithFields(log.Fields{
					"block":       blockID,
					"hash_offset": verityHashOffset,
					"tree_len":    treeLen,
				}).Info("[lazy-viz] block_dmverity_tree_prefill_start")
				if err := handle.EnsureRange(ctx, int64(verityHashOffset), treeLen); err != nil {
					_ = handle.Release()
					return coremount.ActiveMount{}, fmt.Errorf("block: verity-mode merkle-tree pre-fill failed for %s: %w", blockID, err)
				}
				log.G(ctx).WithFields(log.Fields{
					"block":    blockID,
					"tree_len": treeLen,
				}).Info("[lazy-viz] block_dmverity_tree_prefill_done")
			}

			h.mu.Lock()
			h.prefilled[blockID] = struct{}{}
			h.mu.Unlock()
		} else {
			log.G(ctx).WithField("block", blockID).
				Info("[lazy-viz-debug] SB+metadata pre-fill skipped (already prefilled this process)")
		}

		// Step 3: drop page cache for the backing file.  Earlier mount
		// attempts (or other code paths) may have read sparse-hole regions
		// and populated the page cache with zero pages.  If we leave those
		// stale zero pages in the cache, EROFS reads from the kernel mount
		// hit them and return invalid data WITHOUT firing FAN_PRE_ACCESS
		// (no fault = no hook).  POSIX_FADV_DONTNEED drops the file's pages
		// from the page cache; the next read must fault them in fresh.
		if df, err := os.Open(backingFile); err == nil {
			_ = unix.Fadvise(int(df.Fd()), 0, 0, unix.FADV_DONTNEED)
			df.Close()
			log.G(ctx).WithField("block", blockID).
				Info("[lazy-viz-debug] dropped page cache for backing file")
		}

		if err := os.MkdirAll(mp, 0755); err != nil {
			log.G(ctx).WithError(err).WithField("block", blockID).
				Warn("block: lazy mkdirAll failed — falling back to EnsureAll")
			goto eagerFallback
		}

		// Step 4: install fanotify mark BEFORE unix.Mount.  EROFS's
		// filp_open of the backing file during mount latches
		// FMODE_FSNOTIFY_HSM into the resulting struct file, which is
		// the only way runtime reads will fire FAN_PRE_ACCESS.
		//
		// For the verity branch the mark is STILL on the backing-file
		// inode; the loop driver does buffered reads (Direct: false in
		// internal/dmverity.Open) so dm-verity-originated block reads
		// descend through the host filesystem's page cache and fire
		// the same fanotify hook.
		if _, supErr := h.acquireSupervisor(ctx, mp, backingFile, blockID, handle); supErr != nil {
			if verityRequested {
				_ = handle.Release()
				return coremount.ActiveMount{}, fmt.Errorf("block: verity-mode fanotify supervisor install failed for %s: %w", blockID, supErr)
			}
			log.G(ctx).WithError(supErr).WithField("block", blockID).
				Warn("block: fanotify supervisor install failed — falling back to EnsureAll")
			goto eagerFallback
		}

		// Step 5: mount.  Two strategies depending on whether dm-verity
		// is requested:
		//
		//   • non-verity (default): file-backed EROFS mount.  The
		//     kernel's get_tree_bdev() returns -ENOTBLK for a regular
		//     file; EROFS falls back to filp_open(backingFile) — at
		//     which point file_set_fsnotify_mode_from_watchers sees
		//     our mark and latches FMODE_FSNOTIFY_HSM on the struct
		//     file.
		//
		//   • verity-on: dm-verity REQUIRES a block device beneath
		//     it, so we build  backing-file → loop → dm-verity →
		//     /dev/mapper/<name>  and mount EROFS on top.  The
		//     fanotify mark on the backing-file inode still fires
		//     because the loop driver (Readonly + non-O_DIRECT)
		//     reads via the host fs's filemap_read_folio path that
		//     hosts the hook.  Verity hard-fails on any setup
		//     failure (Q4).
		mountOpts := strings.Join(extraOpts, ",")
		var verityName string
		var verityDevice string
		mountSource := backingFile
		if verityRequested {
			verityName = computeVerityName(mp, blockID)
			dev, verr := dmverity.Open(backingFile, verityName, backingFile, verityRoot, verityHashOffset,
				&dmverity.DmverityOptions{
					DataBlockSize: effectiveBlockSize(verityBlockSize),
					HashBlockSize: effectiveBlockSize(verityBlockSize),
					HashOffset:    verityHashOffset,
				})
			if verr != nil {
				h.releaseSupervisor(blockID)
				return coremount.ActiveMount{}, fmt.Errorf("block: verity-mode dmverity.Open failed for %s: %w", blockID, verr)
			}
			verityDevice = dev
			mountSource = dev
			log.G(ctx).WithFields(log.Fields{
				"block":         blockID,
				"verity_name":   verityName,
				"verity_device": dev,
			}).Info("[lazy-viz] block_dmverity_open")
		}

		mountErr := unix.Mount(mountSource, mp, target, unix.MS_RDONLY, mountOpts)
		if mountErr != nil {
			if verityRequested {
				_ = dmverity.Close(verityName)
				h.releaseSupervisor(blockID)
				return coremount.ActiveMount{}, fmt.Errorf("block: verity-mode mount failed for %s: %w (source=%s, fstype=%s, opts=%s)",
					blockID, mountErr, mountSource, target, mountOpts)
			}
			log.G(ctx).WithError(mountErr).WithFields(log.Fields{
				"block":  blockID,
				"source": backingFile,
				"mp":     mp,
				"fstype": target,
				"opts":   mountOpts,
			}).Warn("block: file-backed EROFS mount failed — releasing supervisor, falling back to EnsureAll")
			h.releaseSupervisor(blockID)
			goto eagerFallback
		}

		// Step 6: post-mount self-test.  Read a known-unfilled chunk's
		// byte range from inside the EROFS mount; this should trigger a
		// FAN_PRE_ACCESS event handled by the supervisor.  If the event
		// counter goes up, FMODE_FSNOTIFY_HSM was successfully latched.
		// If not, the kernel is silently skipping the hook and lazy
		// loading won't work — abort and fall back to EnsureAll.
		h.runHSMSelfTest(ctx, blockID, mp, handle)

		log.G(ctx).WithFields(log.Fields{
			"block":  blockID,
			"mp":     mp,
			"source": mountSource,
			"verity": verityRequested,
		}).Info("[lazy-viz] block_mounted_lazy")

		mountMode := "file-backed"
		if verityRequested {
			mountMode = "verity"
		}
		h.mu.Lock()
		h.handles[mp] = &activeBlock{
			handle:         handle,
			blockID:        blockID,
			usesSupervisor: true,
			fileBacked:     !verityRequested,
			dmVerityName:   verityName,
		}
		h.mu.Unlock()

		mountedAt := time.Now().UTC()
		mountData := map[string]string{
			"block.id":   blockID,
			"block.fill": "lazy",
			"block.mode": mountMode,
		}
		if verityRequested {
			mountData["block.dmverity"] = verityDevice
		}
		return coremount.ActiveMount{
			Mount:      m,
			MountedAt:  &mountedAt,
			MountPoint: mp,
			MountData:  mountData,
		}, nil
	}
eagerFallback:

	// ── Eager path: EnsureAll before mount ───────────────────────────────
	//
	// Used when FAN_CLASS_PRE_CONTENT is unavailable (Linux < 6.13), when all
	// chunks are already resident (no fanotify needed), or when the lazy path
	// above failed.
	log.G(ctx).WithField("block", blockID).Info("block: filling all chunks before mount (lazy-load)")
	if err := handle.EnsureAll(ctx); err != nil {
		_ = handle.Release()
		return coremount.ActiveMount{}, fmt.Errorf("block: ensure all chunks for %s: %w", blockID, err)
	}
	log.G(ctx).WithField("block", blockID).Info("block: all chunks filled, mounting")

	// Flush all dirty pages before the mount so any loop device or FUSE
	// reader sees the fully-written file.
	unix.Sync()

	// ── Privileged path: loop device + kernel erofs mount ────────────────
	//
	// Attempt to set up a loop device.  If this fails with a permissions
	// error (no CAP_SYS_ADMIN), fall through to the unprivileged FUSE path.
	loopFile, loopErr := coremount.SetupLoop(backingFile, coremount.LoopParams{
		Readonly:  true,
		Autoclear: true,
		Direct:    false,
	})
	if loopErr == nil {
		loopDev := loopFile.Name()
		defer loopFile.Close()

		// Flush cached blocks on the loop device before mounting.
		const BLKFLSBUF = 0x00001261
		_, _, _ = unix.Syscall(unix.SYS_IOCTL, loopFile.Fd(), BLKFLSBUF, 0)

		if err := os.MkdirAll(mp, 0755); err != nil {
			_ = coremount.DetachLoopDevice(loopDev)
			_ = handle.Release()
			return coremount.ActiveMount{}, fmt.Errorf("block: create mountpoint %s: %w", mp, err)
		}

		flags := uintptr(unix.MS_RDONLY)
		opts := strings.Join(extraOpts, ",")
		if err := unix.Mount(loopDev, mp, target, flags, opts); err != nil {
			_ = coremount.DetachLoopDevice(loopDev)
			_ = handle.Release()
			return coremount.ActiveMount{}, fmt.Errorf("block: mount %s (%s) at %s: %w",
				loopDev, target, mp, err)
		}

		// Disable autoclear so the device survives until Unmount.
		if err := coremount.SetLoopAutoclear(loopFile, false); err != nil {
			_ = unix.Unmount(mp, unix.MNT_DETACH)
			_ = coremount.DetachLoopDevice(loopDev)
			_ = handle.Release()
			return coremount.ActiveMount{}, fmt.Errorf("block: clear autoclear on %s: %w",
				loopDev, err)
		}

		h.mu.Lock()
		h.handles[mp] = &activeBlock{
			handle:  handle,
			loopDev: loopDev,
			blockID: blockID,
		}
		h.mu.Unlock()

		mountedAt := time.Now().UTC()
		return coremount.ActiveMount{
			Mount:      m,
			MountedAt:  &mountedAt,
			MountPoint: mp,
			MountData: map[string]string{
				"block.id":      blockID,
				"block.loopdev": loopDev,
			},
		}, nil
	}

	// ── Unprivileged path: erofs-fuse ────────────────────────────────────
	//
	// Loop setup is not available (no CAP_SYS_ADMIN).  Mount the EROFS
	// image via erofs-fuse which uses /dev/fuse and does not require any
	// privileged kernel interface.
	log.G(ctx).WithField("block", blockID).
		WithError(loopErr).
		Debug("block: loop device unavailable, falling back to erofs-fuse")

	fuseCmd, err := mountErofsFuse(backingFile, mp)
	if err != nil {
		_ = handle.Release()
		return coremount.ActiveMount{}, fmt.Errorf("block: erofs-fuse mount for %s: %w (loop error: %v)",
			blockID, err, loopErr)
	}

	h.mu.Lock()
	h.handles[mp] = &activeBlock{
		handle:  handle,
		blockID: blockID,
		fuseCmd: fuseCmd,
		fuseMP:  mp,
	}
	h.mu.Unlock()

	mountedAt := time.Now().UTC()
	return coremount.ActiveMount{
		Mount:      m,
		MountedAt:  &mountedAt,
		MountPoint: mp,
		MountData: map[string]string{
			"block.id":   blockID,
			"block.fuse": "erofs-fuse",
		},
	}, nil
}

// Unmount deactivates a "block" mount, using the same strategy (loop or FUSE)
// that was used during Mount.
func (h *Handler) Unmount(_ context.Context, mp string) error {
	h.mu.Lock()
	ab, ok := h.handles[mp]
	if ok {
		delete(h.handles, mp)
	}
	h.mu.Unlock()

	var errs []string

	// Unmount the filesystem FIRST.  After unmount, the kernel will no
	// longer route reads through the EROFS mount, so no new FAN_PRE_ACCESS
	// events are generated.  We then release the supervisor (which decrements
	// the per-blob refcount and stops the supervisor only when no other mount
	// of the same blob is active).
	switch {
	case ab != nil && ab.fuseCmd != nil:
		// FUSE path: use fusermount to detach, then wait for the process.
		if err := unmountFuse(ab.fuseMP, ab.fuseCmd); err != nil {
			errs = append(errs, fmt.Sprintf("fuse unmount %s: %v", mp, err))
		}
	case ab != nil && ab.dmVerityName != "":
		// Verity path: kernel unmount, then close the dm-verity device.
		// dmverity.Close removes the /dev/mapper entry and the loop
		// devices it referenced auto-detach (Autoclear was set during
		// SetupLoop in internal/dmverity.Open).  We do NOT touch loopDev
		// directly here — that field is empty in the verity case.
		if err := unix.Unmount(mp, unix.MNT_DETACH); err != nil {
			errs = append(errs, fmt.Sprintf("unmount %s: %v", mp, err))
		}
		if err := dmverity.Close(ab.dmVerityName); err != nil {
			errs = append(errs, fmt.Sprintf("dmverity close %s: %v", ab.dmVerityName, err))
		}
	case ab != nil && ab.fileBacked:
		// File-backed EROFS path: no loop device, just unmount the filesystem.
		if err := unix.Unmount(mp, unix.MNT_DETACH); err != nil {
			errs = append(errs, fmt.Sprintf("unmount %s: %v", mp, err))
		}
	default:
		// Loop path: kernel unmount then detach the loop device.
		if err := unix.Unmount(mp, unix.MNT_DETACH); err != nil {
			errs = append(errs, fmt.Sprintf("unmount %s: %v", mp, err))
		}
		if ab != nil && ab.loopDev != "" {
			if err := coremount.DetachLoopDevice(ab.loopDev); err != nil {
				errs = append(errs, fmt.Sprintf("detach loop %s: %v", ab.loopDev, err))
			}
		}
	}

	// Release the shared supervisor (refcount--; stops when last user releases).
	if ab != nil && ab.usesSupervisor {
		h.releaseSupervisor(ab.blockID)
	}

	if ab != nil {
		if err := ab.handle.Release(); err != nil {
			errs = append(errs, fmt.Sprintf("cache release: %v", err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("block unmount errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

// Compile-time check that Handler implements coremount.Handler.
var _ coremount.Handler = (*Handler)(nil)

// effectiveBlockSize returns the dm-verity block size requested by
// the caller, or DefaultBlockSize when zero.  Mirrors the
// dmverity.DmverityMetadata.EffectiveBlockSize convention so the
// option-string-only path here doesn't have to import the metadata
// type just to apply the default.
func effectiveBlockSize(requested uint32) uint32 {
	if requested == 0 {
		return dmverity.DefaultBlockSize
	}
	return requested
}

// computeVerityName produces a stable, dm-name-legal identifier for
// the dm-verity device backing a single mountpoint.  Stability across
// daemon restarts is desirable so a previously-mounted-but-orphaned
// /dev/mapper entry can be re-discovered and torn down; we derive
// the name from a fingerprint of (mp, blockID) so two simultaneous
// mounts of distinct mountpoints get distinct names.
//
// Format: "containerd-block-<12-hex-chars>".  The dm-mapper accepts
// arbitrary printable ASCII but a leading "containerd-" prefix lets
// operators grep /proc/mounts and `dmsetup ls` for our devices.
func computeVerityName(mp, blockID string) string {
	h := sha256.Sum256([]byte(mp + "\x00" + blockID))
	return fmt.Sprintf("containerd-block-%x", h[:6])
}

// erofsMetadataRange reads the EROFS superblock from backingFile and returns
// the byte offset and length of the inode-table (metadata) region.  This is
// a thin file-path adapter over erofsmeta.MetadataRange, which is also used
// (with a cache.Handle as the io.ReaderAt) by the lazy pull path so that
// client-side fsview reads of /etc/passwd find resident pages without
// triggering a kernel mount.  Returns (0, 0) on any parse error.
func erofsMetadataRange(backingFile string) (off, length int64) {
	f, err := os.Open(backingFile)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	off, length, _ = erofsmeta.MetadataRange(f)
	return off, length
}
