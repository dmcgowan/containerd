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

// Package local is the in-tree implementation of the indexed content
// store (core/content/index).
//
// The store keeps chunk content in containerd's existing content store
// (one content store entry per chunk, keyed by the chunk's per-chunk
// hash from the chunk index) and tracks blob → reachability metadata in
// buckets inside the shared metadata BoltDB.  See buckets.go for the
// schema.
//
// Metadata writes join any bolt.Tx already on the context via
// boltutil.WithTransaction, falling back to opening their own transaction
// when none is present.  Callers that want to batch multiple writes into
// a single fsync push a writable transaction onto the context before
// calling into the store (see bolt.go for the view/update helpers).
package local

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/filters"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	bolt "go.etcd.io/bbolt"
)

// Config configures the local store at construction time.
type Config struct {
	// Root is the directory used for temporary ingest scratch files.
	// Created with mode 0700 if absent.
	Root string

	// DB is the shared metadata transactor.  In production this is the
	// containerd metadata.DB; in tests a plain *bolt.DB is sufficient
	// (it satisfies the Transactor interface directly).  Required.
	DB Transactor

	// Content is the content store the indexed content store delegates
	// chunk, chunk-index, and extra byte-range storage to. Required.
	Content content.Store

	// Providers is the ordered list of byte providers consulted when the
	// store needs to source blob bytes from outside the local content store.
	Providers []contentindex.ByteProvider
}

// Store is the local implementation of index.Store.
type Store struct {
	cfg Config
	db  Transactor

	mu        sync.Mutex
	ingests   map[string]*ingestState  // active ingests, keyed by ref
	fillGates map[fillChunkKey]*fillGate // in-flight FillChunk coalescing
}

// ingestState tracks an in-flight ingest so concurrent calls to Writer
// with the same ref return an error.
type ingestState struct {
	ref       string
	desc      ocispec.Descriptor
	startedAt time.Time
}

// NewStore initialises the local indexed content store.
//
// cfg.DB must be set to the shared metadata Transactor (the containerd
// metadata.DB in production, a plain *bolt.DB in tests).  The store writes
// its metadata into the shared BoltDB under the "indexed-content" bucket
// path; it does not open or own any database file itself.
func NewStore(cfg Config) (*Store, error) {
	if cfg.DB == nil {
		return nil, fmt.Errorf("content/index: Config.DB is required")
	}
	if cfg.Content == nil {
		return nil, fmt.Errorf("content/index: Config.Content is required")
	}
	if cfg.Root == "" {
		return nil, fmt.Errorf("content/index: Config.Root is required")
	}
	if err := os.MkdirAll(cfg.Root, 0700); err != nil {
		return nil, fmt.Errorf("content/index: create root: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(cfg.Root, "ingest"), 0700); err != nil {
		return nil, fmt.Errorf("content/index: create ingest dir: %w", err)
	}
	// Ensure the indexed-content schema version key is written.
	if err := cfg.DB.Update(initDBVersion); err != nil {
		return nil, fmt.Errorf("content/index: init db version: %w", err)
	}
	return &Store{
		cfg:     cfg,
		db:      cfg.DB,
		ingests: map[string]*ingestState{},
	}, nil
}

// Compile-time assertion: Store implements contentindex.Store.
var _ contentindex.Store = (*Store)(nil)

// Close is a no-op: the store does not own the database it was given.
// It exists for interface compatibility and to allow callers to defer
// store.Close() without special-casing.
func (s *Store) Close() error { return nil }

// ContentStore returns the underlying content store.
func (s *Store) ContentStore() content.Store { return s.cfg.Content }

// ── Manager ───────────────────────────────────────────────────────────────────

// Info returns the metadata record for a blob.  Chunk offsets and lengths
// are NOT included; open and parse the chunk-index content-store entry (keyed
// by the returned Info.IndexDigest) to get them.
func (s *Store) Info(ctx context.Context, dgst digest.Digest) (contentindex.Info, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return contentindex.Info{}, err
	}
	var info contentindex.Info
	err = view(ctx, s.db, func(tx *bolt.Tx) error {
		blobBkt := getBlobBucket(tx, ns, dgst)
		if blobBkt == nil {
			return blobNotFound(dgst)
		}
		m, err := readBlobMeta(blobBkt)
		if err != nil {
			return err
		}
		lbls, err := readLabels(blobBkt)
		if err != nil {
			return err
		}
		info = metaToInfo(dgst, m, lbls)
		return nil
	})
	if err != nil {
		return contentindex.Info{}, err
	}
	return info, nil
}

