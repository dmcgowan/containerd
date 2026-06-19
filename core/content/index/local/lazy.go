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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/core/metadata/boltutil"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	bolt "go.etcd.io/bbolt"
)

// ── Lazy write option ──────────────────────────────────────────────────────────

// lazyWriterOpts carries the provider to use for a lazy ingest.
type lazyWriterOpts struct {
	provider contentindex.ByteProvider
}

// lazyWriterOptsKey is used to attach lazyWriterOpts to a content.WriterOpts
// via the Ref field convention. Callers pass the provider by setting a custom
// WriterOpt that stores it in the context or — more simply — via a parallel
// mechanism. We use a lightweight sidecar: callers call WriteLazy directly.
type lazyWriterOptsKey struct{}

// WriteLazy is an alternative entry point to Store.Writer for lazy ingest.
// It fetches only the chunk-index section from provider p, records all
// per-chunk metadata in the store, and returns without fetching chunk bytes.
//
// desc must carry the org.erofs.chunk-index.range annotation.
// ref is the ingest reference (same semantics as content.WriterOpt(WithRef(...))).
// provider is bound to the metadata record and used for subsequent FillChunk
// calls.
func (s *Store) WriteLazy(ctx context.Context, ref string, desc ocispec.Descriptor, p contentindex.ByteProvider) error {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return err
	}


	loc, err := parseIndexLocation(desc)
	if err != nil {
		return fmt.Errorf("content/index: lazy ingest: %w", err)
	}
	if desc.Digest == "" {
		return fmt.Errorf("content/index: lazy ingest: descriptor digest required: %w", errdefs.ErrInvalidArgument)
	}

	// Check if already ingested.
	var alreadyExists bool
	if err := view(ctx, s.db, func(tx *bolt.Tx) error {
		bkt := getBlobBucket(tx, ns, desc.Digest)
		if bkt != nil && bkt.Get(bucketKeyIndex) != nil {
			alreadyExists = true
		}
		return nil
	}); err != nil {
		return err
	}
	if alreadyExists {
		return fmt.Errorf("content/index: blob %s: %w", desc.Digest, errdefs.ErrAlreadyExists)
	}

	// Fetch the chunk-index payload bytes from the provider. We use Open to
	// get a ReaderAt over the full blob, then read just the index range.
	// For large blobs this buffers the whole blob; a future optimisation can
	// use a Range request once providers support it natively.
	ra, err := p.Open(ctx, desc)
	if err != nil {
		return fmt.Errorf("content/index: lazy ingest: open provider: %w", err)
	}
	defer ra.Close()

	// Resolve end-of-index.
	end := loc.End
	if end == 0 {
		end = desc.Size
	}
	indexPayloadSize := end - loc.HeaderOffset
	indexPayload := make([]byte, indexPayloadSize)
	if _, err := ra.ReadAt(indexPayload, loc.HeaderOffset); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("content/index: lazy ingest: read chunk-index: %w", err)
	}

	// Parse chunks from the payload.
	chunks, hdr, err := parseChunkIndexPayload(indexPayload, loc.Offset, desc.MediaType)
	if err != nil {
		return fmt.Errorf("content/index: lazy ingest: parse chunk-index: %w", err)
	}
	if hdr.HashAlgo == chunkIndexHashAlgoNone {
		return fmt.Errorf("content/index: lazy ingest: chunk index has no per-chunk checksums (HashAlgo=0): %w", errdefs.ErrFailedPrecondition)
	}

	// Ingest the chunk-index payload as a content-store entry.
	indexDigest, err := s.ingestIndexPayload(ctx, indexPayload, loc.Digest)
	if err != nil {
		return fmt.Errorf("content/index: lazy ingest: ingest chunk-index entry: %w", err)
	}

	// Collect per-chunk digests (no chunk bytes are fetched).
	chunkDigests := make([]digest.Digest, len(chunks))
	for i, c := range chunks {
		chunkDigests[i] = c.Digest
	}

	// Build the "extras" list so a fully-filled lazy blob can be
	// reassembled byte-for-byte by Store.ReaderAt — i.e. so the
	// resulting bytes hash back to desc.Digest.  Without these
	// entries the assembled-segment reader's coverage stops at the
	// last chunk's end and the chunk-index trailer is missing.
	//
	// Layout decision: store the WHOLE trailer (frame header + index
	// payload + any padding) as ONE zstd-compressed extra, matching
	// the eager Writer path (see writer.go: the gap-finding loop
	// produces a single contiguous gap from the end of the last
	// chunk to blobSize, which is then compressed and ingested as
	// one extraKindHole entry).  That keeps the reader logic
	// uniform: every extra is a zstd-compressed blob whose
	// `decompressedExtra` output is the literal on-blob bytes.
	//
	// The chunk-index payload remains separately addressable in the
	// content store under indexDigest for the WriteLazy/FillChunk
	// fast paths; this extra is just an additional reassembly aid.
	trailerStart := int64(loc.Offset)
	if !isZstdMediaType(desc.MediaType) {
		trailerStart = loc.HeaderOffset
	}
	trailerLen := end - trailerStart
	trailer := make([]byte, trailerLen)
	if _, terr := ra.ReadAt(trailer, trailerStart); terr != nil && !errors.Is(terr, io.EOF) {
		return fmt.Errorf("content/index: lazy ingest: read trailer: %w", terr)
	}
	trailerCompressed, cerr := compressBytes(trailer)
	if cerr != nil {
		return fmt.Errorf("content/index: lazy ingest: compress trailer: %w", cerr)
	}
	trailerExtra := extra{
		Offset: trailerStart,
		Length: trailerLen,
		Kind:   extraKindHole, // matches what the eager-path writer chooses
	}
	if len(trailerCompressed) < inlineThreshold {
		trailerExtra.Inline = trailerCompressed
	} else {
		// Larger compressed trailers go to the content store as
		// their own entry, keyed by the SHA-256 of the compressed
		// bytes (matching the eager writer's extras ingest path).
		exDgst, ierr := s.ingestExtra(ctx, trailerCompressed)
		if ierr != nil {
			return fmt.Errorf("content/index: lazy ingest: ingest trailer extra: %w", ierr)
		}
		trailerExtra.Digest = exDgst
	}
	lazyExtras := []extra{trailerExtra}

	now := time.Now().UTC()
	m := blobMeta{
		Size:             desc.Size,
		UncompressedSize: int64(hdr.UncompressedSize),
		MediaType:        desc.MediaType,
		Provider:         p.Name(),
		IndexDigest:      indexDigest,
		IndexOffset:      loc.Offset,
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	_ = ref // ref is reserved for future dedup of concurrent lazy ingests
	return update(ctx, s.db, func(tx *bolt.Tx) error {
		blobBkt, err := createBlobBucket(tx, ns, desc.Digest)
		if err != nil {
			return err
		}
		if blobBkt.Get(bucketKeyIndex) != nil {
			return fmt.Errorf("content/index: blob %s: %w", desc.Digest, errdefs.ErrAlreadyExists)
		}
		if err := writeBlobMeta(blobBkt, m); err != nil {
			return err
		}
		if err := writeChunkDigests(blobBkt, chunkDigests); err != nil {
			return err
		}
		// Persist the trailer extras so a fully-filled lazy blob
		// can round-trip through Store.ReaderAt and reproduce the
		// original blob bytes exactly.
		if err := writeExtras(blobBkt, lazyExtras); err != nil {
			return err
		}
		// Note: we DO NOT self-root the blob via a label-on-self.  The
		// proper forward edge comes from the manifest's
		// "containerd.io/gc.ref.content-index.l.<i>=<digest>" label
		// (written by core/images.SetChildrenMappedLabels when the layer
		// descriptor carries the org.erofs.chunk-index.range annotation).
		// That way the lazy blob is reaped naturally when the image is
		// removed; no permanent self-root keeps it alive.
		return nil
	})
}

