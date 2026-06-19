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

//go:build linux

// Package blockmount implements the shim-side handler for "block" mount types.
//
// # Motivation
//
// The fanotify supervisor goroutine (which intercepts read-before-serve events
// and fills sparse backing files on demand) must live in the shim process, not
// the containerd daemon.  If the supervisor lived in the daemon and the daemon
// restarted, its fanotify fd would close, the kernel mark would be dropped,
// and subsequent reads to unfilled holes would return zeros or errors — silent
// data corruption while the container continues running.
//
// The shim's lifetime is exactly the container's lifetime; it cannot restart
// independently.  This is the right process boundary for the supervisor.
//
// # How it works
//
// When the shim receives a "block" mount with "fill=sparse":
//
//  1. losetup attaches the local sparse backing file (Source) as read-only.
//  2. mount(2) mounts the target filesystem over the loop device.
//  3. The shim opens a Fill stream to the daemon's BlockCache ttrpc service,
//     identifying itself via "Hello{blockid}".
//  4. If FAN_PRE_ACCESS is supported (kernel ≥6.13), a fanotify supervisor
//     is installed on the mountpoint.  Each FAN_PRE_ACCESS event triggers a
//     Fill request to the daemon; the daemon fills the chunk and responds with
//     the filled byte ranges; the shim updates its local page bitmap and ALLOWs
//     the read through.  When all pages are present the mark is removed and
//     the mount becomes a plain fully-populated filesystem.
//  5. If FAN_PRE_ACCESS is not supported, the shim sends a single Fill request
//     covering the entire file and waits until the daemon reports all pages
//     present before returning from Mount.
//
// # Lifecycle
//
// MountAll is the entry point.  Call it before mount.All(remainingMounts, rootfs)
// in the shim's task creation path, passing the full rootfs mount list.  It
// handles "block" types inline and returns the remaining non-block mounts for
// the caller to process normally.
//
// Cleanup is via Unmount called during container teardown.
package blockmount

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"

	blockcachev1 "github.com/containerd/containerd/api/services/blockcache/v1"
	apitypes "github.com/containerd/containerd/api/types"
	"github.com/containerd/containerd/v2/internal/dmverity"
	"github.com/containerd/containerd/v2/internal/erofsmeta"
	"github.com/containerd/log"
	"golang.org/x/sys/unix"
)

// Handler is the shim-side block mount manager.  One Handler is created per
// shim instance (shared across all containers in the shim).
type Handler struct {
	ttrpcAddr string // daemon ttrpc socket address

	mu      sync.Mutex
	mounts  map[string]*activeMount // keyed by mountpoint
}

type activeMount struct {
	loopDev    string
	blockID    string
	target     string
	supervisor *supervisor // nil when fill=full or fanotify not supported
	stream     blockcachev1.TTRPCBlockCache_FillClient

	// dmVerityName, when non-empty, is the name of the /dev/mapper/<name>
	// dm-verity device interposed between the loop device and the EROFS
	// mount.  Unmount closes the device after unix.Unmount; the loop
	// device referenced by the verity target then auto-detaches.
	dmVerityName string
}

// NewHandler returns a Handler that dials the daemon at ttrpcAddr for Fill
// streams.
func NewHandler(ttrpcAddr string) *Handler {
	return &Handler{
		ttrpcAddr: ttrpcAddr,
		mounts:    make(map[string]*activeMount),
	}
}

// MountAll processes the rootfs mount list, handling any "block" type mounts
// inline.  Non-block mounts are returned unchanged for the caller to mount
// with the standard mount.All path.
//
// Modeled on nerdbox/internal/mountutil.All: iterate mounts, dispatch "block"
// type to this handler, pass everything else through.
func (h *Handler) MountAll(ctx context.Context, mounts []*apitypes.Mount, rootfs, mdir string) ([]*apitypes.Mount, error) {
	var remaining []*apitypes.Mount
	for _, m := range mounts {
		if m.Type != "block" {
			remaining = append(remaining, m)
			continue
		}
		if err := h.mount(ctx, m, rootfs, mdir); err != nil {
			return nil, fmt.Errorf("block mount: %w", err)
		}
	}
	return remaining, nil
}