// Update replaces or merges labels on an existing blob.
func (s *Store) Update(ctx context.Context, info contentindex.Info, fieldpaths ...string) (contentindex.Info, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return contentindex.Info{}, err
	}
	var out contentindex.Info
	err = update(ctx, s.db, func(tx *bolt.Tx) error {
		blobBkt := getBlobBucket(tx, ns, info.Digest)
		if blobBkt == nil {
			return blobNotFound(info.Digest)
		}
		m, err := readBlobMeta(blobBkt)
		if err != nil {
			return err
		}
		if err := updateLabels(blobBkt, fieldpaths, info.Labels); err != nil {
			return err
		}
		m.UpdatedAt = time.Now().UTC()
		if err := writeBlobMeta(blobBkt, m); err != nil {
			return err
		}
		lbls, err := readLabels(blobBkt)
		if err != nil {
			return err
		}
		out = metaToInfo(info.Digest, m, lbls)
		return nil
	})
	if err != nil {
		return contentindex.Info{}, err
	}
	return out, nil
}

// Walk iterates entries in the caller's namespace.
func (s *Store) Walk(ctx context.Context, fn contentindex.WalkFunc, filterStrings ...string) error {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return err
	}
	filter, err := filters.ParseAll(filterStrings...)
	if err != nil {
		return err
	}
	return view(ctx, s.db, func(tx *bolt.Tx) error {
		bb := getBlobsBucket(tx, ns)
		if bb == nil {
			return nil
		}
		return bb.ForEach(func(k, v []byte) error {
			if v != nil {
				return nil // skip plain k/v
			}
			blobBkt := bb.Bucket(k)
			if blobBkt == nil {
				return nil
			}
			dgst, err := digest.Parse(string(k))
			if err != nil {
				return nil
			}
			m, err := readBlobMeta(blobBkt)
			if err != nil {
				return err
			}
			lbls, err := readLabels(blobBkt)
			if err != nil {
				return err
			}
			info := metaToInfo(dgst, m, lbls)
			if !filter.Match(adaptInfo(info)) {
				return nil
			}
			return fn(info)
		})
	})
}

// Delete removes the metadata record for a blob.  Chunks and extras the
// blob referenced are no longer pinned by this entry; whether they are
// reclaimed depends on whether any other indexed-content blob still names them.
func (s *Store) Delete(ctx context.Context, dgst digest.Digest) error {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return err
	}
	return update(ctx, s.db, func(tx *bolt.Tx) error {
		bb := getBlobsBucket(tx, ns)
		if bb == nil {
			return blobNotFound(dgst)
		}
		if bb.Bucket([]byte(dgst)) == nil {
			return blobNotFound(dgst)
		}
		return bb.DeleteBucket([]byte(dgst))
	})
}

// ── Provider ──────────────────────────────────────────────────────────────────

// ReaderAt returns a reader that reproduces the blob's original bytes by
// merging chunk content-store entries and extras in blob-offset order.
//
// The descriptor must carry org.erofs.index.* annotations so the reader can
// locate the chunk-index section within the blob.  Only desc.Digest and
// desc.Annotations are required; desc.Size is used for the reader's Size()
// method (falling back to the metadata-recorded size if zero).
func (s *Store) ReaderAt(ctx context.Context, desc ocispec.Descriptor) (content.ReaderAt, error) {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}

	// Parse chunk-index location from the descriptor annotations.
	loc, err := parseIndexLocation(desc)
	if err != nil {
		return nil, err
	}

	// Read metadata record: IndexDigest, blob size, extras.
	var (
		meta   blobMeta
		extras []extra
	)
	if err := view(ctx, s.db, func(tx *bolt.Tx) error {
		blobBkt := getBlobBucket(tx, ns, desc.Digest)
		if blobBkt == nil {
			return blobNotFound(desc.Digest)
		}
		var merr error
		meta, merr = readBlobMeta(blobBkt)
		if merr != nil {
			return merr
		}
		extras, merr = readExtras(blobBkt)
		return merr
	}); err != nil {
		return nil, err
	}

	size := desc.Size
	if size == 0 {
		size = meta.Size
	}

	// Determine blob-section offset: if loc.End is 0 the index runs to end
	// of blob, so use the recorded blob size as the actual end.
	blobSectionOffset := loc.Offset
	actualEnd := loc.End
	if actualEnd == 0 {
		actualEnd = size
	}

	// Load the chunk-index payload from the content store and parse it.
	indexPayload, err := s.readContentEntry(ctx, meta.IndexDigest)
	if err != nil {
		return nil, fmt.Errorf("content/index: open chunk-index entry %s: %w", meta.IndexDigest, err)
	}
	chunks, _, err := parseChunkIndexPayload(indexPayload, blobSectionOffset, desc.MediaType)
	if err != nil {
		return nil, fmt.Errorf("content/index: parse chunk-index: %w", err)
	}

	// Build the unified segment list covering [0, size).
	segs, err := buildSegments(chunks, extras, size)
	if err != nil {
		return nil, err
	}

	return &assembledReader{
		ctx:  ctx,
		cs:   s.cfg.Content,
		size: size,
		segs: segs,
	}, nil
}