// ingestIndexPayload writes indexPayload into the content store.
func (s *Store) ingestIndexPayload(ctx context.Context, payload []byte, expected digest.Digest) (digest.Digest, error) {
	h := digest.Canonical.Digester()
	h.Hash().Write(payload)
	dgst := h.Digest()
	if expected != "" && expected != dgst {
		return "", fmt.Errorf("content/index: index payload digest mismatch: got %s want %s: %w",
			dgst, expected, errdefs.ErrFailedPrecondition)
	}

	cw, err := s.cfg.Content.Writer(ctx,
		content.WithRef(fmt.Sprintf("content-index-lazy-index-%s", dgst)),
		content.WithDescriptor(ocispec.Descriptor{
			MediaType: "application/octet-stream",
			Digest:    dgst,
			Size:      int64(len(payload)),
		}),
	)
	if err != nil {
		if errdefs.IsAlreadyExists(err) {
			return dgst, nil
		}
		return "", fmt.Errorf("content/index: open index writer: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			cw.Close()
		}
	}()
	// Truncate any stale partial ingest from a previous crashed run so
	// that io.Copy starts from offset 0 and the commit size check passes.
	if err := cw.Truncate(0); err != nil {
		return "", fmt.Errorf("content/index: truncate index writer: %w", err)
	}
	if _, err := io.Copy(cw, bytes.NewReader(payload)); err != nil {
		return "", fmt.Errorf("content/index: write index payload: %w", err)
	}
	if err := cw.Commit(ctx, int64(len(payload)), dgst); err != nil {
		if errdefs.IsAlreadyExists(err) {
			committed = true
			return dgst, nil
		}
		return "", fmt.Errorf("content/index: commit index payload: %w", err)
	}
	committed = true
	return dgst, nil
}

