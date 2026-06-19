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

// Package registry provides a ByteProvider implementation that fetches indexed
// content blobs from an OCI-compliant container registry.
//
// # Range-request design
//
// The docker fetcher returned by remotes.Resolver.Fetcher implements
// io.ReadSeekCloser: seeking causes the next Read to open a new HTTP
// connection with "Range: bytes=<offset>-" (or reuse an existing connection
// when the seek matches the current stream position).  This package exploits
// that property so that:
//
//   - Open returns a ReaderAt backed by one seekable connection.  Each ReadAt
//     seeks to the requested offset and reads only the requested bytes,
//     issuing an HTTP Range request when necessary.  No full-blob download
//     occurs.
//
//   - Fetch issues a single HTTP Range request for [off, off+length) and
//     returns only those bytes to the caller.
//
// Each Open or Fetch call invokes fetcher.Fetch with the caller's
// context.Context so that HTTP requests honour the operation's lifetime
// directly.  A previous version of this provider cached the seekable
// ReadCloser between calls, but the docker fetcher's httpReadSeeker closure
// captures the ctx active at fetcher.Fetch time; reusing a cached seeker
// from a different operation caused HTTP requests to fail with "context
// canceled" when the original ctx had been canceled (e.g. when a Provider
// registered at pull time was later invoked from a separate run-time gRPC
// handler).
//
// Fallback: fetchers that return a non-seekable ReadCloser (e.g. in-process
// test registries) fall back to downloading the full blob into a
// bytes.NewReader buffer, logging a one-time warning per blob.
//
// Concurrency limits are per-Provider instance (typically per-registry host).
package registry

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/log"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/semaphore"
)

// Config controls the concurrency behaviour of a Provider.
type Config struct {
	// MaxConcurrentFetches is the maximum number of simultaneous chunk-fetch
	// operations. Default: 8.
	MaxConcurrentFetches int

	// ForegroundReserve is the minimum number of slots kept exclusively for
	// PriorityForeground requests. Default: 1.
	ForegroundReserve int
}

func (c *Config) setDefaults() {
	if c.MaxConcurrentFetches <= 0 {
		// 32 concurrent range requests is a good default: high enough to keep
		// a fast local or LAN registry saturated, low enough not to overwhelm
		// rate-limited public registries.  The EnsureAll worker pool caps
		// dispatch at 2×NumCPU (max 16), so the effective limit is
		// min(workerPool, MaxConcurrentFetches).
		c.MaxConcurrentFetches = 32
	}
	if c.ForegroundReserve <= 0 {
		c.ForegroundReserve = 1
	}
	if c.ForegroundReserve > c.MaxConcurrentFetches {
		c.ForegroundReserve = c.MaxConcurrentFetches
	}
}

// Provider implements contentindex.ByteProvider by fetching blobs from a
// registry via a remotes.Fetcher.
type Provider struct {
	fetcher remotes.Fetcher
	name    string
	cfg     Config

	// shared semaphore: used by both foreground and background.
	shared *semaphore.Weighted
	// foreground reserve: only foreground requests may acquire this.
	fgReserve *semaphore.Weighted
}

// New returns a Provider that uses fetcher to download blobs.
// name is a human-readable identifier (e.g. "registry:ghcr.io/foo/bar").
func New(fetcher remotes.Fetcher, name string, cfg Config) *Provider {
	cfg.setDefaults()
	sharedSlots := int64(cfg.MaxConcurrentFetches - cfg.ForegroundReserve)
	if sharedSlots < 1 {
		sharedSlots = 1
	}
	return &Provider{
		fetcher:   fetcher,
		name:      name,
		cfg:       cfg,
		shared:    semaphore.NewWeighted(sharedSlots),
		fgReserve: semaphore.NewWeighted(int64(cfg.ForegroundReserve)),
	}
}

// Name implements contentindex.ByteProvider.
func (p *Provider) Name() string { return p.name }

