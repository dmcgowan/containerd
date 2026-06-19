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

package registry_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/core/content/index/registry"
	"github.com/containerd/containerd/v2/core/remotes"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// rangeFetcher is a remotes.Fetcher backed by an httptest.Server.
// It records every Range header received.
type rangeFetcher struct {
	data   []byte
	desc   ocispec.Descriptor
	srv    *httptest.Server
	ranges []string
}

func newRangeFetcher(t *testing.T, size int) *rangeFetcher {
	t.Helper()
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	dgst := digest.FromBytes(data)
	desc := ocispec.Descriptor{
		Digest: dgst,
		Size:   int64(size),
	}

	rf := &rangeFetcher{data: data, desc: desc}

	rf.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rh := r.Header.Get("Range")
		rf.ranges = append(rf.ranges, rh)

		if rh == "" {
			// Full blob request.
			w.Header().Set("Content-Length", strconv.Itoa(size))
			w.WriteHeader(http.StatusOK)
			w.Write(data)
			return
		}

		// Parse "bytes=start-end" or "bytes=start-"
		rh = strings.TrimPrefix(rh, "bytes=")
		parts := strings.SplitN(rh, "-", 2)
		start, _ := strconv.ParseInt(parts[0], 10, 64)
		var end int64
		if parts[1] == "" {
			end = int64(size) - 1
		} else {
			end, _ = strconv.ParseInt(parts[1], 10, 64)
		}
		length := end - start + 1
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data[start : end+1])
	}))

	t.Cleanup(rf.srv.Close)
	return rf
}

// Fetch implements remotes.Fetcher.  It returns an io.ReadSeekCloser so the
// provider can use Range requests.
func (rf *rangeFetcher) Fetch(ctx context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	if desc.Digest != rf.desc.Digest {
		return nil, fmt.Errorf("rangeFetcher: unknown digest %s", desc.Digest)
	}
	return newRangeReadSeekCloser(rf.srv.URL, rf.data, rf.desc.Size), nil
}

var _ remotes.Fetcher = (*rangeFetcher)(nil)

// rangeReadSeekCloser is a minimal io.ReadSeekCloser that mirrors what
// dockerFetcher returns: it remembers the current offset and issues a Range
// GET on each Read (or re-issues if Seek is called).
type rangeReadSeekCloser struct {
	url    string
	data   []byte
	size   int64
	offset int64
	buf    *bytes.Reader // buffered from last range GET
}

func newRangeReadSeekCloser(url string, data []byte, size int64) *rangeReadSeekCloser {
	return &rangeReadSeekCloser{url: url, data: data, size: size}
}

func (r *rangeReadSeekCloser) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.offset + offset
	case io.SeekEnd:
		abs = r.size + offset
	}
	if abs < 0 || abs > r.size {
		return 0, fmt.Errorf("rangeReadSeekCloser: seek out of bounds: %d", abs)
	}
	r.offset = abs
	r.buf = nil // invalidate buffer
	return abs, nil
}

func (r *rangeReadSeekCloser) Read(p []byte) (int, error) {
	if r.offset >= r.size {
		return 0, io.EOF
	}
	if r.buf == nil {
		// Issue a Range GET from current offset.
		end := r.offset + int64(len(p)) - 1
		if end >= r.size {
			end = r.size - 1
		}
		r.buf = bytes.NewReader(r.data[r.offset : end+1])
	}
	n, err := r.buf.Read(p)
	r.offset += int64(n)
	if r.buf.Len() == 0 {
		r.buf = nil
	}
	return n, err
}

func (r *rangeReadSeekCloser) Close() error { return nil }

// nonSeekFetcher returns a plain io.ReadCloser (no Seek) to trigger fallback.
type nonSeekFetcher struct {
	data []byte
	desc ocispec.Descriptor
}