// ingestExtra writes a zstd-compressed extra payload into the content
// store under the SHA-256 of its compressed bytes.  Used by the lazy
// ingest path when the trailer's compressed form exceeds
// inlineThreshold and therefore can't be stored on the metadata
// record.  Idempotent: if the entry already exists, returns its
// digest without re-ingesting.
func (s *Store) ingestExtra(ctx context.Context, compressed []byte) (digest.Digest, error) {
	h := digest.Canonical.Digester()
	h.Hash().Write(compressed)
	dgst := h.Digest()
	if _, err := s.cfg.Content.Info(ctx, dgst); err == nil {
		return dgst, nil
	} else if !errdefs.IsNotFound(err) {
		return "", err
	}
	cw, err := s.cfg.Content.Writer(ctx,
		content.WithRef(fmt.Sprintf("content-index-lazy-extra-%s", dgst)),
		content.WithDescriptor(ocispec.Descriptor{
			MediaType: "application/octet-stream",
			Digest:    dgst,
			Size:      int64(len(compressed)),
		}),
	)
	if err != nil {
		if errdefs.IsAlreadyExists(err) {
			return dgst, nil
		}
		return "", fmt.Errorf("content/index: open extra writer: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			cw.Close()
		}
	}()
	if _, err := cw.Write(compressed); err != nil {
		return "", fmt.Errorf("content/index: write extra: %w", err)
	}
	if err := cw.Commit(ctx, int64(len(compressed)), dgst); err != nil {
		if errdefs.IsAlreadyExists(err) {
			committed = true
			return dgst, nil
		}
		return "", fmt.Errorf("content/index: commit extra: %w", err)
	}
	committed = true
	return dgst, nil
}

// ── AllChunks ─────────────────────────────────────────────────────────────────

// AllChunks returns all ChunkRefs for the blob in chunk-index order, whether
// or not their bytes are present in the content store.
func (s *Store) AllChunks(ctx context.Context, dgst digest.Digest) ([]contentindex.ChunkRef, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}

	var (
		meta        blobMeta
		chunkDigsts []digest.Digest
	)
	if err := view(ctx, s.db, func(tx *bolt.Tx) error {
		blobBkt := getBlobBucket(tx, ns, dgst)
		if blobBkt == nil {
			return blobNotFound(dgst)
		}
		var merr error
		meta, merr = readBlobMeta(blobBkt)
		if merr != nil {
			return merr
		}
		chunkDigsts, merr = readChunkDigests(blobBkt)
		return merr
	}); err != nil {
		return nil, err
	}

	indexPayload, err := s.readContentEntry(ctx, meta.IndexDigest)
	if err != nil {
		return nil, fmt.Errorf("content/index: AllChunks: read chunk-index: %w", err)
	}
	chunks, _, err := parseChunkIndexPayload(indexPayload, meta.IndexOffset, meta.MediaType)
	if err != nil {
		return nil, fmt.Errorf("content/index: AllChunks: parse chunk-index: %w", err)
	}
	// Overlay the stored per-chunk digests (which may differ from what
	// parseChunkIndexPayload computed, since we pass offset=0).
	for i := range chunks {
		if i < len(chunkDigsts) && chunkDigsts[i] != "" {
			chunks[i].Digest = chunkDigsts[i]
		}
	}
	return chunks, nil
}

// ── MissingChunks ─────────────────────────────────────────────────────────────

// MissingChunks returns the ChunkRefs whose bytes are not yet present in the
// content store, in chunk-index order.
func (s *Store) MissingChunks(ctx context.Context, dgst digest.Digest) ([]contentindex.ChunkRef, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}

	// Read per-chunk digest list and the chunk-index entry from the DB.
	var (
		meta        blobMeta
		chunkDigsts []digest.Digest
	)
	if err := view(ctx, s.db, func(tx *bolt.Tx) error {
		blobBkt := getBlobBucket(tx, ns, dgst)
		if blobBkt == nil {
			return blobNotFound(dgst)
		}
		var merr error
		meta, merr = readBlobMeta(blobBkt)
		if merr != nil {
			return merr
		}
		chunkDigsts, merr = readChunkDigests(blobBkt)
		return merr
	}); err != nil {
		return nil, err
	}

	// Load the chunk-index payload to get per-chunk offsets and lengths.
	indexPayload, err := s.readContentEntry(ctx, meta.IndexDigest)
	if err != nil {
		return nil, fmt.Errorf("content/index: MissingChunks: read chunk-index: %w", err)
	}

	chunks, _, err := parseChunkIndexPayload(indexPayload, meta.IndexOffset, meta.MediaType)
	if err != nil {
		return nil, fmt.Errorf("content/index: MissingChunks: parse chunk-index: %w", err)
	}

	var missing []contentindex.ChunkRef
	for i, c := range chunks {
		if i < len(chunkDigsts) {
			c.Digest = chunkDigsts[i]
		}
		if c.Digest == "" {
			missing = append(missing, c)
			continue
		}
		if _, err := s.cfg.Content.Info(ctx, c.Digest); errdefs.IsNotFound(err) {
			missing = append(missing, c)
		} else if err != nil {
			return nil, fmt.Errorf("content/index: MissingChunks: probe chunk %d: %w", i, err)
		}
	}
	return missing, nil
}