// mount handles a single "block" mount descriptor.
func (h *Handler) mount(ctx context.Context, m *apitypes.Mount, rootfs, mdir string) error {
	backingFile := m.Source
	if backingFile == "" {
		return fmt.Errorf("block mount source (backing file) must not be empty")
	}

	// Parse options.
	target := "erofs"
	blockID := ""
	fillSparse := false
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
		if opt == "fill=sparse" {
			fillSparse = true
			continue
		}
		if strings.HasPrefix(opt, "fill=") {
			// Unknown fill mode — treat as sparse for forward-compat.
			fillSparse = true
			continue
		}
		if v, ok := strings.CutPrefix(opt, "dmverity-roothash="); ok {
			verityRoot = v
			continue
		}
		if v, ok := strings.CutPrefix(opt, "dmverity-hashoffset="); ok {
			n, perr := strconv.ParseUint(v, 10, 64)
			if perr != nil {
				return fmt.Errorf("block mount: parse dmverity-hashoffset=%q: %w", v, perr)
			}
			verityHashOffset = n
			continue
		}
		if v, ok := strings.CutPrefix(opt, "dmverity-blocksize="); ok {
			n, perr := strconv.ParseUint(v, 10, 32)
			if perr != nil {
				return fmt.Errorf("block mount: parse dmverity-blocksize=%q: %w", v, perr)
			}
			verityBlockSize = uint32(n)
			continue
		}
		extraOpts = append(extraOpts, opt)
	}
	if blockID == "" {
		return fmt.Errorf("block mount missing blockid= option (source=%s)", backingFile)
	}

	// Verity hard-fail policy (Q4): present-but-broken verity is fatal.
	verityRequested := verityRoot != ""
	if verityRequested && verityHashOffset == 0 {
		return fmt.Errorf("block mount: dmverity-roothash given without dmverity-hashoffset (blockid=%s)", blockID)
	}
	if verityRequested {
		if supported, serr := dmverity.IsSupported(); !supported {
			if serr == nil {
				serr = fmt.Errorf("dm_verity kernel module not loaded")
			}
			return fmt.Errorf("block mount: dmverity requested but unavailable: %w (blockid=%s)", serr, blockID)
		}
	}

	// Attach a loop device.
	loopDev, err := losetupReadOnly(backingFile)
	if err != nil {
		return fmt.Errorf("losetup %s: %w", backingFile, err)
	}

	// Verity pre-fill: open the Fill stream early and request the
	// daemon fill (a) the SB chunk, (b) the inode-table region, and
	// (c) the merkle-tree region BEFORE dm-verity opens — every byte
	// dm-verity hashes during setup or block verification must be
	// resident, otherwise verity reads zeros from a hole and fails
	// the block permanently.  This pre-fill preserves lazy loading
	// for the EROFS file-data region (which container reads will
	// fault in via fanotify on the mountpoint once we mount).
	var stream blockcachev1.TTRPCBlockCache_FillClient
	if verityRequested {
		stream, err = h.openFillStream(ctx, blockID)
		if err != nil {
			_ = losetupDetach(loopDev)
			return fmt.Errorf("block mount: open BlockCache Fill stream for verity pre-fill (%s): %w", blockID, err)
		}
		if perr := prefillForVerity(ctx, backingFile, blockID, verityHashOffset, stream); perr != nil {
			_ = stream.CloseSend()
			_ = losetupDetach(loopDev)
			return fmt.Errorf("block mount: verity pre-fill failed for %s: %w", blockID, perr)
		}
		log.G(ctx).WithFields(log.Fields{
			"blockid":     blockID,
			"hash_offset": verityHashOffset,
		}).Debug("block mount: verity pre-fill complete")
	}

	// Verity setup: insert a dm-verity target between the loop device
	// and the EROFS mount.  hashDevice == dataDevice (single-file
	// layout — the merkle tree is appended to the EROFS image).
	mountSource := loopDev
	var verityName string
	if verityRequested {
		verityName = computeVerityName(rootfs, blockID)
		dev, verr := dmverity.Open(backingFile, verityName, backingFile, verityRoot, verityHashOffset,
			&dmverity.DmverityOptions{
				DataBlockSize: effectiveBlockSize(verityBlockSize),
				HashBlockSize: effectiveBlockSize(verityBlockSize),
				HashOffset:    verityHashOffset,
			})
		if verr != nil {
			if stream != nil {
				_ = stream.CloseSend()
			}
			_ = losetupDetach(loopDev)
			return fmt.Errorf("block mount: dmverity.Open for %s: %w", blockID, verr)
		}
		mountSource = dev
		log.G(ctx).WithFields(log.Fields{
			"blockid":       blockID,
			"verity_name":   verityName,
			"verity_device": dev,
		}).Debug("block mount: dm-verity device opened")
	}

	// Mount the target filesystem over the (verity or loop) block device.
	flags := uintptr(unix.MS_RDONLY)
	opts := strings.Join(extraOpts, ",")
	if err := unix.Mount(mountSource, rootfs, target, flags, opts); err != nil {
		if verityName != "" {
			_ = dmverity.Close(verityName)
		}
		if stream != nil {
			_ = stream.CloseSend()
		}
		_ = losetupDetach(loopDev)
		return fmt.Errorf("mount %s (%s) at %s: %w", mountSource, target, rootfs, err)
	}

	log.G(ctx).WithFields(log.Fields{
		"blockid":  blockID,
		"loopdev":  loopDev,
		"source":   mountSource,
		"rootfs":   rootfs,
		"fill":     fillSparse,
		"verity":   verityRequested,
	}).Debug("block mount: mounted")

	var sup *supervisor
	if fillSparse {
		// Open the Fill stream if not already open (verity opened it
		// early for the pre-fill).  Reuse the same stream — the daemon
		// keeps the cache handle alive for the lifetime of the stream.
		if stream == nil {
			stream, err = h.openFillStream(ctx, blockID)
			if err != nil {
				if verityName != "" {
					_ = dmverity.Close(verityName)
				}
				_ = unix.Unmount(rootfs, unix.MNT_DETACH)
				_ = losetupDetach(loopDev)
				return fmt.Errorf("open BlockCache Fill stream for %s: %w", blockID, err)
			}
		}

		if isFanotifyPreContentSupported() {
			// On-demand: install fanotify supervisor on the mountpoint.
			sup, err = newSupervisor(ctx, rootfs, backingFile, blockID, stream)
			if err != nil {
				_ = stream.CloseSend()
				if verityName != "" {
					_ = dmverity.Close(verityName)
				}
				_ = unix.Unmount(rootfs, unix.MNT_DETACH)
				_ = losetupDetach(loopDev)
				return fmt.Errorf("start block-fill supervisor for %s: %w", blockID, err)
			}
			log.G(ctx).WithField("blockid", blockID).Debug("block mount: fanotify supervisor started")
		} else {
			// Full-fill fallback: send one Fill covering the whole file,
			// wait until the daemon reports all pages present.
			log.G(ctx).WithField("blockid", blockID).Debug("block mount: fanotify unavailable, doing full fill before mount completes")
			if err := fullFill(ctx, backingFile, blockID, stream); err != nil {
				_ = stream.CloseSend()
				if verityName != "" {
					_ = dmverity.Close(verityName)
				}
				_ = unix.Unmount(rootfs, unix.MNT_DETACH)
				_ = losetupDetach(loopDev)
				return fmt.Errorf("full fill for %s: %w", blockID, err)
			}
			log.G(ctx).WithField("blockid", blockID).Debug("block mount: full fill complete")
		}
	}

	h.mu.Lock()
	h.mounts[rootfs] = &activeMount{
		loopDev:      loopDev,
		blockID:      blockID,
		target:       target,
		supervisor:   sup,
		stream:       stream,
		dmVerityName: verityName,
	}
	h.mu.Unlock()
	return nil
}