// Open returns a ReaderAt over the bytes of the blob.  The ReaderAt holds
// one open HTTP connection for the lifetime of the ReaderAt; ReadAt
// performs Seek + Read on it (which translates to a Range request when
// the seek position differs from the current stream position).
//
// The caller MUST Close the returned ReaderAt to release the connection.
func (p *Provider) Open(ctx context.Context, desc ocispec.Descriptor) (content.ReaderAt, error) {
	rc, err := p.fetcher.Fetch(ctx, desc)
	if err != nil {
		return nil, fmt.Errorf("registry provider: open %s: %w", desc.Digest, err)
	}
	if rsc, ok := rc.(io.ReadSeekCloser); ok {
		return &seekReaderAt{rc: rsc, size: desc.Size}, nil
	}
	// Fallback: download the full blob and serve from a bytes.Reader.
	log.G(ctx).WithField("digest", desc.Digest).
		Warn("registry provider: fetcher does not support seeking; falling back to full-blob buffer")
	data, readErr := io.ReadAll(rc)
	rc.Close()
	if readErr != nil {
		return nil, fmt.Errorf("registry provider: read fallback %s: %w", desc.Digest, readErr)
	}
	return &seekReaderAt{rc: &seekCloser{r: bytes.NewReader(data)}, size: desc.Size}, nil
}

// maxFetchAttempts is the number of times Fetch will retry on transient
// transport errors (e.g. "transport is closing", connection reset) before
// giving up.  Docker Hub closes HTTP/2 connections aggressively on long
// range requests, so retries are needed for large blobs.
const maxFetchAttempts = 5

// isTransientFetchErr returns true for network errors that are safe to retry.
func isTransientFetchErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, substr := range []string{
		"transport is closing",
		"connection reset by peer",
		"connection refused",
		"broken pipe",
		"EOF",
		"use of closed network connection",
		"http2: server sent GOAWAY",
		"stream error",
	} {
		if strings.Contains(msg, substr) {
			return true
		}
	}
	return false
}