// ── FillChunk ─────────────────────────────────────────────────────────────────

// fillGate coalesces concurrent FillChunk calls for the same (blob, chunkIdx).
type fillGate struct {
	ch  chan struct{} // closed when fill completes
	err error        // result of the fill
}

// fillChunkKey identifies a single in-flight fill operation.
type fillChunkKey struct {
	dgst     digest.Digest
	chunkIdx int
}

// FillChunk fetches one chunk through provider p, verifies its hash, and
// writes it to the content store.
func (s *Store) FillChunk(
	ctx context.Context,
	dgst digest.Digest,
	chunkIdx int,
	p contentindex.ByteProvider,
	priority contentindex.Priority,
) error {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return err
	}

	// Load chunk metadata from the DB.
	var (
		meta        blobMeta
		chunkDigsts []digest.Digest
	)
	if err := view(ctx, s.db, func(tx *bolt.Tx) error {
		blobBkt := getBlobBucket(tx, ns, dgst)
		if blobBkt == nil {
			return blobNotFound(dgst)
		}
		var merr error
		meta, merr = readBlobMeta(blobBkt)
		if merr != nil {
			return merr
		}
		chunkDigsts, merr = readChunkDigests(blobBkt)
		return merr
	}); err != nil {
		return err
	}

	if chunkIdx < 0 || chunkIdx >= len(chunkDigsts) {
		return fmt.Errorf("content/index: FillChunk: chunk index %d out of range [0,%d): %w",
			chunkIdx, len(chunkDigsts), errdefs.ErrInvalidArgument)
	}
	chunkDgst := chunkDigsts[chunkIdx]
	if chunkDgst == "" {
		return fmt.Errorf("content/index: FillChunk: chunk %d has no digest", chunkIdx)
	}

	// Quick path: already present.
	if _, err := s.cfg.Content.Info(ctx, chunkDgst); err == nil {
		return nil
	} else if !errdefs.IsNotFound(err) {
		return fmt.Errorf("content/index: FillChunk: info chunk %d: %w", chunkIdx, err)
	}

	// Coalesce concurrent fills for the same (blob, chunk).
	key := fillChunkKey{dgst: dgst, chunkIdx: chunkIdx}
	gate, owner := s.acquireFillGate(key)
	if !owner {
		// Another goroutine is filling this chunk. Wait for it.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-gate.ch:
		}
		return gate.err
	}
	defer s.releaseFillGate(key, gate)

	// Load the chunk-index to get this chunk's on-blob range.
	indexPayload, err := s.readContentEntry(ctx, meta.IndexDigest)
	if err != nil {
		gate.err = fmt.Errorf("content/index: FillChunk: read chunk-index: %w", err)
		return gate.err
	}
	desc := ocispec.Descriptor{
		Digest:    dgst,
		Size:      meta.Size,
		MediaType: meta.MediaType,
	}
	chunks, _, err := parseChunkIndexPayload(indexPayload, meta.IndexOffset, meta.MediaType)
	if err != nil {
		gate.err = fmt.Errorf("content/index: FillChunk: parse chunk-index: %w", err)
		return gate.err
	}
	if chunkIdx >= len(chunks) {
		gate.err = fmt.Errorf("content/index: FillChunk: chunk index %d out of range", chunkIdx)
		return gate.err
	}
	c := chunks[chunkIdx]
	c.Digest = chunkDgst

	// Fetch the chunk bytes from the provider.
	pStr := "fg"
	if priority != contentindex.PriorityForeground {
		pStr = "bg"
	}
	log.G(ctx).WithFields(log.Fields{
		"blob":             dgst,
		"chunk":            chunkIdx,
		"priority":         pStr,
		"range_start":      c.OnBlobStart,
		"range_end":        c.OnBlobEnd,
		"uncompressed_off": c.Offset,
	}).Info("[lazy-viz] chunk_fetch_start")
	rc, err := p.Fetch(ctx, desc, c.OnBlobStart, c.OnBlobEnd-c.OnBlobStart, priority)
	if err != nil {
		gate.err = fmt.Errorf("content/index: FillChunk: fetch chunk %d: %w", chunkIdx, err)
		return gate.err
	}
	defer rc.Close()

	// Read the raw on-blob bytes.  The stored chunk digest is computed
	// over the raw bytes (consistent with how extractChunks stores them in
	// the eager path), so do NOT decompress here.  The cache file holds
	// the original on-blob layout (the kernel's EROFS userspace
	// decompresses on read).
	chunkBytes, err := io.ReadAll(rc)
	if err != nil {
		gate.err = fmt.Errorf("content/index: FillChunk: read chunk %d: %w", chunkIdx, err)
		return gate.err
	}

	// Verify per-chunk hash.
	h := chunkDgst.Algorithm().Digester()
	h.Hash().Write(chunkBytes)
	actual := h.Digest()
	if actual != chunkDgst {
		gate.err = fmt.Errorf("content/index: FillChunk: chunk %d hash mismatch: got %s want %s: %w",
			chunkIdx, actual, chunkDgst, errdefs.ErrFailedPrecondition)
		return gate.err
	}

	// Write to content store.
	cw, err := s.cfg.Content.Writer(ctx,
		content.WithRef(fmt.Sprintf("content-index-fill-%s-%d", dgst, chunkIdx)),
		content.WithDescriptor(ocispec.Descriptor{
			MediaType: "application/octet-stream",
			Digest:    chunkDgst,
			Size:      int64(len(chunkBytes)),
		}),
	)
	if err != nil {
		if errdefs.IsAlreadyExists(err) {
			return nil
		}
		gate.err = fmt.Errorf("content/index: FillChunk: open writer for chunk %d: %w", chunkIdx, err)
		return gate.err
	}
	committed := false
	defer func() {
		if !committed {
			cw.Close()
		}
	}()
	// Truncate any stale partial ingest from a previous failed run.
	// Without this, a previous interrupted write would leave bytes in the
	// in-progress ingest; the next write appends to them, doubling the size
	// and causing the commit size-validation to fail.
	if err := cw.Truncate(0); err != nil {
		gate.err = fmt.Errorf("content/index: FillChunk: truncate writer for chunk %d: %w", chunkIdx, err)
		return gate.err
	}
	if _, err := io.Copy(cw, bytes.NewReader(chunkBytes)); err != nil {
		gate.err = fmt.Errorf("content/index: FillChunk: write chunk %d: %w", chunkIdx, err)
		return gate.err
	}
	if err := cw.Commit(ctx, int64(len(chunkBytes)), chunkDgst); err != nil {
		if errdefs.IsAlreadyExists(err) {
			committed = true
			return nil
		}
		gate.err = fmt.Errorf("content/index: FillChunk: commit chunk %d: %w", chunkIdx, err)
		return gate.err
	}
	committed = true
	log.G(ctx).WithFields(log.Fields{
		"blob":  dgst,
		"chunk": chunkIdx,
		"bytes": len(chunkBytes),
	}).Info("[lazy-viz] chunk_fetch_done")

	// If this was the last missing chunk, purge the provider reconstruction
	// record from boltdb — credentials are no longer needed for restart
	// recovery because the blob is now fully resident in the content store.
	// This is a best-effort operation; failure leaves the record in place
	// where it will be reaped later when the image is removed via GC.
	if meta.Provider != "" {
		s.purgeProviderIfFull(ctx, ns, dgst, meta.Provider, chunkDigsts, chunkIdx)
	}

	return nil
}

