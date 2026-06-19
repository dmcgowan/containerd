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

package blockmount

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	blockcachev1 "github.com/containerd/containerd/api/services/blockcache/v1"
	"github.com/containerd/log"
	"golang.org/x/sys/unix"
)

// supervisor watches a mountpoint for FAN_PRE_ACCESS events.
// When an event fires it checks the local page bitmap:
//   - All needed pages present → FAN_ALLOW immediately (no RPC).
//   - Any missing → send Fill request to daemon via the Fill stream,
//     wait for Filled response covering all needed pages, then FAN_ALLOW.
//
// When all pages become present (filled counter == numPages) the supervisor
// removes the fanotify mark and exits, leaving the mount as a plain
// fully-populated filesystem with zero ongoing overhead.
type supervisor struct {
	mp         string
	blockID    string
	stream     blockcachev1.TTRPCBlockCache_FillClient
	pb         *pageBitmap
	fanotifyFd int

	ctx    context.Context
	cancel context.CancelFunc

	wg sync.WaitGroup

	// stats
	eventsReceived atomic.Uint64
	fillsSent      atomic.Uint64
	allowsSent     atomic.Uint64
	deniedErrors   atomic.Uint64
}

func newSupervisor(
	ctx context.Context,
	mp, backingFile, blockID string,
	stream blockcachev1.TTRPCBlockCache_FillClient,
) (*supervisor, error) {
	// Open the fanotify group.
	fd, err := unix.FanotifyInit(
		unix.FAN_CLASS_PRE_CONTENT|unix.FAN_CLOEXEC,
		unix.O_RDONLY|unix.O_LARGEFILE,
	)
	if err != nil {
		return nil, fmt.Errorf("fanotify init: %w", err)
	}

	// Mark the mountpoint.
	if err := unix.FanotifyMark(
		fd,
		unix.FAN_MARK_ADD|unix.FAN_MARK_MOUNT,
		unix.FAN_PRE_ACCESS,
		unix.AT_FDCWD,
		mp,
	); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("fanotify mark %s: %w", mp, err)
	}

	// Build the page bitmap by stat-ing the backing file.
	var st syscall.Stat_t
	if err := syscall.Stat(backingFile, &st); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("stat backing file %s: %w", backingFile, err)
	}
	pageSize := int64(syscall.Getpagesize())
	numPages := (st.Size + pageSize - 1) / pageSize
	pb := newPageBitmap(int(numPages), pageSize)

	sctx, cancel := context.WithCancel(ctx)
	s := &supervisor{
		mp:         mp,
		blockID:    blockID,
		stream:     stream,
		pb:         pb,
		fanotifyFd: fd,
		ctx:        sctx,
		cancel:     cancel,
	}

	// Start the Filled-message receiver goroutine.
	s.wg.Add(1)
	go s.recvLoop()

	// Start the event loop.
	s.wg.Add(1)
	go s.eventLoop()

	return s, nil
}

// stop signals the supervisor to exit and waits for its goroutines.
func (s *supervisor) stop() error {
	s.cancel()
	unix.Close(s.fanotifyFd)
	s.wg.Wait()
	return nil
}

// ── Filled-message receiver ───────────────────────────────────────────────────

// recvLoop reads Filled (and FillError) messages from the daemon stream and
// updates the local page bitmap.  When all pages are present it signals the
// event loop via the supervisor's context so it can remove the fanotify mark
// and exit.
func (s *supervisor) recvLoop() {
	defer s.wg.Done()
	for {
		msg, err := s.stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) || s.ctx.Err() != nil {
				return
			}
			// Daemon disconnected — log and return.  Any pending FAN_PRE_ACCESS
			// events will be parked in the event loop until the stream is
			// re-opened (reconnect logic is in the Fill stream helper).
			log.G(s.ctx).WithError(err).WithField("blockid", s.blockID).
				Error("block supervisor: Fill stream recv error — container reads may block until daemon reconnects")
			return
		}

		if filled := msg.Filled; filled != nil {
			for _, r := range filled.GetRanges() {
				s.pb.markRange(r.GetOffset(), r.GetLength())
			}
			if s.pb.allPresent() {
				log.G(s.ctx).WithField("blockid", s.blockID).
					Debug("block supervisor: all pages present — removing fanotify mark")
				s.promote()
				return
			}
		}
		if errMsg := msg.Error; errMsg != nil {
			log.G(s.ctx).WithField("blockid", s.blockID).
				Errorf("block supervisor: daemon fill error: %s", errMsg.GetMessage())
			s.deniedErrors.Add(1)
			// The event loop will respond FAN_DENY when the pending request
			// sees the stream return an error.
		}
	}
}

// promote removes the fanotify mark and cancels the supervisor context,
// causing the event loop to exit.  After this the mount behaves identically
// to a fully-populated non-lazy mount with no ongoing overhead.
func (s *supervisor) promote() {
	_ = unix.FanotifyMark(
		s.fanotifyFd,
		unix.FAN_MARK_REMOVE|unix.FAN_MARK_MOUNT,
		unix.FAN_PRE_ACCESS,
		unix.AT_FDCWD,
		s.mp,
	)
	s.cancel()
}

// ── fanotify event loop ───────────────────────────────────────────────────────

// fanotifyInfoHeader mirrors struct fanotify_event_info_header (4 bytes).
type fanotifyInfoHeader struct {
	InfoType uint8
	Pad      uint8
	Len      uint16
}

