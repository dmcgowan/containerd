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

// Block supervisor — daemon-side fanotify supervisor for on-demand chunk fill.
//
// When FAN_CLASS_PRE_CONTENT is supported (Linux ≥ 6.13) the daemon mounts the
// EROFS loop device immediately after filling only chunk 0 (which contains the
// superblock), then installs a fanotify supervisor on the mountpoint.
//
// Every time the container reads a page that is not yet in the sparse backing
// file the kernel delivers a FAN_PRE_ACCESS event.  The supervisor calls
// handle.EnsureRange to fill the overlapping chunks, then responds FAN_ALLOW.
// The kernel read then proceeds with valid data.
//
// Once every chunk is present the supervisor removes the fanotify mark, after
// which the mount behaves identically to a fully-populated non-lazy mount with
// no ongoing goroutine overhead.

//go:build linux

package block

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/containerd/containerd/v2/core/content/index/cache"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/log"
	"golang.org/x/sys/unix"
)

// nextChunkPrefetchAhead controls how many uncompressed bytes past
// the last-fetched chunk we Prefetch on each fanotify event.  Set
// to 1 because go-erofs chunks are uniform and Prefetch resolves
// the byte-range to the chunk(s) it overlaps — so a single byte at
// nextOff fires the next-chunk fill exactly.  Raise this if you
// want a longer prediction window (e.g. chunk size × N to prefetch
// N chunks ahead); 1 chunk ahead is the conservative default that
// matches what `cache.WarmAll` does at concurrency=1.
const nextChunkPrefetchAhead int64 = 1

// daemonSupervisor watches the backing data file for FAN_PRE_ACCESS events and
// fills chunks on demand via handle.EnsureRange.  Unlike the shim supervisor it
// does not use a ttrpc stream — it calls the cache handle directly.
//
// The fanotify mark is on the backing file's INODE (not on the EROFS mountpoint):
// EROFS does not support FAN_CLASS_PRE_CONTENT (no SB_I_ALLOW_HSM), but btrfs/
// ext4 (where the backing file lives) does.  FAN_PRE_ACCESS fires when the loop
// device page-cache misses a page that is a sparse hole in the backing file.
type daemonSupervisor struct {
	mp          string
	backingFile string
	blockID     string
	handle      cache.Handle
	fd          int

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// closeFd guards unix.Close(s.fd) so stop() and promote() may both run
	// without double-closing the fanotify group fd.  Closing the group fd
	// also releases any reader stuck in fanotify_handle_event by causing
	// the kernel to auto-ALLOW pending permission events.
	closeFd sync.Once

	// fillCtx is a long-lived context for EnsureRange calls.  It carries the
	// namespace from the original Mount() request (required by the indexed
	// content store) but is detached from the request's cancellation/timeout
	// — fanotify event handlers must be able to run to completion regardless
	// of supervisor lifecycle.
	fillCtx context.Context

	// warmCtx + warmCancel drive the per-supervisor background warmer.
	// We spawn `handle.WarmAll(warmCtx)` once at supervisor-create time
	// so warming is bound to the supervisor's lifetime (not to the
	// transient cache.Warm() invocation from pull, which dies on daemon
	// restart).  warmCancel is fired in stop()/promote() so the warmer
	// goroutine exits cleanly on Unmount or fully-resident promotion.
	warmCtx    context.Context
	warmCancel context.CancelFunc

	// stats counters (read by Unmount for debug logging)
	eventsReceived atomic.Uint64
	fillsSent      atomic.Uint64
	deniedErrors   atomic.Uint64

	// eventSeq is a monotonic per-supervisor counter stamped on every
	// fanotify event we observe.  Trace lines downstream use it as a
	// stable identifier to correlate request/done pairs and to derive
	// an empirical load order from the captured fanotify stream.
	eventSeq atomic.Uint64
}