// purgeProviderIfFull deletes the provider reconstruction record for blob dgst
// when every chunk digest in chunkDigests is now present in the content store.
// chunkJustFilled is the index of the chunk that was just committed — it is
// guaranteed present so we skip its probe.
//
// Called after a successful FillChunk commit; errors are logged but do not
// surface to the caller (the fill itself succeeded).
func (s *Store) purgeProviderIfFull(
	ctx context.Context,
	ns string,
	dgst digest.Digest,
	providerName string,
	chunkDigests []digest.Digest,
	chunkJustFilled int,
) {
	for i, cd := range chunkDigests {
		if i == chunkJustFilled {
			continue // just committed, definitely present
		}
		if cd == "" {
			return // sparse / unknown digest — cannot confirm full
		}
		if _, err := s.cfg.Content.Info(ctx, cd); err != nil {
			return // at least one chunk still missing
		}
	}
	// All chunks present — remove the provider record.
	if err := update(ctx, s.db, func(tx *bolt.Tx) error {
		return deleteProvider(tx, providerName)
	}); err != nil {
		log.G(ctx).WithError(err).WithFields(log.Fields{
			"blob":     dgst,
			"provider": providerName,
		}).Warn("content/index: failed to purge provider record after full fill")
	}
}

// ── FillBatch ─────────────────────────────────────────────────────────────────

// batchOwnedEntry pairs a chunk index with the digest expected for its
// bytes and the fill gate this caller acquired.  Shared between
// FillBatch (which builds the slice) and fetchAndIngestRun (which
// consumes contiguous sub-slices).
type batchOwnedEntry struct {
	idx       int
	chunkDgst digest.Digest
	gate      *fillGate
}