// Fetch returns the raw bytes [off, off+length) of blob desc via an HTTP
// Range request.  The caller decides what those bytes mean (one chunk's
// compressed frame, a coalesced multi-chunk range, the chunk-index
// trailer, etc.) — the provider only knows about byte offsets.
//
// A fresh fetcher invocation is made for each call so the HTTP request
// uses the caller's ctx.  Transient transport errors are retried up to
// maxFetchAttempts times with exponential back-off.
func (p *Provider) Fetch(
	ctx context.Context,
	desc ocispec.Descriptor,
	off, length int64,
	priority contentindex.Priority,
) (io.ReadCloser, error) {
	usedReserve, err := p.acquire(ctx, priority)
	if err != nil {
		return nil, err
	}
	defer p.release(usedReserve)

	if length <= 0 || off < 0 || off+length > desc.Size {
		return nil, fmt.Errorf("registry provider: range [%d,%d) out of bounds for blob %s (size %d)",
			off, off+length, desc.Digest, desc.Size)
	}

	var lastErr error
	for attempt := 0; attempt < maxFetchAttempts; attempt++ {
		if attempt > 0 {
			// Exponential back-off: 500ms, 1s, 2s, 4s.
			delay := time.Duration(500<<uint(attempt-1)) * time.Millisecond
			log.G(ctx).WithFields(log.Fields{
				"attempt": attempt + 1,
				"delay":   delay,
				"error":   lastErr,
				"range":   fmt.Sprintf("[%d,%d)", off, off+length),
			}).Warn("registry provider: transient error fetching range, retrying")
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		buf, err := p.fetchOnce(ctx, desc, off, length)
		if err == nil {
			return io.NopCloser(bytes.NewReader(buf)), nil
		}
		if !isTransientFetchErr(err) {
			return nil, err
		}
		lastErr = err
	}
	return nil, fmt.Errorf("registry provider: range [%d,%d) of %s failed after %d attempts: %w",
		off, off+length, desc.Digest, maxFetchAttempts, lastErr)
}

func (p *Provider) fetchOnce(ctx context.Context, desc ocispec.Descriptor, off, length int64) ([]byte, error) {
	rc, err := p.fetcher.Fetch(ctx, desc)
	if err != nil {
		return nil, fmt.Errorf("registry provider: fetch %s: %w", desc.Digest, err)
	}
	defer rc.Close()

	var seeker io.ReadSeeker
	if rsc, ok := rc.(io.ReadSeekCloser); ok {
		seeker = rsc
	} else {
		log.G(ctx).WithField("digest", desc.Digest).
			Warn("registry provider: fetcher does not support seeking; falling back to full-blob buffer")
		data, readErr := io.ReadAll(rc)
		if readErr != nil {
			return nil, fmt.Errorf("registry provider: read fallback %s: %w", desc.Digest, readErr)
		}
		seeker = bytes.NewReader(data)
	}

	if _, err := seeker.Seek(off, io.SeekStart); err != nil {
		return nil, fmt.Errorf("registry provider: seek to %d of %s: %w", off, desc.Digest, err)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(seeker, buf); err != nil {
		return nil, fmt.Errorf("registry provider: read range [%d,%d) of %s: %w",
			off, off+length, desc.Digest, err)
	}
	return buf, nil
}

// DropBuffer is a no-op; this provider does not cache blob buffers between
// calls.  Each Fetch creates a fresh seeker.
func (p *Provider) DropBuffer(dgst digest.Digest) {}

// acquire waits for a concurrency slot appropriate for priority.  Returns
// usedReserve=true when the foreground caller drew from fgReserve (because
// the shared pool was full); the corresponding release must release from
// the same pool to keep the semaphore counts balanced.
//
// Foreground: try shared pool first; if full, draw from fgReserve.
// Background: wait on shared pool only.
func (p *Provider) acquire(ctx context.Context, priority contentindex.Priority) (usedReserve bool, err error) {
	if priority == contentindex.PriorityForeground {
		if p.shared.TryAcquire(1) {
			return false, nil
		}
		if err := p.fgReserve.Acquire(ctx, 1); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, p.shared.Acquire(ctx, 1)
}

// release returns a slot to the pool it was drawn from.  Releasing to the
// wrong pool would over-fill one semaphore (golang.org/x/sync/semaphore
// panics with "semaphore: released more than held") and slowly leak slots
// from the other.
func (p *Provider) release(usedReserve bool) {
	if usedReserve {
		p.fgReserve.Release(1)
		return
	}
	p.shared.Release(1)
}

// ── helpers ───────────────────────────────────────────────────────────────────

// seekReaderAt implements content.ReaderAt over a ReadSeekCloser.
// ReadAt acquires the per-ReaderAt mutex, seeks to off, reads len(p)
// bytes, then releases.  Close closes the underlying ReadSeekCloser.
type seekReaderAt struct {
	mu   sync.Mutex
	rc   io.ReadSeekCloser
	size int64
}

func (r *seekReaderAt) ReadAt(p []byte, off int64) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.rc.Seek(off, io.SeekStart); err != nil {
		return 0, fmt.Errorf("registry provider: seek to %d: %w", off, err)
	}
	return io.ReadFull(r.rc, p)
}

func (r *seekReaderAt) Size() int64  { return r.size }
func (r *seekReaderAt) Close() error { return r.rc.Close() }

// seekCloser wraps bytes.Reader to satisfy io.ReadSeekCloser for the fallback
// non-seekable case.
type seekCloser struct {
	r *bytes.Reader
}

func (s *seekCloser) Read(p []byte) (int, error)           { return s.r.Read(p) }
func (s *seekCloser) Seek(off int64, w int) (int64, error) { return s.r.Seek(off, w) }
func (s *seekCloser) Close() error                         { return nil }

// Ensure interface compliance.
var (
	_ contentindex.ByteProvider = (*Provider)(nil)
	_ content.ReaderAt          = (*seekReaderAt)(nil)
	_ io.ReadSeekCloser         = (*seekCloser)(nil)
)