// newDaemonSupervisor opens a fanotify group in FAN_CLASS_PRE_CONTENT mode,
// marks the BACKING FILE's inode for FAN_PRE_ACCESS, and starts the event
// loop goroutine.
//
// Important: we mark the backing data file (on btrfs/ext4/etc.), NOT the EROFS
// mountpoint.  EROFS does not set SB_I_ALLOW_HSM so fanotify marks on it return
// EOPNOTSUPP.  The backing file's host filesystem (typically btrfs) does support
// HSM, and fanotify fires FAN_PRE_ACCESS when the loop device reads a page that
// is backed by a sparse hole — i.e., an unfilled chunk.  This fires BEFORE the
// read returns, so we can fill the hole and respond FAN_ALLOW before the kernel
// delivers (zero) data to EROFS.
//
// The loop device must NOT use O_DIRECT (Direct: false in SetupLoop): O_DIRECT
// bypasses the backing file's page cache, skipping the filemap_pre_read_folio_hook
// that generates FAN_PRE_ACCESS events.
//
// The caller must call stop() when the mount is being torn down.
func newDaemonSupervisor(ctx context.Context, mp, backingFile, blockID string, handle cache.Handle) (*daemonSupervisor, error) {
	fd, err := unix.FanotifyInit(
		unix.FAN_CLASS_PRE_CONTENT|unix.FAN_CLOEXEC,
		unix.O_RDONLY|unix.O_LARGEFILE,
	)
	if err != nil {
		return nil, fmt.Errorf("block supervisor: fanotify init: %w", err)
	}

	// Open the backing file to get an fd for FAN_MARK_INODE.
	// The mark persists by inode so the fd can be closed after marking.
	backingFd, err := unix.Open(backingFile, unix.O_RDONLY|unix.O_LARGEFILE, 0)
	if err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("block supervisor: open backing file %s: %w", backingFile, err)
	}
	markErr := unix.FanotifyMark(
		fd,
		unix.FAN_MARK_ADD|unix.FAN_MARK_INODE,
		unix.FAN_PRE_ACCESS,
		backingFd,
		"",
	)
	unix.Close(backingFd)
	if markErr != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("block supervisor: fanotify mark inode %s: %w", backingFile, markErr)
	}

	// Decouple the supervisor's lifetime from the request ctx that triggered
	// this Mount.  The Activate RPC ctx is cancelled the moment Activate
	// returns (~tens of ms after the shim connects), long before the
	// container has read any bytes through the EROFS mount.  If the
	// supervisor ctx is derived from that ctx, the event loop exits while
	// the fanotify mark is still live — any subsequent runc/EROFS read
	// queues a FAN_PRE_ACCESS event to a group with no reader and the
	// task wedges in uninterruptible D-state inside fanotify_handle_event.
	//
	// The supervisor is terminated explicitly by stop() (on Unmount) or
	// promote() (when AllPresent), both of which close the group fd via
	// closeFd.Once so any in-flight permission event is auto-ALLOWed and
	// the reader unblocks cleanly.
	sctx, cancel := context.WithCancel(context.Background())

	// Build a long-lived fill context: detached from the request's cancel/timeout
	// but preserving the namespace (required by the indexed content store for
	// EnsureRange → FillChunk lookups).
	fillCtx := context.Background()
	if ns, ok := namespaces.Namespace(ctx); ok {
		fillCtx = namespaces.WithNamespace(fillCtx, ns)
	}

	// Per-supervisor warmer ctx: derived from fillCtx (carries the
	// namespace) but with its own cancellation, so stop()/promote()
	// can terminate background warming without interrupting in-flight
	// fanotify event handlers (which run on fillCtx directly).
	warmCtx, warmCancel := context.WithCancel(fillCtx)

	s := &daemonSupervisor{
		mp:          mp,
		backingFile: backingFile,
		blockID:     blockID,
		handle:      handle,
		fd:          fd,
		ctx:         sctx,
		cancel:      cancel,
		fillCtx:     fillCtx,
		warmCtx:     warmCtx,
		warmCancel:  warmCancel,
	}

	log.G(ctx).WithFields(log.Fields{
		"blob": blockID,
		"mp":   mp,
	}).Info("[lazy-viz] fanotify_supervisor_start")

	s.wg.Add(1)
	go s.eventLoop()

	// Spawn the background warmer.  Bound to the supervisor's
	// lifetime so it's guaranteed to be running while fanotify is
	// active — even on a daemon-restart-without-re-pull, where the
	// pull-time cache.Warm() goroutine is long gone.  WarmAll runs
	// at PriorityBackground with concurrency=1, fills chunks in
	// strict sequential order (0..N-1), and yields to fanotify via
	// inflight coalescing whenever a foreground event lands on a
	// chunk the warmer is currently filling.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.handle.WarmAll(s.warmCtx); err != nil && s.warmCtx.Err() == nil {
			log.G(s.ctx).WithError(err).WithField("blob", s.blockID).
				Warn("[lazy-viz-debug] supervisor warmer exited with error")
		} else {
			log.G(s.ctx).WithField("blob", s.blockID).
				Info("[lazy-viz-debug] supervisor warmer exited")
		}
	}()

	return s, nil
}