// FillBatch fetches a contiguous run of chunks in a single provider Fetch
// call.  See the IndexStore.FillBatch interface for semantics.
//
// Concurrency model: this method shares the per-(blob, chunkIdx) fillGate
// machinery with FillChunk.  For every passed chunkIdx the call either
// becomes the owner (and is responsible for fetch + ingest) or a waiter
// (a peer FillChunk/FillBatch is already in flight).  The set of OWNED
// indices is then split into maximal contiguous sub-runs and each
// sub-run becomes ONE provider Fetch.  Waiter gates are awaited at the
// end.  This is correct under arbitrary peer overlap; in the common
// no-overlap case the whole run is one fetch.
//
// Validation: chunkIdxs MUST be sorted ascending, free of duplicates,
// and the underlying chunks MUST be contiguous on the blob (every
// chunk's OnBlobEnd equals the next's OnBlobStart).  Failures return
// errdefs.ErrInvalidArgument before any side effect.
func (s *Store) FillBatch(
	ctx context.Context,
	dgst digest.Digest,
	chunkIdxs []int,
	p contentindex.ByteProvider,
	priority contentindex.Priority,
) error {
	if len(chunkIdxs) == 0 {
		return nil
	}
	if len(chunkIdxs) == 1 {
		// Trivial fall-through: avoid the validation overhead.
		return s.FillChunk(ctx, dgst, chunkIdxs[0], p, priority)
	}

	// Validate the input list shape before anything else.
	for i := 1; i < len(chunkIdxs); i++ {
		if chunkIdxs[i] <= chunkIdxs[i-1] {
			return fmt.Errorf("content/index: FillBatch: chunkIdxs not strictly ascending at %d (got %d after %d): %w",
				i, chunkIdxs[i], chunkIdxs[i-1], errdefs.ErrInvalidArgument)
		}
	}

	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return err
	}

	// Load blob metadata + chunk digests + chunk-index payload once
	// for the whole batch.
	var (
		meta        blobMeta
		chunkDigsts []digest.Digest
	)
	if err := view(ctx, s.db, func(tx *bolt.Tx) error {
		blobBkt := getBlobBucket(tx, ns, dgst)
		if blobBkt == nil {
			return blobNotFound(dgst)
		}
		var merr error
		meta, merr = readBlobMeta(blobBkt)
		if merr != nil {
			return merr
		}
		chunkDigsts, merr = readChunkDigests(blobBkt)
		return merr
	}); err != nil {
		return err
	}

	indexPayload, err := s.readContentEntry(ctx, meta.IndexDigest)
	if err != nil {
		return fmt.Errorf("content/index: FillBatch: read chunk-index: %w", err)
	}
	desc := ocispec.Descriptor{
		Digest:    dgst,
		Size:      meta.Size,
		MediaType: meta.MediaType,
	}
	chunks, _, err := parseChunkIndexPayload(indexPayload, meta.IndexOffset, meta.MediaType)
	if err != nil {
		return fmt.Errorf("content/index: FillBatch: parse chunk-index: %w", err)
	}

	// Validate every index is in range and the requested run is
	// contiguous on the blob.  Without on-blob contiguity, a single
	// Range request would skip needed bytes — the whole strategy is
	// wrong.  Caller must guarantee this; we belt-and-braces verify.
	for i, idx := range chunkIdxs {
		if idx < 0 || idx >= len(chunks) || idx >= len(chunkDigsts) {
			return fmt.Errorf("content/index: FillBatch: index %d out of range [0,%d): %w",
				idx, len(chunks), errdefs.ErrInvalidArgument)
		}
		if i > 0 {
			prev := chunks[chunkIdxs[i-1]]
			cur := chunks[idx]
			if prev.OnBlobEnd != cur.OnBlobStart {
				return fmt.Errorf("content/index: FillBatch: chunks %d → %d not contiguous (gap %d→%d): %w",
					chunkIdxs[i-1], idx, prev.OnBlobEnd, cur.OnBlobStart, errdefs.ErrInvalidArgument)
			}
		}
	}

	// Phase 1: probe the content store + acquire fill gates.  Build
	// three sets:
	//   - skip:    already present in the content store
	//   - owned:   newly-acquired gates we must fill
	//   - waiters: peer-owned gates we'll await at the end
	type waitEntry struct {
		idx  int
		gate *fillGate
	}
	owned := make([]batchOwnedEntry, 0, len(chunkIdxs))
	waiters := make([]waitEntry, 0)

	for _, idx := range chunkIdxs {
		chunkDgst := chunkDigsts[idx]
		if chunkDgst == "" {
			return fmt.Errorf("content/index: FillBatch: chunk %d has no digest", idx)
		}
		if _, err := s.cfg.Content.Info(ctx, chunkDgst); err == nil {
			continue // already present — skip
		} else if !errdefs.IsNotFound(err) {
			return fmt.Errorf("content/index: FillBatch: info chunk %d: %w", idx, err)
		}
		key := fillChunkKey{dgst: dgst, chunkIdx: idx}
		gate, owner := s.acquireFillGate(key)
		if owner {
			owned = append(owned, batchOwnedEntry{idx: idx, chunkDgst: chunkDgst, gate: gate})
		} else {
			waiters = append(waiters, waitEntry{idx: idx, gate: gate})
		}
	}

	// Cleanup helper for partial-failure paths: release any gates we
	// own that we haven't yet completed.  Gates we successfully
	// completed are released inline via releaseFillGate.
	releaseAllOwned := func() {
		for _, o := range owned {
			s.releaseFillGate(fillChunkKey{dgst: dgst, chunkIdx: o.idx}, o.gate)
		}
		owned = nil
	}

	// Phase 2: split owned indices into maximal contiguous sub-runs
	// (peer-owned indices break a run).  Each sub-run = one fetch.
	pStr := "fg"
	if priority != contentindex.PriorityForeground {
		pStr = "bg"
	}
	runStart := 0
	for runStart < len(owned) {
		runEnd := runStart + 1
		for runEnd < len(owned) {
			prev := chunks[owned[runEnd-1].idx]
			cur := chunks[owned[runEnd].idx]
			if prev.OnBlobEnd != cur.OnBlobStart {
				break
			}
			runEnd++
		}
		run := owned[runStart:runEnd]
		if err := s.fetchAndIngestRun(ctx, dgst, desc, p, priority, pStr, chunks, run); err != nil {
			// Mark every remaining owned gate with the error,
			// then release them.  Waiters will observe gate.err.
			for i := runStart; i < len(owned); i++ {
				owned[i].gate.err = err
			}
			releaseAllOwned()
			return err
		}
		// Successful sub-run: release these gates.
		for _, o := range run {
			s.releaseFillGate(fillChunkKey{dgst: dgst, chunkIdx: o.idx}, o.gate)
		}
		runStart = runEnd
	}
	owned = nil // all released

	// Phase 3: await any peer-owned gates.  We don't re-issue a fetch
	// for them — the peer's own commit covers it.
	for _, w := range waiters {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.gate.ch:
		}
		if w.gate.err != nil {
			return fmt.Errorf("content/index: FillBatch: peer fill of chunk %d failed: %w", w.idx, w.gate.err)
		}
	}

	// If the batch terminated at a chunk-index that completes the
	// blob, the last chunk's purgeProviderIfFull (called below for
	// each ingested chunk) already took care of provider record
	// removal.  No additional bookkeeping needed here.
	return nil
}