// readContentEntry reads all bytes from a content-store entry into memory.
func (s *Store) readContentEntry(ctx context.Context, dgst digest.Digest) ([]byte, error) {
	ra, err := s.cfg.Content.ReaderAt(ctx, ocispec.Descriptor{Digest: dgst})
	if err != nil {
		return nil, err
	}
	defer ra.Close()
	buf := make([]byte, ra.Size())
	if _, err := ra.ReadAt(buf, 0); err != nil && err != io.EOF {
		return nil, err
	}
	return buf, nil
}

// Mounts is not implemented in v1.
func (s *Store) Mounts(_ context.Context, _ digest.Digest) ([]mount.Mount, error) {
	return nil, fmt.Errorf("content/index: Mounts not implemented in v1: %w", errdefs.ErrNotImplemented)
}

// ── Segment list and assembled reader ────────────────────────────────────────

// segKind identifies the source of a segment's bytes.
type segKind int

const (
	segChunk segKind = iota // bytes come from a content-store chunk entry
	segExtra                // bytes come from a zstd-compressed extra
)

// segment describes one contiguous byte range in the original blob.
//
// Segments cover [0, blobSize) without gaps or overlaps.  They are built
// once in buildSegments and then used read-only by the assembled reader.
type segment struct {
	start int64
	end   int64
	kind  segKind

	// For segChunk: the content-store digest of the compressed zstd frame
	// (or raw bytes for a raw layer).
	chunkDigest digest.Digest

	// For segExtra: compressed bytes (either inline from the metadata record or
	// loaded once from the content store) and their decompressed form
	// (populated lazily on first access).
	extraDigest  digest.Digest // non-empty → content-store entry
	extraInline  []byte        // non-nil → inline compressed bytes
	extraDecOnce sync.Once
	extraDec     []byte // decompressed; populated by extraDecOnce
	extraDecErr  error
}

// decompressedExtra returns the decompressed bytes for a segExtra segment,
// loading the compressed payload from the content store if needed.
func (seg *segment) decompressedExtra(ctx context.Context, cs content.Store) ([]byte, error) {
	seg.extraDecOnce.Do(func() {
		var compressed []byte
		if len(seg.extraInline) > 0 {
			compressed = seg.extraInline
		} else if seg.extraDigest != "" {
			ra, err := cs.ReaderAt(ctx, ocispec.Descriptor{Digest: seg.extraDigest})
			if err != nil {
				seg.extraDecErr = fmt.Errorf("content/index: open extra %s: %w", seg.extraDigest, err)
				return
			}
			defer ra.Close()
			buf := make([]byte, ra.Size())
			if _, err := ra.ReadAt(buf, 0); err != nil && err != io.EOF {
				seg.extraDecErr = fmt.Errorf("content/index: read extra %s: %w", seg.extraDigest, err)
				return
			}
			compressed = buf
		}
		if len(compressed) == 0 {
			seg.extraDec = nil
			return
		}
		dec, err := zstd.NewReader(nil)
		if err != nil {
			seg.extraDecErr = fmt.Errorf("content/index: new zstd decoder: %w", err)
			return
		}
		defer dec.Close()
		out, err := dec.DecodeAll(compressed, nil)
		if err != nil {
			seg.extraDecErr = fmt.Errorf("content/index: decompress extra: %w", err)
			return
		}
		seg.extraDec = out
	})
	return seg.extraDec, seg.extraDecErr
}