// stop signals the supervisor to exit and waits for all goroutines.
// Must be called before unmounting the filesystem.
func (s *daemonSupervisor) stop() {
	evs := s.eventsReceived.Load()
	fills := s.fillsSent.Load()
	denied := s.deniedErrors.Load()
	log.G(s.ctx).WithFields(log.Fields{
		"blob":           s.blockID,
		"events":         evs,
		"fills_sent":     fills,
		"denied_errors":  denied,
		"all_present":    s.handle.AllPresent(),
	}).Info("block supervisor: stopping")

	s.cancel()
	// Cancel the background warmer first.  It runs on warmCtx
	// (derived from fillCtx, namespace-preserving) and is in
	// s.wg, so we must signal it before s.wg.Wait() below.
	if s.warmCancel != nil {
		s.warmCancel()
	}
	// Close the group fd through the sync.Once gate.  promote() may also
	// close it; whichever runs first wins and the other becomes a no-op.
	// Closing the group fd is what releases any reader stuck in
	// fanotify_handle_event (the kernel auto-ALLOWs pending events).
	s.closeFd.Do(func() { unix.Close(s.fd) })
	s.wg.Wait()
}

// promote removes the fanotify mark and cancels the supervisor context.
// After this the mount is a plain fully-populated EROFS mount.
func (s *daemonSupervisor) promote() {
	// Remove the inode mark on the backing file.
	if bfd, err := unix.Open(s.backingFile, unix.O_RDONLY|unix.O_LARGEFILE, 0); err == nil {
		_ = unix.FanotifyMark(
			s.fd,
			unix.FAN_MARK_REMOVE|unix.FAN_MARK_INODE,
			unix.FAN_PRE_ACCESS,
			bfd,
			"",
		)
		unix.Close(bfd)
	}
	log.G(s.ctx).WithFields(log.Fields{
		"blob":       s.blockID,
		"events":     s.eventsReceived.Load(),
		"fills_sent": s.fillsSent.Load(),
	}).Info("[lazy-viz] fanotify_promote")
	s.cancel()
	// Stop the background warmer too — promote() means the image
	// reached AllPresent, so the warmer has no more work to do.
	if s.warmCancel != nil {
		s.warmCancel()
	}
	// Close the group fd through the sync.Once gate.  After mark removal +
	// fd close the kernel auto-ALLOWs any pending permission events,
	// releasing readers stuck in fanotify_handle_event.  Idempotent with
	// stop(): whichever runs first closes; the other becomes a no-op.
	s.closeFd.Do(func() { unix.Close(s.fd) })
}

// ─── fanotify structures ─────────────────────────────────────────────────────

// fanInfoHeader mirrors struct fanotify_event_info_header (4 bytes).
type fanInfoHeader struct {
	InfoType uint8
	Pad      uint8
	Len      uint16
}

// fanInfoRange mirrors struct fanotify_event_info_range (24 bytes).
type fanInfoRange struct {
	Hdr    fanInfoHeader
	Pad    uint32
	Offset uint64
	Count  uint64
}

func (s *daemonSupervisor) eventLoop() {
	defer s.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			log.G(s.ctx).WithFields(log.Fields{
				"blob":  s.blockID,
				"panic": fmt.Sprintf("%v", r),
			}).Error("block supervisor: eventLoop PANIC")
		}
	}()

	// Buffer large enough for many batched events.  Each event = 24 bytes
	// metadata + ~24 bytes range info = ~48 bytes.  64 KiB holds ~1300 events.
	buf := make([]byte, 65536)
	pfd := []unix.PollFd{{Fd: int32(s.fd), Events: unix.POLLIN}}

	log.G(s.ctx).WithFields(log.Fields{
		"blob": s.blockID,
		"fd":   s.fd,
	}).Info("[lazy-viz-debug] eventLoop: starting")

	pollIter := 0
	for {
		if s.ctx.Err() != nil {
			log.G(s.ctx).WithField("blob", s.blockID).Info("[lazy-viz-debug] eventLoop: ctx done, exiting")
			return
		}

		pfd[0].Revents = 0
		n, err := unix.Poll(pfd, 200) // 200 ms timeout
		pollIter++
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			log.G(s.ctx).WithError(err).WithField("blob", s.blockID).Warn("[lazy-viz-debug] eventLoop: Poll error, exiting")
			return
		}
		if n == 0 {
			// Timeout — check context and loop.
			continue
		}
		if pfd[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
			log.G(s.ctx).WithFields(log.Fields{
				"blob":    s.blockID,
				"revents": fmt.Sprintf("0x%x", pfd[0].Revents),
			}).Warn("[lazy-viz-debug] eventLoop: POLLHUP/POLLERR/POLLNVAL, exiting")
			return
		}
		if pfd[0].Revents&unix.POLLIN == 0 {
			continue
		}

		n, err = unix.Read(s.fd, buf)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			if err == syscall.EBADF || err == syscall.ENODEV || s.ctx.Err() != nil {
				log.G(s.ctx).WithError(err).WithField("blob", s.blockID).Info("[lazy-viz-debug] eventLoop: Read EBADF/ENODEV, exiting")
				return
			}
			log.G(s.ctx).WithError(err).WithField("blob", s.blockID).
				Error("block supervisor: fanotify read error")
			return
		}
		if n == 0 {
			log.G(s.ctx).WithField("blob", s.blockID).Info("[lazy-viz-debug] eventLoop: Read returned 0, exiting")
			return
		}
		log.G(s.ctx).WithFields(log.Fields{
			"blob":  s.blockID,
			"bytes": n,
			"poll":  pollIter,
		}).Info("[lazy-viz-debug] eventLoop: read events")
		s.dispatch(buf[:n])
	}
}