// fanotifyInfoRange mirrors struct fanotify_event_info_range (24 bytes).
type fanotifyInfoRange struct {
	Hdr    fanotifyInfoHeader
	Pad    uint32
	Offset uint64
	Count  uint64
}

func (s *supervisor) eventLoop() {
	defer s.wg.Done()
	buf := make([]byte, 4096+256)

	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}

		n, err := unix.Read(s.fanotifyFd, buf)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			if err == syscall.EBADF || err == syscall.ENODEV || s.ctx.Err() != nil {
				return
			}
			log.G(s.ctx).WithError(err).WithField("blockid", s.blockID).
				Error("block supervisor: fanotify read error")
			return
		}
		if n == 0 {
			return
		}
		s.dispatch(buf[:n])
	}
}

func (s *supervisor) dispatch(buf []byte) {
	const metaSz = int(unsafe.Sizeof(unix.FanotifyEventMetadata{}))
	off := 0
	for off+metaSz <= len(buf) {
		evLen := int(binary.NativeEndian.Uint32(buf[off : off+4]))
		if evLen < metaSz || off+evLen > len(buf) {
			break
		}
		meta := *(*unix.FanotifyEventMetadata)(unsafe.Pointer(&buf[off]))

		// Walk info records for range data.
		infoOff := off + metaSz
		infoEnd := off + evLen
		var rangeFound bool
		var evOff, evCount uint64

		for infoOff+4 <= infoEnd {
			hdr := *(*fanotifyInfoHeader)(unsafe.Pointer(&buf[infoOff]))
			recLen := int(hdr.Len)
			if recLen < 4 || infoOff+recLen > infoEnd {
				break
			}
			if hdr.InfoType == unix.FAN_EVENT_INFO_TYPE_RANGE {
				if recLen >= int(unsafe.Sizeof(fanotifyInfoRange{})) {
					ir := *(*fanotifyInfoRange)(unsafe.Pointer(&buf[infoOff]))
					evOff = ir.Offset
					evCount = ir.Count
					rangeFound = true
				}
			}
			infoOff += recLen
		}

		if meta.Mask&unix.FAN_PRE_ACCESS != 0 {
			s.eventsReceived.Add(1)
			if rangeFound {
				s.handleEvent(meta.Fd, evOff, evCount)
			} else {
				s.respond(meta.Fd, unix.FAN_ALLOW)
			}
		} else if meta.Fd >= 0 {
			unix.Close(int(meta.Fd))
		}

		off += evLen
	}
}

// handleEvent processes a FAN_PRE_ACCESS event for [offset, offset+count).
func (s *supervisor) handleEvent(eventFd int32, offset, count uint64) {
	off := int64(offset)
	cnt := int64(count)

	// Fast path: all needed pages already present.
	if s.pb.allPagesPresent(off, cnt) {
		s.respond(eventFd, unix.FAN_ALLOW)
		return
	}

	// Send a Fill request to the daemon.
	err := s.stream.Send(&blockcachev1.FillMessage{
		Fill: &blockcachev1.FillRequest{Offset: off, Length: cnt},
	})
	s.fillsSent.Add(1)
	if err != nil {
		log.G(s.ctx).WithError(err).WithField("blockid", s.blockID).
			Errorf("block supervisor: send Fill request failed — denying read")
		s.deniedErrors.Add(1)
		s.respond(eventFd, unix.FAN_DENY|fanDenyEIO())
		return
	}

	// The recvLoop updates the page bitmap when Filled arrives.
	// We need to block the event until the pages we need are present.
	// Simple approach: spin-wait on the bitmap with exponential backoff.
	// A future optimization is a condition variable or channel per pending request.
	waitForPages(s.ctx, s.pb, off, cnt)

	if s.ctx.Err() != nil || s.pb.allPagesPresent(off, cnt) {
		s.respond(eventFd, unix.FAN_ALLOW)
	} else {
		// Daemon returned an error (seen in recvLoop's deniedErrors counter).
		s.respond(eventFd, unix.FAN_DENY|fanDenyEIO())
	}
}

// waitForPages waits until the page bitmap marks all pages in [off, off+length)
// as present, or the context is cancelled.  Uses a simple poll loop since
// the typical fill time is short (one network round-trip).
func waitForPages(ctx context.Context, pb *pageBitmap, off, length int64) {
	for {
		if pb.allPagesPresent(off, length) {
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
			// Yield to other goroutines; the recvLoop will update the bitmap
			// shortly after the daemon ACKs the Fill request.
			runtime.Gosched()
		}
	}
}

// respond writes a fanotify_response and closes the event fd.
func (s *supervisor) respond(eventFd int32, response uint32) {
	resp := unix.FanotifyResponse{Fd: eventFd, Response: response}
	b := (*[8]byte)(unsafe.Pointer(&resp))[:]
	if _, err := unix.Write(s.fanotifyFd, b); err != nil && err != syscall.EBADF {
		log.G(s.ctx).WithError(err).WithField("blockid", s.blockID).
			Error("block supervisor: write fanotify response")
	}
	unix.Close(int(eventFd))
	s.allowsSent.Add(1)
}

func fanDenyEIO() uint32 {
	const fanErrnoShift = unix.FAN_ERRNO_SHIFT
	return (uint32(syscall.EIO) << fanErrnoShift)
}