// buildSegments merges the parsed chunks and stored extras into a single
// list of segments sorted by start offset, covering [0, blobSize).
//
// Chunks own their on-blob byte ranges.  Extras fill everything else.
// A gap that appears in neither the chunk list nor the extras list is
// treated as an unrecoverable layout error.
func buildSegments(chunks []contentindex.ChunkRef, extras []extra, blobSize int64) ([]*segment, error) {
	segs := make([]*segment, 0, len(chunks)+len(extras))

	for i := range chunks {
		c := &chunks[i]
		segs = append(segs, &segment{
			start:       c.OnBlobStart,
			end:         c.OnBlobEnd,
			kind:        segChunk,
			chunkDigest: c.Digest,
		})
	}
	for i := range extras {
		ex := &extras[i]
		seg := &segment{
			start:       ex.Offset,
			end:         ex.Offset + ex.Length,
			kind:        segExtra,
			extraDigest: ex.Digest,
		}
		if len(ex.Inline) > 0 {
			// Copy inline bytes so the segment is self-contained.
			seg.extraInline = make([]byte, len(ex.Inline))
			copy(seg.extraInline, ex.Inline)
		}
		segs = append(segs, seg)
	}

	// Sort by start offset.
	sort.Slice(segs, func(i, j int) bool { return segs[i].start < segs[j].start })

	// Validate: no gaps, no overlaps, must cover [0, blobSize).
	var pos int64
	for _, seg := range segs {
		if seg.start > pos {
			return nil, fmt.Errorf("content/index: gap in blob coverage at offset %d (next segment starts at %d)", pos, seg.start)
		}
		if seg.start < pos {
			return nil, fmt.Errorf("content/index: segment overlap at offset %d", seg.start)
		}
		if seg.end <= seg.start {
			return nil, fmt.Errorf("content/index: empty or inverted segment [%d, %d)", seg.start, seg.end)
		}
		pos = seg.end
	}
	if pos != blobSize {
		return nil, fmt.Errorf("content/index: segment coverage ends at %d but blob size is %d", pos, blobSize)
	}
	return segs, nil
}

// assembledReader implements content.ReaderAt by serving bytes from the
// ordered segment list.  Each read locates the covering segment(s) via
// binary search and dispatches to the appropriate content-store entry or
// decompressed extra payload.
type assembledReader struct {
	ctx  context.Context
	cs   content.Store
	size int64
	segs []*segment
}

func (r *assembledReader) Size() int64 { return r.size }

func (r *assembledReader) Close() error { return nil }

// ReadAt serves bytes from the assembled segment list.
func (r *assembledReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("content/index: negative offset %d", off)
	}
	if off >= r.size {
		return 0, io.EOF
	}

	// Find the segment that covers off using binary search.
	idx := sort.Search(len(r.segs), func(i int) bool {
		return r.segs[i].end > off
	})
	if idx >= len(r.segs) || r.segs[idx].start > off {
		return 0, io.EOF
	}

	written := 0
	for written < len(p) && idx < len(r.segs) {
		seg := r.segs[idx]
		if seg.start > off {
			// Gap (shouldn't happen after buildSegments validation, but be safe).
			break
		}

		withinSeg := off - seg.start
		segLen := seg.end - seg.start
		remaining := segLen - withinSeg
		need := int64(len(p) - written)
		if need > remaining {
			need = remaining
		}

		switch seg.kind {
		case segChunk:
			n, err := r.readFromChunk(seg, p[written:written+int(need)], withinSeg)
			written += n
			off += int64(n)
			if err != nil && err != io.EOF {
				return written, err
			}
		case segExtra:
			dec, err := seg.decompressedExtra(r.ctx, r.cs)
			if err != nil {
				return written, err
			}
			// Serve directly from the decompressed buffer.
			if withinSeg+need > int64(len(dec)) {
				return written, fmt.Errorf("content/index: extra decompressed length %d < required %d", len(dec), withinSeg+need)
			}
			copy(p[written:written+int(need)], dec[withinSeg:withinSeg+need])
			written += int(need)
			off += need
		}

		if off >= seg.end {
			idx++
		}
	}

	if written == len(p) {
		return written, nil
	}
	return written, io.EOF
}

// readFromChunk reads need bytes starting at withinSeg from the chunk's
// content-store entry.
func (r *assembledReader) readFromChunk(seg *segment, dst []byte, withinSeg int64) (int, error) {
	ra, err := r.cs.ReaderAt(r.ctx, ocispec.Descriptor{Digest: seg.chunkDigest})
	if err != nil {
		return 0, fmt.Errorf("content/index: open chunk %s: %w", seg.chunkDigest, err)
	}
	defer ra.Close()
	n, err := ra.ReadAt(dst, withinSeg)
	if err == io.EOF {
		err = nil
	}
	return n, err
}

// ── Filter adaptor ────────────────────────────────────────────────────────────

func adaptInfo(info contentindex.Info) filters.Adaptor {
	return filters.AdapterFunc(func(fieldpath []string) (string, bool) {
		if len(fieldpath) == 0 {
			return "", false
		}
		switch fieldpath[0] {
		case "digest":
			return info.Digest.String(), true
		case "size":
			return fmt.Sprintf("%d", info.Size), true
		case "mediatype":
			return info.MediaType, true
		case "labels":
			return checkLabelMap(fieldpath[1:], info.Labels)
		}
		return "", false
	})
}

func checkLabelMap(fieldpath []string, m map[string]string) (string, bool) {
	if len(m) == 0 {
		return "", false
	}
	v, ok := m[strings.Join(fieldpath, ".")]
	return v, ok
}