func (s *daemonSupervisor) dispatch(buf []byte) {
	const metaSz = int(unsafe.Sizeof(unix.FanotifyEventMetadata{}))
	off := 0
	evCount := 0
	for off+metaSz <= len(buf) {
		evLen := int(binary.NativeEndian.Uint32(buf[off : off+4]))
		if evLen < metaSz || off+evLen > len(buf) {
			log.G(s.ctx).WithFields(log.Fields{
				"blob":      s.blockID,
				"ev_len":    evLen,
				"off":       off,
				"buf_len":   len(buf),
				"ev_index":  evCount,
			}).Warn("[lazy-viz-debug] dispatch: malformed/truncated event, stopping")
			break
		}
		meta := *(*unix.FanotifyEventMetadata)(unsafe.Pointer(&buf[off]))

		// Walk info records to find the FAN_EVENT_INFO_TYPE_RANGE record.
		infoOff := off + metaSz
		infoEnd := off + evLen
		var rangeFound bool
		var evOff, evRangeCount uint64
		var infoTypes []uint8

		for infoOff+4 <= infoEnd {
			hdr := *(*fanInfoHeader)(unsafe.Pointer(&buf[infoOff]))
			recLen := int(hdr.Len)
			if recLen < 4 || infoOff+recLen > infoEnd {
				break
			}
			infoTypes = append(infoTypes, hdr.InfoType)
			if hdr.InfoType == unix.FAN_EVENT_INFO_TYPE_RANGE {
				if recLen >= int(unsafe.Sizeof(fanInfoRange{})) {
					ir := *(*fanInfoRange)(unsafe.Pointer(&buf[infoOff]))
					evOff = ir.Offset
					evRangeCount = ir.Count
					rangeFound = true
				}
			}
			infoOff += recLen
		}

		log.G(s.ctx).WithFields(log.Fields{
			"blob":        s.blockID,
			"ev_index":    evCount,
			"ev_len":      evLen,
			"mask":        fmt.Sprintf("0x%x", meta.Mask),
			"meta_fd":     meta.Fd,
			"pid":         meta.Pid,
			"range_found": rangeFound,
			"range_off":   evOff,
			"range_count": evRangeCount,
			"info_types":  infoTypes,
		}).Info("[lazy-viz-debug] dispatch: event")

		if meta.Mask&unix.FAN_PRE_ACCESS != 0 {
			s.eventsReceived.Add(1)
			// Mint a monotonic event sequence number BEFORE spawning
			// the per-event goroutine so request/done log pairs
			// share the same correlator and downstream parsers can
			// derive a deterministic load-order trace.
			seq := s.eventSeq.Add(1)
			if rangeFound {
				eventFd := meta.Fd
				go s.handleEvent(seq, meta.Pid, eventFd, evOff, evRangeCount)
			} else {
				// No range info — allow unconditionally (conservative).
				log.G(s.ctx).WithFields(log.Fields{
					"blob":      s.blockID,
					"event_seq": seq,
					"pid":       meta.Pid,
					"meta_fd":   meta.Fd,
				}).Info("[lazy-viz] fanotify_event_no_range")
				s.respond(meta.Fd, unix.FAN_ALLOW)
			}
		} else {
			// Unknown event type — log and close fd if valid.
			log.G(s.ctx).WithFields(log.Fields{
				"blob":    s.blockID,
				"mask":    fmt.Sprintf("0x%x", meta.Mask),
				"meta_fd": meta.Fd,
			}).Warn("[lazy-viz-debug] dispatch: non-PRE_ACCESS event received")
			if meta.Fd >= 0 {
				unix.Close(int(meta.Fd))
			}
		}

		off += evLen
		evCount++
	}
}

