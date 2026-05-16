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

package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	bolt "go.etcd.io/bbolt"
)

// Writer initiates an ingest of a complete indexed-content blob.
//
// The producer streams the blob bytes to the returned content.Writer.
// On Commit:
//  1. The streamed digest is verified against the descriptor digest
//     supplied via content.WithDescriptor.
//  2. The chunk index is located and parsed using the descriptor's
//     org.erofs.index.* annotations.
//  3. The chunk-index payload is ingested into the content store under
//     its SHA-256 hash (= org.erofs.index.digest), and verified when
//     org.erofs.index.digest is present.
//  4. Each chunk is sliced from the staging file and ingested into the
//     content store under its per-chunk hash.
//  5. Extra byte ranges (the skippable-frame header for +zstd layers,
//     any zero-padding, and the chunk-index payload itself) are
//     identified, zstd-compressed, and stored inline (when compressed
//     size < inlineThreshold) or as content-store entries.
//  6. The sidecar record is written: scalar metadata, ordered chunk-digest
//     list (for GC), and extras list (for byte-exact reproduction).
//
// The descriptor passed via content.WithDescriptor is required and must
// carry the org.erofs.index.range annotation. Per-chunk checksums
// (HashAlgo != 0) are required so every chunk has a content-store digest.
func (s *Store) Writer(ctx context.Context, opts ...content.WriterOpt) (content.Writer, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}
	var wOpts content.WriterOpts
	for _, opt := range opts {
		if err := opt(&wOpts); err != nil {
			return nil, err
		}
	}
	if wOpts.Ref == "" {
		return nil, fmt.Errorf("content/index: ref must not be empty: %w", errdefs.ErrInvalidArgument)
	}
	if wOpts.Desc.Digest == "" {
		return nil, fmt.Errorf("content/index: descriptor digest required: %w", errdefs.ErrInvalidArgument)
	}
	if _, err := parseIndexLocation(wOpts.Desc); err != nil {
		return nil, err
	}

	s.mu.Lock()
	if _, ok := s.ingests[wOpts.Ref]; ok {
		s.mu.Unlock()
		return nil, fmt.Errorf("content/index: ingest %q in progress: %w", wOpts.Ref, errdefs.ErrUnavailable)
	}
	s.ingests[wOpts.Ref] = &ingestState{
		ref:       wOpts.Ref,
		desc:      wOpts.Desc,
		startedAt: time.Now().UTC(),
	}
	s.mu.Unlock()

	scratchPath := filepath.Join(s.cfg.Root, "ingest", refToFilename(wOpts.Ref))
	f, err := os.OpenFile(scratchPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		s.releaseRef(wOpts.Ref)
		return nil, fmt.Errorf("content/index: open scratch: %w", err)
	}
	return &writer{
		store:     s,
		ns:        ns,
		ref:       wOpts.Ref,
		desc:      wOpts.Desc,
		scratch:   f,
		path:      scratchPath,
		digester:  digest.Canonical.Digester(),
		startedAt: time.Now().UTC(),
	}, nil
}

func (s *Store) releaseRef(ref string) {
	s.mu.Lock()
	delete(s.ingests, ref)
	s.mu.Unlock()
}

// writer implements content.Writer for an indexed-content ingest.
type writer struct {
	store     *Store
	ns        string
	ref       string
	desc      ocispec.Descriptor
	scratch   *os.File
	path      string
	digester  digest.Digester
	written   int64
	startedAt time.Time
	committed bool
	closed    bool
}

func (w *writer) Write(p []byte) (int, error) {
	if w.closed {
		return 0, fmt.Errorf("content/index: writer closed")
	}
	n, err := w.scratch.Write(p)
	if n > 0 {
		w.digester.Hash().Write(p[:n])
		w.written += int64(n)
	}
	return n, err
}

func (w *writer) Digest() digest.Digest { return w.digester.Digest() }

func (w *writer) Status() (content.Status, error) {
	return content.Status{
		Ref:       w.ref,
		Offset:    w.written,
		Total:     w.desc.Size,
		Expected:  w.desc.Digest,
		StartedAt: w.startedAt,
		UpdatedAt: time.Now().UTC(),
	}, nil
}

func (w *writer) Truncate(size int64) error {
	if size != 0 {
		return errors.New("content/index: Truncate supports only size=0")
	}
	if err := w.scratch.Truncate(0); err != nil {
		return err
	}
	if _, err := w.scratch.Seek(0, io.SeekStart); err != nil {
		return err
	}
	w.written = 0
	w.digester = digest.Canonical.Digester()
	return nil
}

// Close releases the writer without committing.
func (w *writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	defer w.store.releaseRef(w.ref)
	if w.scratch != nil {
		_ = w.scratch.Close()
	}
	if !w.committed && w.path != "" {
		_ = os.Remove(w.path)
	}
	return nil
}