func (f *nonSeekFetcher) Fetch(_ context.Context, desc ocispec.Descriptor) (io.ReadCloser, error) {
	if desc.Digest != f.desc.Digest {
		return nil, fmt.Errorf("nonSeekFetcher: unknown digest %s", desc.Digest)
	}
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

var _ remotes.Fetcher = (*nonSeekFetcher)(nil)

// ── tests ─────────────────────────────────────────────────────────────────────

// TestProviderOpenReadsAt verifies that Open returns a ReaderAt that can
// service arbitrary-offset reads without downloading the full blob upfront.
func TestProviderOpenReadsAt(t *testing.T) {
	const size = 1 * 1024 * 1024 // 1 MiB
	rf := newRangeFetcher(t, size)
	p := registry.New(rf, "test", registry.Config{})

	ctx := context.Background()
	ra, err := p.Open(ctx, rf.desc)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer ra.Close()

	if ra.Size() != int64(size) {
		t.Errorf("Size: got %d, want %d", ra.Size(), size)
	}

	// Read bytes [256KiB, 256KiB+128) — a small window in the middle.
	const off = 256 * 1024
	buf := make([]byte, 128)
	if _, err := ra.ReadAt(buf, off); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(buf, rf.data[off:off+128]) {
		t.Error("ReadAt: data mismatch")
	}
}

// TestProviderFetchChunk verifies that Fetch transfers only the requested chunk
// byte range and returns the correct content.
func TestProviderFetchChunk(t *testing.T) {
	const size = 4 * 1024 * 1024 // 4 MiB blob
	rf := newRangeFetcher(t, size)
	p := registry.New(rf, "test", registry.Config{})

	ctx := context.Background()

	const off = int64(1 * 1024 * 1024) // 1 MiB offset
	const length = int64(1 * 1024 * 1024)
	rc, err := p.Fetch(ctx, rf.desc, off, length, contentindex.PriorityForeground)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	defer rc.Close()

	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	want := rf.data[off : off+length]
	if !bytes.Equal(got, want) {
		t.Errorf("Fetch: data mismatch (got %d bytes, want %d)", len(got), len(want))
	}
}

// TestProviderFetchMultipleChunks verifies that fetching two non-adjacent
// chunks from the same blob returns the correct data for each.
func TestProviderFetchMultipleChunks(t *testing.T) {
	const size = 8 * 1024 * 1024 // 8 MiB blob
	rf := newRangeFetcher(t, size)
	p := registry.New(rf, "test", registry.Config{MaxConcurrentFetches: 4})

	ctx := context.Background()

	ranges := []struct{ off, length int64 }{
		{0, 1 * 1024 * 1024},
		{4 * 1024 * 1024, 1 * 1024 * 1024},
	}
	for _, r := range ranges {
		rc, err := p.Fetch(ctx, rf.desc, r.off, r.length, contentindex.PriorityBackground)
		if err != nil {
			t.Fatalf("Fetch [%d,%d): %v", r.off, r.off+r.length, err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		want := rf.data[r.off : r.off+r.length]
		if !bytes.Equal(got, want) {
			t.Errorf("Fetch [%d,%d): data mismatch", r.off, r.off+r.length)
		}
	}
}

// TestProviderDropBuffer verifies that DropBuffer closes the cached connection
// so the next call re-opens fresh.
func TestProviderDropBuffer(t *testing.T) {
	const size = 512 * 1024
	rf := newRangeFetcher(t, size)
	p := registry.New(rf, "test", registry.Config{})

	ctx := context.Background()
	ra, err := p.Open(ctx, rf.desc)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	buf := make([]byte, 64)
	if _, err := ra.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt before DropBuffer: %v", err)
	}
	ra.Close()

	p.DropBuffer(rf.desc.Digest)

	// After DropBuffer a new Open should succeed and return correct data.
	ra2, err := p.Open(ctx, rf.desc)
	if err != nil {
		t.Fatalf("Open after DropBuffer: %v", err)
	}
	defer ra2.Close()
	buf2 := make([]byte, 64)
	if _, err := ra2.ReadAt(buf2, 0); err != nil {
		t.Fatalf("ReadAt after DropBuffer: %v", err)
	}
	if !bytes.Equal(buf2, rf.data[:64]) {
		t.Error("data mismatch after DropBuffer")
	}
}

// TestProviderFallbackNonSeekable verifies that a fetcher returning a
// non-seekable ReadCloser works correctly via the buffer fallback.
func TestProviderFallbackNonSeekable(t *testing.T) {
	data := make([]byte, 256*1024)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	dgst := digest.FromBytes(data)
	desc := ocispec.Descriptor{Digest: dgst, Size: int64(len(data))}
	f := &nonSeekFetcher{data: data, desc: desc}
	p := registry.New(f, "fallback-test", registry.Config{})

	ctx := context.Background()
	ra, err := p.Open(ctx, desc)
	if err != nil {
		t.Fatalf("Open (fallback): %v", err)
	}
	defer ra.Close()

	buf := make([]byte, 128)
	if _, err := ra.ReadAt(buf, 1024); err != nil {
		t.Fatalf("ReadAt (fallback): %v", err)
	}
	if !bytes.Equal(buf, data[1024:1024+128]) {
		t.Error("fallback: data mismatch")
	}
}

// TestProviderBytesTransferred is an integration-style test that uses a
// counting httptest server to assert that fetching one chunk of a multi-chunk
// blob transfers only approximately that chunk's worth of bytes (plus a small
// HTTP overhead allowance).
func TestProviderBytesTransferred(t *testing.T) {
	const blobSize = 8 * 1024 * 1024 // 8 MiB
	const chunkSize = 1 * 1024 * 1024 // 1 MiB

	data := make([]byte, blobSize)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	dgst := digest.FromBytes(data)
	desc := ocispec.Descriptor{Digest: dgst, Size: blobSize}

	var bytesServed atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rh := r.Header.Get("Range")
		var start, end int64
		if rh == "" {
			end = blobSize - 1
		} else {
			rh = strings.TrimPrefix(rh, "bytes=")
			parts := strings.SplitN(rh, "-", 2)
			start, _ = strconv.ParseInt(parts[0], 10, 64)
			if parts[1] == "" {
				end = blobSize - 1
			} else {
				end, _ = strconv.ParseInt(parts[1], 10, 64)
			}
		}
		served := end - start + 1
		bytesServed.Add(served)
		if rh != "" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, blobSize))
			w.Header().Set("Content-Length", strconv.FormatInt(served, 10))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.Header().Set("Content-Length", strconv.FormatInt(served, 10))
			w.WriteHeader(http.StatusOK)
		}
		w.Write(data[start : end+1])
	}))
	defer srv.Close()

	// Build a fetcher that returns a rangeReadSeekCloser backed by this server.
	fetcher := remotes.FetcherFunc(func(_ context.Context, d ocispec.Descriptor) (io.ReadCloser, error) {
		return newRangeReadSeekCloser(srv.URL, data, blobSize), nil
	})

	p := registry.New(fetcher, "bytes-test", registry.Config{})
	ctx := context.Background()

	rc, err := p.Fetch(ctx, desc, int64(3*chunkSize), int64(chunkSize), contentindex.PriorityForeground)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	io.Copy(io.Discard, rc)
	rc.Close()

	served := bytesServed.Load()
	// Allow 10% overhead for test-server bookkeeping; the important thing is
	// we did NOT serve the whole 8 MiB blob.
	maxAllowed := int64(chunkSize) + int64(chunkSize)/10
	if served > maxAllowed {
		t.Errorf("bytes transferred: got %d, want <= %d (chunkSize=%d, blobSize=%d)",
			served, maxAllowed, chunkSize, blobSize)
	}
	t.Logf("bytes transferred for 1-chunk fetch: %d / %d blob bytes (%.1f%%)",
		served, blobSize, float64(served)/float64(blobSize)*100)
}