// handleEvent processes one FAN_PRE_ACCESS event by calling EnsureRange for
// the affected byte range, then responding FAN_ALLOW (or FAN_DENY on error).
//
// Emits one structured `[lazy-viz] fanotify_fill_request` line per event with
// the full chunk-index → chunk-digest correspondence for the requested range,
// and one `[lazy-viz] fanotify_fill_done` line on success.  Both lines share
// the same event_seq so downstream tooling can pair them and reconstruct a
// deterministic load-order trace for the workload.
func (s *daemonSupervisor) handleEvent(seq uint64, pid int32, eventFd int32, offset, count uint64) {
	// NOT tracked in s.wg — see dispatch() comment.
	defer func() {
		if r := recover(); r != nil {
			log.G(s.ctx).WithFields(log.Fields{
				"blob":      s.blockID,
				"event_seq": seq,
				"off":       offset,
				"len":       count,
				"panic":     fmt.Sprintf("%v", r),
			}).Error("[lazy-viz-debug] handleEvent PANIC — responding FAN_DENY")
			s.deniedErrors.Add(1)
			s.respond(eventFd, unix.FAN_DENY|supDenyEIO())
		}
	}()

	off := int64(offset)
	cnt := int64(count)

	// Resolve the chunk-index → chunk-digest mapping for this range
	// BEFORE filling.  Capturing it pre-fill records the kernel's
	// causal demand (which chunks the read actually needed), and lets
	// us flag which chunks were already resident vs newly filled.
	chunks := s.handle.ChunksInRange(off, cnt)
	chunkIndices := make([]int, len(chunks))
	chunkDigests := make([]string, len(chunks))
	chunkRanges := make([]map[string]int64, len(chunks))
	presentBefore := 0
	for i, c := range chunks {
		chunkIndices[i] = c.Index
		chunkDigests[i] = c.Digest.String()
		chunkRanges[i] = map[string]int64{
			"idx":           int64(c.Index),
			"cache_off":     c.Offset,
			"cache_len":     c.Length,
			"on_blob_start": c.OnBlobStart,
			"on_blob_end":   c.OnBlobEnd,
		}
		if c.Present {
			presentBefore++
		}
	}
	chunksJSON, _ := json.Marshal(chunkIndices)
	digestsJSON, _ := json.Marshal(chunkDigests)
	rangesJSON, _ := json.Marshal(chunkRanges)

	log.G(s.ctx).WithFields(log.Fields{
		"blob":           s.blockID,
		"event_seq":      seq,
		"pid":            pid,
		"off":            off,
		"len":            cnt,
		"priority":       "fg",
		"event_fd":       eventFd,
		"chunk_count":    len(chunks),
		"present_before": presentBefore,
		"chunks":         string(chunksJSON),
		"digests":        string(digestsJSON),
		"chunk_ranges":   string(rangesJSON),
	}).Info("[lazy-viz] fanotify_fill_request")
	s.fillsSent.Add(1)

	// EnsureRange fills all chunks overlapping [off, off+cnt).
	// Already-present chunks are skipped via the in-memory bitmap (fast path).
	//
	// We use s.fillCtx (long-lived, namespace-preserving) — NOT the supervisor
	// ctx — so that a cancelled supervisor ctx (from stop()) does NOT abort
	// fills for ranges that are already partially fetched or need fetching.
	// fillCtx carries the namespace from the original Mount() request, which
	// the indexed content store requires for blob lookups.
	//
	// Critical: we must respond FAN_ALLOW whenever EnsureRange returns nil,
	// even if the supervisor ctx is already cancelled.  Responding FAN_DENY for
	// data that IS present sends EIO to the filesystem driver (EROFS, overlay)
	// and causes the in-progress mount or read to fail permanently.
	err := s.handle.EnsureRange(s.fillCtx, off, cnt)
	if err != nil {
		log.G(s.ctx).WithError(err).WithFields(log.Fields{
			"blob":      s.blockID,
			"event_seq": seq,
			"off":       off,
			"len":       cnt,
			"chunks":    string(chunksJSON),
			"digests":   string(digestsJSON),
		}).Warn("[lazy-viz] fanotify_fill_failed")
		s.deniedErrors.Add(1)
		s.respond(eventFd, unix.FAN_DENY|supDenyEIO())
		return
	}

	log.G(s.ctx).WithFields(log.Fields{
		"blob":        s.blockID,
		"event_seq":   seq,
		"off":         off,
		"len":         cnt,
		"chunk_count": len(chunks),
		"chunks":      string(chunksJSON),
		"digests":     string(digestsJSON),
	}).Info("[lazy-viz] fanotify_fill_done")
	s.respond(eventFd, unix.FAN_ALLOW)

	// Predictive next-chunk prefetch.
	//
	// Fanotify is a hard block: the reading process is suspended
	// until our FAN_ALLOW reaches the kernel, which means by the
	// time we observe an event we have no way to peek at what
	// offset the workload will read NEXT.  But access patterns
	// — start-up library loads, dirent walks, sequential reads —
	// are overwhelmingly contiguous, so the chunk immediately
	// following the one we just satisfied is a high-probability
	// next fault.  We fire a fire-and-forget background fill for
	// it; if the prediction is right, the next foreground event
	// finds the chunk already resident (or in-flight, coalesced
	// via blobState.inflight) and unblocks faster.
	//
	// Wrong predictions waste at most one chunk of network — the
	// data still lives in the sparse cache and counts toward the
	// supervisor's promote() threshold, so nothing is lost.  We
	// trigger exactly nextChunkPrefetchAhead chunks past the
	// last touched chunk; tune that constant if you want a longer
	// look-ahead window.
	if len(chunks) > 0 {
		last := chunks[len(chunks)-1]
		nextOff := last.Offset + last.Length
		// Prefetch a small window starting at nextOff — go-erofs
		// chunks are uniform in the chunk-index coordinate
		// system, so 1 byte at nextOff lands in the next chunk
		// and the cache resolves the full chunk to prefetch.
		_ = s.handle.Prefetch(s.fillCtx, nextOff, nextChunkPrefetchAhead)
		log.G(s.ctx).WithFields(log.Fields{
			"blob":      s.blockID,
			"event_seq": seq,
			"next_off":  nextOff,
			"last_idx":  last.Index,
		}).Debug("[lazy-viz-debug] predictive next-chunk prefetch")
	}

	// Promote once the last chunk is filled.
	if s.handle.AllPresent() {
		s.promote()
	}
}