// Unmount tears down a block mount created by MountAll.
func (h *Handler) Unmount(ctx context.Context, mountpoint string) error {
	h.mu.Lock()
	am, ok := h.mounts[mountpoint]
	if ok {
		delete(h.mounts, mountpoint)
	}
	h.mu.Unlock()

	if !ok {
		return nil
	}

	var errs []string

	// Stop the fanotify supervisor first — it must not fire EnsureRange after
	// the stream is closed.
	if am.supervisor != nil {
		if err := am.supervisor.stop(); err != nil {
			log.G(ctx).WithError(err).WithField("blockid", am.blockID).
				Warn("block mount: error stopping supervisor on unmount")
			errs = append(errs, fmt.Sprintf("stop supervisor: %v", err))
		}
	}

	// Close the Fill stream so the daemon releases the cache handle.
	if am.stream != nil {
		if err := am.stream.CloseSend(); err != nil {
			log.G(ctx).WithError(err).WithField("blockid", am.blockID).
				Warn("block mount: error closing Fill stream on unmount")
		}
	}

	if err := unix.Unmount(mountpoint, unix.MNT_DETACH); err != nil {
		errs = append(errs, fmt.Sprintf("unmount %s: %v", mountpoint, err))
	}
	// Close the dm-verity device (if any) BEFORE detaching the loop —
	// the verity target holds a kernel reference to the loop device,
	// so detaching the loop before closing the verity device would
	// fail with EBUSY.
	if am.dmVerityName != "" {
		if err := dmverity.Close(am.dmVerityName); err != nil {
			errs = append(errs, fmt.Sprintf("dmverity close %s: %v", am.dmVerityName, err))
		}
	}
	if am.loopDev != "" {
		if err := losetupDetach(am.loopDev); err != nil {
			errs = append(errs, fmt.Sprintf("losetup detach %s: %v", am.loopDev, err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("block unmount %s: %s", mountpoint, strings.Join(errs, "; "))
	}
	return nil
}

// ── loop device helpers ───────────────────────────────────────────────────────

func losetupReadOnly(path string) (string, error) {
	out, err := exec.Command("losetup", "--find", "--show", "--read-only", path).Output()
	if err != nil {
		return "", fmt.Errorf("losetup: %w (output: %s)", err, bytes.TrimSpace(out))
	}
	return string(bytes.TrimSpace(out)), nil
}

func losetupDetach(dev string) error {
	out, err := exec.Command("losetup", "-d", dev).CombinedOutput()
	if err != nil {
		return fmt.Errorf("losetup -d %s: %w (output: %s)", dev, err, bytes.TrimSpace(out))
	}
	return nil
}

// ── fill stream helpers ───────────────────────────────────────────────────────

func (h *Handler) openFillStream(ctx context.Context, blockID string) (blockcachev1.TTRPCBlockCache_FillClient, error) {
	client, err := newBlockCacheClient(ctx, h.ttrpcAddr)
	if err != nil {
		return nil, fmt.Errorf("dial daemon BlockCache service at %s: %w", h.ttrpcAddr, err)
	}
	stream, err := client.Fill(ctx)
	if err != nil {
		return nil, fmt.Errorf("open Fill stream: %w", err)
	}
	if err := stream.Send(&blockcachev1.FillMessage{
		Hello: &blockcachev1.Hello{Blockid: blockID},
	}); err != nil {
		_ = stream.CloseSend()
		return nil, fmt.Errorf("send Hello: %w", err)
	}
	return stream, nil
}

// fullFill sends a Fill request for [0, fileSize) and waits until the daemon
// reports all pages are resident (via Filled messages covering the full range).
// Used when fanotify is not available.
func fullFill(ctx context.Context, backingFile, blockID string, stream blockcachev1.TTRPCBlockCache_FillClient) error {
	// Stat the backing file to get its total size.
	var st syscall.Stat_t
	if err := syscall.Stat(backingFile, &st); err != nil {
		return fmt.Errorf("stat backing file %s: %w", backingFile, err)
	}
	totalSize := st.Size
	if totalSize == 0 {
		return nil // nothing to fill
	}

	pageSize := int64(syscall.Getpagesize())
	numPages := (totalSize + pageSize - 1) / pageSize

	// Build a simple page-present bitmap.
	pb := newPageBitmap(int(numPages), pageSize)

	// Send a Fill for the whole extent.
	if err := stream.Send(&blockcachev1.FillMessage{
		Fill: &blockcachev1.FillRequest{Offset: 0, Length: totalSize},
	}); err != nil {
		return fmt.Errorf("send Fill request: %w", err)
	}

	// Drain Filled messages until all pages are marked present.
	for !pb.allPresent() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		msg, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("recv Filled: %w", err)
		}
		if errMsg := msg.Error; errMsg != nil {
			return fmt.Errorf("daemon fill error for %s: %s", blockID, errMsg.GetMessage())
		}
		if filled := msg.Filled; filled != nil {
			for _, r := range filled.GetRanges() {
				pb.markRange(r.GetOffset(), r.GetLength())
			}
		}
	}
	return nil
}

// fillRange sends a Fill for the byte range [off, off+length) and
// blocks until the daemon reports the entire range filled via
// Filled messages.  Used by the verity pre-fill sequence — each
// distinct region (SB chunk, metadata, merkle tree) is filled by
// a separate call so we can request only what we need without
// touching the data section (which stays lazy).
func fillRange(ctx context.Context, off, length int64, blockID string, stream blockcachev1.TTRPCBlockCache_FillClient) error {
	if length <= 0 {
		return nil
	}
	pageSize := int64(syscall.Getpagesize())
	// Build a page bitmap covering only [off, off+length).  We track
	// presence by absolute file offset and round the requested range
	// down/up to page boundaries for the marking step.
	startPage := off / pageSize
	endPage := (off + length + pageSize - 1) / pageSize
	if endPage <= startPage {
		return nil
	}
	pb := newPageBitmap(int(endPage-startPage), pageSize)

	if err := stream.Send(&blockcachev1.FillMessage{
		Fill: &blockcachev1.FillRequest{Offset: off, Length: length},
	}); err != nil {
		return fmt.Errorf("send Fill[%d,%d): %w", off, off+length, err)
	}
	for !pb.allPresent() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		msg, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("recv Filled for [%d,%d): %w", off, off+length, err)
		}
		if errMsg := msg.Error; errMsg != nil {
			return fmt.Errorf("daemon fill error for %s [%d,%d): %s", blockID, off, off+length, errMsg.GetMessage())
		}
		filled := msg.Filled
		if filled == nil {
			continue
		}
		for _, r := range filled.GetRanges() {
			ro := r.GetOffset()
			rl := r.GetLength()
			// Clip to the page-bitmap's [off, off+length) window
			// so unrelated Filled messages don't mark out-of-range
			// pages.
			if ro+rl <= off {
				continue
			}
			if ro >= off+length {
				continue
			}
			clipOff := ro
			clipEnd := ro + rl
			if clipOff < off {
				clipOff = off
			}
			if clipEnd > off+length {
				clipEnd = off + length
			}
			pb.markRange(clipOff-off, clipEnd-clipOff)
		}
	}
	return nil
}