// Commit verifies the blob, extracts all content-store entries and extras,
// and writes the sidecar record.  Always closes the writer, even on error.
func (w *writer) Commit(ctx context.Context, size int64, expected digest.Digest, opts ...content.Opt) error {
	defer w.Close()
	if w.closed {
		return errors.New("content/index: writer already closed")
	}
	if err := w.scratch.Sync(); err != nil {
		return err
	}

	// ── 1. Verify blob digest ────────────────────────────────────────────
	actual := w.digester.Digest()
	if expected == "" {
		expected = w.desc.Digest
	}
	if size > 0 && size != w.written {
		return fmt.Errorf("content/index: ingest size %d != expected %d: %w", w.written, size, errdefs.ErrFailedPrecondition)
	}
	if expected != "" && expected != actual {
		return fmt.Errorf("content/index: ingest digest %s != expected %s: %w", actual, expected, errdefs.ErrFailedPrecondition)
	}
	blobSize := w.written

	desc := w.desc
	desc.Digest = actual
	if desc.Size == 0 {
		desc.Size = blobSize
	}

	// ── 2. Parse chunk-index location and chunk entries ──────────────────
	loc, err := parseIndexLocation(desc)
	if err != nil {
		return err
	}
	// Resolve "to end of blob" if loc.End was omitted.
	if loc.End == 0 {
		loc.End = blobSize
	}

	chunks, hdr, err := parseChunkIndex(w.scratch, loc, desc.MediaType)
	if err != nil {
		return err
	}
	if hdr.HashAlgo == chunkIndexHashAlgoNone {
		return fmt.Errorf("content/index: chunk index has no per-chunk checksums (HashAlgo=0); ingest requires hashed chunk indexes: %w", errdefs.ErrFailedPrecondition)
	}

	// Validate dm-verity annotations if present (not stored, but check format).
	if _, err := parseDmVerity(desc); err != nil {
		return err
	}

	// ── 3. Ingest the chunk-index payload as a content-store entry ───────
	// The payload is the raw bytes at [loc.HeaderOffset, loc.End).
	indexPayload := make([]byte, loc.End-loc.HeaderOffset)
	if _, err := w.scratch.ReadAt(indexPayload, loc.HeaderOffset); err != nil {
		return fmt.Errorf("content/index: read chunk-index payload: %w", err)
	}
	indexDigest, err := w.ingestBytes(ctx, indexPayload, loc.Digest, "chunk-index")
	if err != nil {
		return err
	}

	// ── 4. Extract chunks ────────────────────────────────────────────────
	chunkDigests, err := w.extractChunks(ctx, chunks)
	if err != nil {
		return err
	}

	// ── 5. Build extras ──────────────────────────────────────────────────
	// Extras cover every byte range in [0, blobSize) that is NOT covered
	// by a chunk. In practice for a +zstd blob:
	//   • any padding before first chunk (rare)
	//   • any inter-chunk gaps (should be none per spec)
	//   • zero-padding between last chunk and skippable frame (0-3 bytes)
	//   • the 8-byte zstd skippable-frame header (for +zstd layers)
	//   • the chunk-index payload itself (same bytes as step 3 above)
	extras, err := w.buildExtras(ctx, chunks, loc, blobSize, indexDigest, desc.MediaType)
	if err != nil {
		return err
	}

	// ── 6. Write sidecar record ──────────────────────────────────────────
	now := time.Now().UTC()
	m := blobMeta{
		Size:        blobSize,
		MediaType:   desc.MediaType,
		IndexDigest: indexDigest,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	infoLabels := extractOptLabels(opts...)

	return w.store.db.Update(func(tx *bolt.Tx) error {
		blobBkt, err := createBlobBucket(tx, w.ns, actual)
		if err != nil {
			return err
		}
		// Reject duplicate ingest.
		if blobBkt.Get(bucketKeyIndex) != nil {
			return fmt.Errorf("content/index: blob %s: %w", actual, errdefs.ErrAlreadyExists)
		}
		if err := writeBlobMeta(blobBkt, m); err != nil {
			return err
		}
		if err := writeChunkDigests(blobBkt, chunkDigests); err != nil {
			return err
		}
		if err := writeExtras(blobBkt, extras); err != nil {
			return err
		}
		if err := writeLabels(blobBkt, infoLabels); err != nil {
			return err
		}
		return nil
	})
}

// ── Ingest helpers ────────────────────────────────────────────────────────────

// ingestBytes writes data into the content store under its SHA-256 digest.
// If expected is non-empty it is verified against the computed digest.
// refSuffix is used in the ingest ref for debugging.
func (w *writer) ingestBytes(ctx context.Context, data []byte, expected digest.Digest, refSuffix string) (digest.Digest, error) {
	// Compute digest of data.
	h := digest.Canonical.Digester()
	h.Hash().Write(data)
	dgst := h.Digest()

	if expected != "" && expected != dgst {
		return "", fmt.Errorf("content/index: %s digest mismatch: got %s want %s: %w", refSuffix, dgst, expected, errdefs.ErrFailedPrecondition)
	}

	// Quick path: already present.
	if _, err := w.store.cfg.Content.Info(ctx, dgst); err == nil {
		return dgst, nil
	} else if !errdefs.IsNotFound(err) {
		return "", err
	}

	cw, err := w.store.cfg.Content.Writer(ctx,
		content.WithRef(fmt.Sprintf("content-index-%s-%s", refSuffix, dgst)),
		content.WithDescriptor(ocispec.Descriptor{
			MediaType: "application/octet-stream",
			Digest:    dgst,
			Size:      int64(len(data)),
		}),
	)
	if err != nil {
		if errdefs.IsAlreadyExists(err) {
			return dgst, nil
		}
		return "", fmt.Errorf("content/index: open %s writer: %w", refSuffix, err)
	}
	committed := false
	defer func() {
		if !committed {
			cw.Close()
		}
	}()
	if _, err := cw.Write(data); err != nil {
		return "", fmt.Errorf("content/index: write %s: %w", refSuffix, err)
	}
	if err := cw.Commit(ctx, int64(len(data)), dgst); err != nil {
		if errdefs.IsAlreadyExists(err) {
			committed = true
			return dgst, nil
		}
		return "", fmt.Errorf("content/index: commit %s: %w", refSuffix, err)
	}
	committed = true
	return dgst, nil
}

// extractChunks slices each chunk from the scratch file and ingests it into
// the content store under its per-chunk hash.  Returns the ordered list of
// chunk digests.
func (w *writer) extractChunks(ctx context.Context, chunks []contentindex.ChunkRef) ([]digest.Digest, error) {
	dgsts := make([]digest.Digest, len(chunks))
	for i, c := range chunks {
		if c.Digest == "" {
			return nil, fmt.Errorf("content/index: chunk %d missing per-chunk digest", i)
		}
		dgsts[i] = c.Digest
		if c.OnBlobEnd <= c.OnBlobStart {
			continue
		}
		// Quick path: already present.
		if _, err := w.store.cfg.Content.Info(ctx, c.Digest); err == nil {
			continue
		} else if !errdefs.IsNotFound(err) {
			return nil, err
		}
		if err := w.ingestChunk(ctx, &chunks[i], i); err != nil {
			return nil, err
		}
	}
	return dgsts, nil
}

func (w *writer) ingestChunk(ctx context.Context, c *contentindex.ChunkRef, i int) error {
	chunkLen := c.OnBlobEnd - c.OnBlobStart
	cw, err := w.store.cfg.Content.Writer(ctx,
		content.WithRef(fmt.Sprintf("content-index-chunk-%s-%d", c.Digest, i)),
		content.WithDescriptor(ocispec.Descriptor{
			MediaType: "application/octet-stream",
			Digest:    c.Digest,
			Size:      chunkLen,
		}),
	)
	if err != nil {
		if errdefs.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("content/index: open chunk %d writer: %w", i, err)
	}
	committed := false
	defer func() {
		if !committed {
			cw.Close()
		}
	}()
	sr := io.NewSectionReader(w.scratch, c.OnBlobStart, chunkLen)
	if _, err := io.Copy(cw, sr); err != nil {
		return fmt.Errorf("content/index: copy chunk %d: %w", i, err)
	}
	if err := cw.Commit(ctx, chunkLen, c.Digest); err != nil {
		if errdefs.IsAlreadyExists(err) {
			committed = true
			return nil
		}
		return fmt.Errorf("content/index: commit chunk %d: %w", i, err)
	}
	committed = true
	return nil
}

// buildExtras identifies every byte range not covered by a chunk, compresses
// each range, and stores it inline or in the content store.
//
// For a +zstd layer the typical extras are:
//   - any zero-padding between last chunk and the skippable frame
//   - the 8-byte skippable-frame header               (kind="frame")
//   - the chunk-index payload                         (kind="index",
//     references the same content-store entry as indexDigest)
//
// For a raw layer the typical extra is:
//   - the raw chunk-index payload appended after the image data (kind="index")
//
// Any other gap is stored as kind="hole".
func (w *writer) buildExtras(
	ctx context.Context,
	chunks []contentindex.ChunkRef,
	loc *indexLocation,
	blobSize int64,
	indexDigest digest.Digest,
	mediaType string,
) ([]extra, error) {
	// Build a set of covered ranges from chunks.
	type rng struct{ start, end int64 }
	covered := make([]rng, 0, len(chunks))
	for _, c := range chunks {
		if c.OnBlobEnd > c.OnBlobStart {
			covered = append(covered, rng{c.OnBlobStart, c.OnBlobEnd})
		}
	}
	sort.Slice(covered, func(i, j int) bool { return covered[i].start < covered[j].start })

	// Find uncovered gaps in [0, blobSize).
	type gap struct {
		start, end int64
		kind       extraKind
	}
	var gaps []gap
	pos := int64(0)
	for _, r := range covered {
		if r.start > pos {
			gaps = append(gaps, gap{pos, r.start, extraKindHole})
		}
		pos = r.end
	}
	if pos < blobSize {
		gaps = append(gaps, gap{pos, blobSize, extraKindHole})
	}

	// Classify gaps that fall within the chunk-index section.
	// For +zstd:  [loc.Offset, loc.Offset+8) = frame header, [loc.HeaderOffset, loc.End) = index payload
	// For raw:    [loc.Offset, loc.End) = index payload (no frame header)
	zstd := isZstdMediaType(mediaType)
	classifyGap := func(g gap) gap {
		if zstd {
			// The 8-byte skippable-frame header sits at [loc.Offset, loc.HeaderOffset).
			if g.start == loc.Offset && g.end == loc.HeaderOffset {
				g.kind = extraKindFrame
				return g
			}
		}
		// Padding between last chunk and the chunk-index section start.
		if g.end <= loc.Offset {
			if allZeros(w.scratch, g.start, g.end) {
				g.kind = extraKindPadding
			}
			return g
		}
		// The chunk-index payload (or a range that exactly covers it).
		if g.start == loc.HeaderOffset && g.end == loc.End {
			g.kind = extraKindIndex
			return g
		}
		return g
	}

	var extras []extra
	for _, g := range gaps {
		g = classifyGap(g)
		length := g.end - g.start

		// The chunk-index payload is already ingested; reference the
		// same content-store entry rather than re-ingesting.
		if g.kind == extraKindIndex {
			extras = append(extras, extra{
				Offset: g.start,
				Length: length,
				Kind:   extraKindIndex,
				Digest: indexDigest,
			})
			continue
		}

		// Read the raw bytes for this gap.
		raw := make([]byte, length)
		if _, err := w.scratch.ReadAt(raw, g.start); err != nil {
			return nil, fmt.Errorf("content/index: read extra [%d,%d): %w", g.start, g.end, err)
		}

		// Compress with zstd.
		compressed, err := compressBytes(raw)
		if err != nil {
			return nil, fmt.Errorf("content/index: compress extra [%d,%d): %w", g.start, g.end, err)
		}

		if len(compressed) < inlineThreshold {
			// Store inline.
			extras = append(extras, extra{
				Offset: g.start,
				Length: length,
				Kind:   g.kind,
				Inline: compressed,
			})
		} else {
			// Ingest into content store.
			exDgst, err := w.ingestBytes(ctx, compressed, "", fmt.Sprintf("extra-%d", g.start))
			if err != nil {
				return nil, err
			}
			extras = append(extras, extra{
				Offset: g.start,
				Length: length,
				Kind:   g.kind,
				Digest: exDgst,
			})
		}
	}
	return extras, nil
}

// ── Compression helpers ───────────────────────────────────────────────────────

var zstdEncoder, _ = zstd.NewWriter(nil)

// compressBytes zstd-compresses src and returns the compressed bytes.
func compressBytes(src []byte) ([]byte, error) {
	return zstdEncoder.EncodeAll(src, nil), nil
}

// ── Misc helpers ──────────────────────────────────────────────────────────────

// allZeros reports whether the bytes at [start, end) in r are all zero.
func allZeros(r io.ReaderAt, start, end int64) bool {
	if end <= start {
		return true
	}
	buf := make([]byte, end-start)
	n, err := r.ReadAt(buf, start)
	if err != nil && err != io.EOF {
		return false
	}
	for _, b := range buf[:n] {
		if b != 0 {
			return false
		}
	}
	return true
}

// extractOptLabels applies content.Opt's to a temporary shell and returns the labels.
func extractOptLabels(opts ...content.Opt) map[string]string {
	var info content.Info
	for _, opt := range opts {
		_ = opt(&info)
	}
	return info.Labels
}

// refToFilename produces a filesystem-safe scratch filename from an ingest ref.
func refToFilename(ref string) string {
	const safe = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-._"
	out := make([]byte, 0, len(ref))
	for i := 0; i < len(ref); i++ {
		c := ref[i]
		if indexByte(safe, c) >= 0 {
			out = append(out, c)
		} else {
			out = append(out, '_')
		}
	}
	return string(out)
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}