// respond writes a fanotify_response struct to the fanotify fd and closes the
// per-event file descriptor returned in the event metadata.
func (s *daemonSupervisor) respond(eventFd int32, response uint32) {
	resp := unix.FanotifyResponse{Fd: eventFd, Response: response}
	b := (*[8]byte)(unsafe.Pointer(&resp))[:]
	n, err := unix.Write(s.fd, b)
	if err != nil && err != syscall.EBADF {
		log.G(s.ctx).WithError(err).WithFields(log.Fields{
			"blob":     s.blockID,
			"event_fd": eventFd,
			"response": fmt.Sprintf("0x%x", response),
		}).Error("[lazy-viz-debug] respond: write to fanotify fd failed")
	} else if n != 8 {
		log.G(s.ctx).WithFields(log.Fields{
			"blob":     s.blockID,
			"event_fd": eventFd,
			"wrote":    n,
		}).Warn("[lazy-viz-debug] respond: short write (expected 8 bytes)")
	} else {
		log.G(s.ctx).WithFields(log.Fields{
			"blob":     s.blockID,
			"event_fd": eventFd,
			"response": fmt.Sprintf("0x%x", response),
		}).Info("[lazy-viz-debug] respond: FAN response written")
	}
	if err := unix.Close(int(eventFd)); err != nil && err != syscall.EBADF {
		log.G(s.ctx).WithError(err).WithFields(log.Fields{
			"blob":     s.blockID,
			"event_fd": eventFd,
		}).Warn("[lazy-viz-debug] respond: close eventFd failed")
	}
}

// supDenyEIO encodes EIO as a fanotify deny error code.
func supDenyEIO() uint32 {
	return uint32(syscall.EIO) << unix.FAN_ERRNO_SHIFT
}

// isFanotifyPreContentSupported returns true when the running kernel supports
// FAN_CLASS_PRE_CONTENT (Linux ≥ 6.13).  It probes by opening a fanotify fd
// and immediately closing it — no marks are installed.
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