// prefillForVerity pre-fills the byte regions that dm-verity reads
// during Open() and block verification:
//   - the EROFS superblock chunk (chunk 0), so MetadataRange below can
//     parse the SB and locate the inode table.
//   - the inode-table region, so EROFS metabuf reads during mount-time
//     root-inode lookup hit resident pages.  (Both metabuf and
//     dm-verity bypass the fanotify-on-mount hook the shim uses for
//     content reads.)
//   - the dm-verity superblock + merkle tree at [hashOffset, end).
//
// Each region is filled by a separate Fill request so the data
// section between the metadata and the verity superblock stays lazy
// — the fanotify supervisor on the mountpoint catches container
// reads of unfilled data chunks and pulls them on demand.
func prefillForVerity(ctx context.Context, backingFile, blockID string, hashOffset uint64, stream blockcachev1.TTRPCBlockCache_FillClient) error {
	// Region 1: SB chunk.  Reading a single byte at offset 0 causes
	// EnsureRange on the daemon side to fill the entire chunk that
	// contains the EROFS superblock at offset 1024.
	if err := fillRange(ctx, 0, int64(erofsmeta.SuperBlockOffset+erofsmeta.SuperBlockSize), blockID, stream); err != nil {
		return fmt.Errorf("SB chunk: %w", err)
	}

	// Region 2: inode-table region (derived from the now-resident SB).
	f, err := os.Open(backingFile)
	if err != nil {
		return fmt.Errorf("open backing file for SB parse: %w", err)
	}
	metaOff, metaLen, merr := erofsmeta.MetadataRange(f)
	f.Close()
	if merr != nil {
		// Not an EROFS image, or SB parse failed.  Without a parsed
		// SB we can't know the metadata extent — fall back to a
		// conservative "fill everything before the verity tree"
		// strategy.  This is still better than full-fill because
		// the verity tree is typically near the end.
		log.G(ctx).WithError(merr).WithField("blockid", blockID).
			Warn("block mount: verity pre-fill: SB parse failed; pre-filling [0, hashOffset) conservatively")
		if err := fillRange(ctx, 0, int64(hashOffset), blockID, stream); err != nil {
			return fmt.Errorf("conservative pre-fill: %w", err)
		}
	} else if metaOff > 0 && metaLen > 0 {
		if err := fillRange(ctx, metaOff, metaLen, blockID, stream); err != nil {
			return fmt.Errorf("metadata region: %w", err)
		}
	}

	// Region 3: verity superblock + merkle tree.  Stat the backing
	// file to derive the tree length (file end - hashOffset).
	var st syscall.Stat_t
	if err := syscall.Stat(backingFile, &st); err != nil {
		return fmt.Errorf("stat backing file: %w", err)
	}
	if int64(hashOffset) >= st.Size {
		return fmt.Errorf("hashOffset %d ≥ file size %d", hashOffset, st.Size)
	}
	treeLen := st.Size - int64(hashOffset)
	if err := fillRange(ctx, int64(hashOffset), treeLen, blockID, stream); err != nil {
		return fmt.Errorf("verity tree: %w", err)
	}
	return nil
}

// effectiveBlockSize returns the dm-verity block size requested by
// the caller, or DefaultBlockSize when zero.
func effectiveBlockSize(requested uint32) uint32 {
	if requested == 0 {
		return dmverity.DefaultBlockSize
	}
	return requested
}

// computeVerityName produces a stable, dm-name-legal identifier for
// the dm-verity device backing a single mountpoint.  Mirrors the
// daemon-side helper so the same (mp, blockID) pair always maps to
// the same name on either side, easing forensic debugging
// (`dmsetup ls | grep containerd-block-`).
func computeVerityName(mp, blockID string) string {
	h := sha256.Sum256([]byte(mp + "\x00" + blockID))
	return fmt.Sprintf("containerd-block-%x", h[:6])
}

// isFanotifyPreContentSupported probes whether the kernel supports
// FAN_CLASS_PRE_CONTENT (Linux ≥6.13) without any side effect.
func isFanotifyPreContentSupported() bool {
	fd, err := unix.FanotifyInit(
		unix.FAN_CLASS_PRE_CONTENT|unix.FAN_CLOEXEC,
		unix.O_RDONLY|unix.O_LARGEFILE,
	)
	if err != nil {
		return false
	}
	unix.Close(fd)
	return true
}