// fetchAndIngestRun issues ONE provider.Fetch for the contiguous
// on-blob range spanning `run`, then verifies + ingests each chunk
// into the content store.  Caller still holds the gate for every
// chunk in `run`; this function does NOT release them.
func (s *Store) fetchAndIngestRun(
	ctx context.Context,
	dgst digest.Digest,
	desc ocispec.Descriptor,
	p contentindex.ByteProvider,
	priority contentindex.Priority,
	pStr string,
	chunks []contentindex.ChunkRef,
	run []batchOwnedEntry,
) error {
	if len(run) == 0 {
		return nil
	}
	first := chunks[run[0].idx]
	last := chunks[run[len(run)-1].idx]
	mergedStart := first.OnBlobStart
	mergedEnd := last.OnBlobEnd
	mergedLen := mergedEnd - mergedStart

	log.G(ctx).WithFields(log.Fields{
		"blob":        dgst,
		"chunk_first": run[0].idx,
		"chunk_last":  run[len(run)-1].idx,
		"chunk_count": len(run),
		"priority":    pStr,
		"range_start": mergedStart,
		"range_end":   mergedEnd,
		"range_bytes": mergedLen,
	}).Info("[lazy-viz] batch_fetch_start")

	// ── Phase 1: network fetch (no bolt tx, no content-store mutation).
	fetchStart := time.Now()
	rc, err := p.Fetch(ctx, desc, mergedStart, mergedLen, priority)
	if err != nil {
		return fmt.Errorf("content/index: FillBatch: fetch [%d,%d): %w", mergedStart, mergedEnd, err)
	}
	mergedBytes, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return fmt.Errorf("content/index: FillBatch: read [%d,%d): %w", mergedStart, mergedEnd, err)
	}
	if int64(len(mergedBytes)) != mergedLen {
		return fmt.Errorf("content/index: FillBatch: short read: got %d want %d: %w",
			len(mergedBytes), mergedLen, errdefs.ErrFailedPrecondition)
	}
	fetchDur := time.Since(fetchStart)

	log.G(ctx).WithFields(log.Fields{
		"blob":        dgst,
		"chunk_first": run[0].idx,
		"chunk_last":  run[len(run)-1].idx,
		"chunk_count": len(run),
		"bytes":       len(mergedBytes),
		"duration_ms": fetchDur.Milliseconds(),
		"bps":         int64(float64(len(mergedBytes)) / fetchDur.Seconds()),
	}).Info("[lazy-viz] batch_fetch_done")

	// ── Phase 2: hash-verify every chunk's slice (no bolt tx).
	//
	// Doing all hashing before the tx means a single bad chunk
	// fails before we burn a write transaction; it also keeps the
	// tx narrow (no CPU-heavy work inside it).
	chunkSlices := make([][]byte, len(run))
	for i, o := range run {
		c := chunks[o.idx]
		lo := c.OnBlobStart - mergedStart
		hi := c.OnBlobEnd - mergedStart
		chunkBytes := mergedBytes[lo:hi]
		chunkSlices[i] = chunkBytes
		h := o.chunkDgst.Algorithm().Digester()
		h.Hash().Write(chunkBytes)
		if actual := h.Digest(); actual != o.chunkDgst {
			err := fmt.Errorf("content/index: FillBatch: chunk %d hash mismatch: got %s want %s: %w",
				o.idx, actual, o.chunkDgst, errdefs.ErrFailedPrecondition)
			o.gate.err = err
			return err
		}
	}

	// ── Phase 3: ONE bolt write transaction wrapping every chunk's
	// Writer + Truncate + Write + Commit.  s.db is the shared
	// metadata Transactor — opening Update here puts a single
	// writable tx in ctx via boltutil.WithTransaction; every
	// subsequent call into the metadata-wrapped content store
	// (Writer, Commit) sees that tx through their own
	// `update(ctx, ...)` helpers and reuses it instead of
	// opening their own.
	//
	// Per-chunk we would otherwise pay 2 bolt fsyncs (one for
	// Writer's ingest-bucket-create + one for Commit's
	// commit-and-lease-swap).  Batching collapses 2N → 1 bolt
	// fsync for a run of N chunks.
	//
	// The "no extra actions during the transaction" invariant: we
	// have already fetched and hash-verified the bytes; nothing
	// inside the closure touches the network, parses the
	// chunk-index, or does any work unrelated to persisting these
	// specific chunks.  The data-file writes (io.Copy into the
	// local writer's ingest file) and per-file fsyncs (inside
	// Commit) DO happen inside the tx — that's unavoidable since
	// they're part of the commit pipeline — but those don't take
	// the bolt write lock and they're the minimum-necessary work
	// to durably land a chunk.
	if err := update(ctx, s.db, func(tx *bolt.Tx) error {
		txCtx := boltutil.WithTransaction(ctx, tx)
		for i, o := range run {
			chunkBytes := chunkSlices[i]
			cw, werr := s.cfg.Content.Writer(txCtx,
				content.WithRef(fmt.Sprintf("content-index-fill-%s-%d", dgst, o.idx)),
				content.WithDescriptor(ocispec.Descriptor{
					MediaType: "application/octet-stream",
					Digest:    o.chunkDgst,
					Size:      int64(len(chunkBytes)),
				}),
			)
			if werr != nil {
				if errdefs.IsAlreadyExists(werr) {
					continue
				}
				o.gate.err = werr
				return fmt.Errorf("content/index: FillBatch: open writer for chunk %d: %w", o.idx, werr)
			}
			if terr := cw.Truncate(0); terr != nil {
				cw.Close()
				o.gate.err = terr
				return fmt.Errorf("content/index: FillBatch: truncate writer for chunk %d: %w", o.idx, terr)
			}
			if _, werr := io.Copy(cw, bytes.NewReader(chunkBytes)); werr != nil {
				cw.Close()
				o.gate.err = werr
				return fmt.Errorf("content/index: FillBatch: write chunk %d: %w", o.idx, werr)
			}
			if cerr := cw.Commit(txCtx, int64(len(chunkBytes)), o.chunkDgst); cerr != nil {
				if !errdefs.IsAlreadyExists(cerr) {
					cw.Close()
					o.gate.err = cerr
					return fmt.Errorf("content/index: FillBatch: commit chunk %d: %w", o.idx, cerr)
				}
			}
		}
		return nil
	}); err != nil {
		return err
	}

	// ── Phase 4: post-tx observability and provider-record GC.
	for _, o := range run {
		log.G(ctx).WithFields(log.Fields{
			"blob":  dgst,
			"chunk": o.idx,
			"bytes": len(chunkSlices[0]), // approximate; logged for visualizer
		}).Info("[lazy-viz] chunk_fetch_done")
	}
	last_idx := run[len(run)-1].idx
	ns, _ := namespaces.Namespace(ctx) // namespace already validated upstream
	var chunkDigsts []digest.Digest
	var providerName string
	if err := view(ctx, s.db, func(tx *bolt.Tx) error {
		blobBkt := getBlobBucket(tx, ns, dgst)
		if blobBkt == nil {
			return nil
		}
		meta, _ := readBlobMeta(blobBkt)
		providerName = meta.Provider
		chunkDigsts, _ = readChunkDigests(blobBkt)
		return nil
	}); err != nil || providerName == "" {
		return nil
	}
	s.purgeProviderIfFull(ctx, ns, dgst, providerName, chunkDigsts, last_idx)
	return nil
}

// ── Fill gate helpers ──────────────────────────────────────────────────────────

// acquireFillGate either creates a new fillGate for the key (owner=true) or
// returns the existing one for a concurrent caller to wait on (owner=false).
func (s *Store) acquireFillGate(key fillChunkKey) (*fillGate, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fillGates == nil {
		s.fillGates = make(map[fillChunkKey]*fillGate)
	}
	if g, ok := s.fillGates[key]; ok {
		return g, false
	}
	g := &fillGate{ch: make(chan struct{})}
	s.fillGates[key] = g
	return g, true
}

func (s *Store) releaseFillGate(key fillChunkKey, g *fillGate) {
	s.mu.Lock()
	delete(s.fillGates, key)
	s.mu.Unlock()
	close(g.ch)
}
