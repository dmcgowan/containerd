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
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/klauspost/compress/zstd"
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

	now := time.Now().UTC()
	m := blobMeta{
		Size:        desc.Size,
		MediaType:   desc.MediaType,
		Provider:    p.Name(),
		IndexDigest: indexDigest,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

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
		// No extras for lazy ingest — they are reconstructed from the index
		// if the blob is ever re-assembled. The extras only matter for
		// byte-exact blob reproduction (push back to registry), which the
		// lazy path does not support in v1.
		return writeLabels(blobBkt, map[string]string{
			"containerd.io/gc.ref.content.index": desc.Digest.String(),
		})
	})
	_ = ref // ref is reserved for future dedup of concurrent lazy ingests
	return err
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
	chunks, _, err := parseChunkIndexPayload(indexPayload, 0, meta.MediaType)
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

	// Determine blobSectionOffset for parsing.
	// We don't have the descriptor here so we'll use offset 0 as a fallback —
	// parseChunkIndexPayload only needs it to compute OnBlobStart/OnBlobEnd.
	// For lazy blobs the descriptor's chunk-index range annotation would
	// give the exact offset; pass 0 for a best-effort result.
	chunks, _, err := parseChunkIndexPayload(indexPayload, 0, meta.MediaType)
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
	chunks, _, err := parseChunkIndexPayload(indexPayload, 0, meta.MediaType)
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
	rc, err := p.Fetch(ctx, desc, c, priority)
	if err != nil {
		gate.err = fmt.Errorf("content/index: FillChunk: fetch chunk %d: %w", chunkIdx, err)
		return gate.err
	}
	defer rc.Close()

	// Read and potentially decompress. The stored chunk bytes are the
	// uncompressed bytes (consistent with how extractChunks stores them in
	// the eager path).
	chunkBytes, err := s.decompressChunk(rc, meta.MediaType)
	if err != nil {
		gate.err = fmt.Errorf("content/index: FillChunk: decompress chunk %d: %w", chunkIdx, err)
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
	return nil
}

// decompressChunk decompresses chunkBytes if the blob's media type is +zstd.
// For raw layers the bytes are returned as-is.
func (s *Store) decompressChunk(r io.Reader, mediaType string) ([]byte, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if !isZstdMediaType(mediaType) {
		return raw, nil
	}
	dec, err := zstd.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("zstd decoder: %w", err)
	}
	defer dec.Close()
	return io.ReadAll(dec)
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
