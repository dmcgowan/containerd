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
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"path/filepath"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	localcs "github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	bolt "go.etcd.io/bbolt"
)

// newTestStore opens a plain bolt.DB (which satisfies Transactor directly)
// and creates an indexed content store backed by it.  The DB is closed
// automatically when the test ends.
func newTestStore(t *testing.T, cs content.Store) *Store {
	t.Helper()
	bdb, err := bolt.Open(
		filepath.Join(t.TempDir(), "meta.db"), 0644, nil)
	if err != nil {
		t.Fatalf("open bolt db: %v", err)
	}
	t.Cleanup(func() { bdb.Close() })
	store, err := NewStore(Config{
		Root:    t.TempDir(),
		DB:      bdb,
		Content: cs,
	})
	if err != nil {
		t.Fatalf("new indexed store: %v", err)
	}
	return store
}

// TestRoundTrip_ZstdChunkedBlob creates a synthetic +zstd chunked blob whose
// format matches the erofs-image-spec exactly, ingests it into the local
// indexed content store, then verifies:
//
//  1. Each chunk is present in the content store under its per-chunk hash.
//  2. The chunk-index payload is present under org.erofs.index.digest.
//  3. ReaderAt reproduces the original blob byte-for-byte.
//  4. The sequential digest of the assembled reader matches the descriptor
//     digest (byte-exact blob reproduction).
//  5. Reading individual chunks at random offsets returns correct bytes.
//  6. Info returns the correct IndexDigest and size.
func TestRoundTrip_ZstdChunkedBlob(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")

	// Build a content store backed by a temp directory.
	csRoot := t.TempDir()
	cs, err := localcs.NewStore(csRoot)
	if err != nil {
		t.Fatalf("new content store: %v", err)
	}

	// Build the indexed content store.
	store := newTestStore(t, cs)

	// ── Build a chunked blob ───────────────────────────────────────────────
	// Chunk sizes (uncompressed). Kept small for test speed; real layers
	// would use MiB-sized chunks.
	chunkSizes := []int{
		4 * 1024,   //  4 KiB
		16 * 1024,  // 16 KiB
		64 * 1024,  // 64 KiB
		256 * 1024, // 256 KiB
	}

	blob, desc := buildZstdChunkedBlob(t, chunkSizes)

	// ── Ingest ────────────────────────────────────────────────────────────
	w, err := store.Writer(ctx,
		content.WithRef("test-blob"),
		content.WithDescriptor(desc),
	)
	if err != nil {
		t.Fatalf("Writer: %v", err)
	}
	if _, err := io.Copy(w, bytes.NewReader(blob)); err != nil {
		t.Fatalf("stream blob: %v", err)
	}
	if err := w.Commit(ctx, int64(len(blob)), desc.Digest); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// ── 1. Verify chunk-index entry is in the content store ───────────────
	info, err := store.Info(ctx, desc.Digest)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.IndexDigest == "" {
		t.Fatal("IndexDigest not recorded in metadata")
	}
	if _, err := cs.Info(ctx, info.IndexDigest); err != nil {
		t.Fatalf("chunk-index entry %s not found in content store: %v", info.IndexDigest, err)
	}
	t.Logf("chunk-index entry: %s", info.IndexDigest)

	// ── 2. Verify each chunk is in the content store ───────────────────────
	// Re-parse the chunk-index payload from the content store.
	idxPayload := readContentEntry(t, ctx, cs, info.IndexDigest)

	// blobSectionOffset from the descriptor annotation.
	loc, err := parseIndexLocation(desc)
	if err != nil {
		t.Fatalf("parseIndexLocation: %v", err)
	}
	blobSectionOffset := loc.Offset

	chunks, _, err := parseChunkIndexPayload(idxPayload, blobSectionOffset, desc.MediaType)
	if err != nil {
		t.Fatalf("parseChunkIndexPayload: %v", err)
	}
	if len(chunks) != len(chunkSizes) {
		t.Fatalf("expected %d chunks, got %d", len(chunkSizes), len(chunks))
	}
	for i, c := range chunks {
		if c.Digest == "" {
			t.Errorf("chunk %d missing digest", i)
			continue
		}
		if _, err := cs.Info(ctx, c.Digest); err != nil {
			t.Errorf("chunk %d (%s) not found in content store: %v", i, c.Digest, err)
		}
		t.Logf("chunk %d: digest=%s onBlob=[%d,%d) uncompLen=%d", i, c.Digest, c.OnBlobStart, c.OnBlobEnd, c.Length)
	}

	// ── 3. ReaderAt reproduces the blob byte-for-byte ─────────────────────
	ra, err := store.ReaderAt(ctx, desc)
	if err != nil {
		t.Fatalf("ReaderAt: %v", err)
	}
	defer ra.Close()

	if ra.Size() != int64(len(blob)) {
		t.Fatalf("ReaderAt.Size() = %d, want %d", ra.Size(), len(blob))
	}

	got := make([]byte, len(blob))
	if _, err := ra.ReadAt(got, 0); err != nil && err != io.EOF {
		t.Fatalf("ReadAt full: %v", err)
	}
	if !bytes.Equal(got, blob) {
		// Find first difference for a useful error message.
		for i := range blob {
			if got[i] != blob[i] {
				t.Fatalf("blob mismatch at offset %d: got 0x%02x want 0x%02x", i, got[i], blob[i])
			}
		}
	}
	t.Logf("byte-for-byte reproduction: OK (%d bytes)", len(blob))

	// ── 4. Sequential digest matches descriptor digest ────────────────────
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(ra, 0, ra.Size())); err != nil {
		t.Fatalf("hash assembled reader: %v", err)
	}
	seqDigest := digest.NewDigest(digest.SHA256, h)
	if seqDigest != desc.Digest {
		t.Fatalf("sequential digest mismatch: got %s want %s", seqDigest, desc.Digest)
	}
	t.Logf("sequential digest: %s ✓", seqDigest)

	// ── 5. Random-offset reads return correct bytes ────────────────────────
	offsets := []int64{
		0,
		int64(chunkSizes[0]) / 2,       // mid first chunk
		int64(chunkSizes[0]) - 1,       // last byte of first chunk
		int64(chunkSizes[0]),            // first byte of second chunk
		int64(chunkSizes[0] + chunkSizes[1]/3), // into second chunk
	}
	for _, off := range offsets {
		readLen := 512
		if int(off)+readLen > len(blob) {
			readLen = len(blob) - int(off)
		}
		buf := make([]byte, readLen)
		n, err := ra.ReadAt(buf, off)
		if err != nil && err != io.EOF {
			t.Errorf("ReadAt(%d, %d): %v", off, readLen, err)
			continue
		}
		if !bytes.Equal(buf[:n], blob[off:off+int64(n)]) {
			t.Errorf("ReadAt(%d): bytes mismatch", off)
		}
	}
	t.Log("random-offset reads: OK")

	// ── 6. Info returns correct metadata ──────────────────────────────────
	if info.Size != int64(len(blob)) {
		t.Errorf("Info.Size = %d, want %d", info.Size, len(blob))
	}
	if info.MediaType != contentindex.MediaTypeEROFSZstd {
		t.Errorf("Info.MediaType = %q, want %q", info.MediaType, contentindex.MediaTypeEROFSZstd)
	}
	t.Logf("Info: size=%d mediaType=%s indexDigest=%s", info.Size, info.MediaType, info.IndexDigest)
}

// TestReingestDedup verifies that ingesting the same blob twice does not
// duplicate chunk content-store entries.
func TestReingestDedup(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")

	cs, err := localcs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := newTestStore(t, cs)

	blob, desc := buildZstdChunkedBlob(t, []int{8 * 1024, 16 * 1024})
	ingest := func() error {
		w, err := store.Writer(ctx,
			content.WithRef("dedup-test"),
			content.WithDescriptor(desc),
		)
		if err != nil {
			return err
		}
		if _, err := io.Copy(w, bytes.NewReader(blob)); err != nil {
			w.Close()
			return err
		}
		return w.Commit(ctx, int64(len(blob)), desc.Digest)
	}

	if err := ingest(); err != nil {
		t.Fatalf("first ingest: %v", err)
	}

	// Count content-store entries before second ingest.
	var countBefore int
	cs.Walk(ctx, func(info content.Info) error { countBefore++; return nil })

	// Second ingest: Commit should return ErrAlreadyExists.
	if err := ingest(); err == nil {
		t.Log("second ingest returned nil (idempotent is acceptable)")
	}

	var countAfter int
	cs.Walk(ctx, func(info content.Info) error { countAfter++; return nil })

	if countAfter > countBefore {
		t.Errorf("second ingest created %d new content-store entries; expected 0", countAfter-countBefore)
	}
	t.Logf("content-store entries: before=%d after=%d", countBefore, countAfter)
}

// TestDeleteRemovesMetadataRecord verifies that Delete removes the metadata record
// and that a subsequent Info returns ErrNotFound.
func TestDeleteRemovesMetadataRecord(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")

	cs, err := localcs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := newTestStore(t, cs)

	blob, desc := buildZstdChunkedBlob(t, []int{4 * 1024})
	w, err := store.Writer(ctx,
		content.WithRef("del-test"),
		content.WithDescriptor(desc),
	)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(w, bytes.NewReader(blob))
	if err := w.Commit(ctx, int64(len(blob)), desc.Digest); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if err := store.Delete(ctx, desc.Digest); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Info(ctx, desc.Digest); err == nil {
		t.Fatal("expected ErrNotFound after Delete, got nil")
	}
	t.Log("Delete: metadata record removed correctly")
}

// ── Blob builder ─────────────────────────────────────────────────────────────

// buildZstdChunkedBlob constructs a syntactically correct
// application/vnd.erofs.layer.v1+zstd blob with one zstd frame per chunk and
// an appended chunk index wrapped in a zstd skippable frame.  Random data is
// used for the "image data" so every chunk has a distinct hash.
//
// The returned blob bytes and descriptor (with all org.erofs.index.*
// annotations filled in) are ready to be handed to Store.Writer.
func buildZstdChunkedBlob(t *testing.T, chunkSizes []int) ([]byte, ocispec.Descriptor) {
	t.Helper()

	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		t.Fatalf("new zstd encoder: %v", err)
	}

	type chunkInfo struct {
		compressedFrame []byte
		uncompLen       int64
		uncompOffset    int64 // logical (uncompressed) offset
		blobStart       int64 // compressed frame start in blob
		hash            []byte // sha256 of the compressed frame
	}

	// Compress each chunk into an independent zstd frame.
	var (
		chunks      []chunkInfo
		blobBuf     bytes.Buffer
		uncompTotal int64
	)
	for i, sz := range chunkSizes {
		plain := make([]byte, sz)
		if _, err := rand.Read(plain); err != nil {
			t.Fatalf("rand chunk %d: %v", i, err)
		}

		frame := enc.EncodeAll(plain, nil)

		h := sha256.Sum256(frame)
		chunks = append(chunks, chunkInfo{
			compressedFrame: frame,
			uncompLen:       int64(sz),
			uncompOffset:    uncompTotal,
			blobStart:       int64(blobBuf.Len()),
			hash:            h[:],
		})
		blobBuf.Write(frame)
		uncompTotal += int64(sz)
	}

	// ── Build the chunk-index payload (32-byte header + N × 48-byte entry) ──
	//
	// Variable-mode entry shape: BlockOffset(8) + UncompressedOffset(8) +
	// SHA-256 Checksum(32) = 48 bytes each (erofs-image-spec §3.4).
	const (
		hashAlgoSHA2 = uint8(1)
		hashSize     = uint8(32)
		testEntrySize = 8 + 8 + int(hashSize) // 48 bytes
	)

	var idxBuf bytes.Buffer

	// Header (32 bytes, little-endian; erofs-image-spec §3.4.1).
	var hdr [32]byte
	binary.LittleEndian.PutUint32(hdr[0:4], chunkIndexMagic)
	hdr[4] = 1 // Version = 1
	hdr[5] = 1 // CompressionType = zstd
	// hdr[6:8] Flags = 0 (reserved uint16)
	binary.LittleEndian.PutUint64(hdr[8:16], uint64(uncompTotal))
	binary.LittleEndian.PutUint32(hdr[16:20], uint32(len(chunks)))
	hdr[20] = hashAlgoSHA2
	hdr[21] = hashSize
	// hdr[22:32] Reserved = 0
	idxBuf.Write(hdr[:])

	// Entries: one per chunk.
	for _, c := range chunks {
		var entry [testEntrySize]byte
		binary.LittleEndian.PutUint64(entry[0:8], uint64(c.blobStart))     // BlockOffset
		binary.LittleEndian.PutUint64(entry[8:16], uint64(c.uncompOffset)) // UncompressedOffset
		copy(entry[16:48], c.hash)                                          // Checksum (SHA-256)
		idxBuf.Write(entry[:])
	}

	idxPayload := idxBuf.Bytes()

	// ── Wrap chunk-index in a zstd skippable frame ────────────────────────
	// Magic must be in [0x184D2A50, 0x184D2A5F].
	const skippableMagic = uint32(0x184D2A50)
	var frameHdr [8]byte
	binary.LittleEndian.PutUint32(frameHdr[0:4], skippableMagic)
	binary.LittleEndian.PutUint32(frameHdr[4:8], uint32(len(idxPayload)))

	// Record where the skippable frame starts.
	chunkIndexSectionOffset := int64(blobBuf.Len())
	blobBuf.Write(frameHdr[:])
	blobBuf.Write(idxPayload)

	blob := blobBuf.Bytes()

	// ── Compute descriptor digest (sha256 of whole blob) ─────────────────
	blobDigest := digest.FromBytes(blob)

	// ── Compute chunk-index payload digest (org.erofs.index.digest) ───────
	idxDigest := digest.FromBytes(idxPayload)

	// ── Build org.erofs.index.range annotation ────────────────────────────
	// offset = start of skippable frame, end = end of blob.
	chunkIndexEnd := int64(len(blob))
	rangeAnnotation := fmt.Sprintf("%d:%d", chunkIndexSectionOffset, chunkIndexEnd)

	desc := ocispec.Descriptor{
		MediaType: contentindex.MediaTypeEROFSZstd,
		Digest:    blobDigest,
		Size:      int64(len(blob)),
		Annotations: map[string]string{
			contentindex.AnnotationChunkIndexRange:     rangeAnnotation,
			contentindex.AnnotationChunkIndexDigest:    idxDigest.String(),
			contentindex.AnnotationChunkIndexMediaType: contentindex.ChunkIndexMediaTypeEROFSV1,
		},
	}

	t.Logf("built blob: size=%d chunks=%d chunkIndexOffset=%d idxPayloadSize=%d",
		len(blob), len(chunks), chunkIndexSectionOffset, len(idxPayload))
	t.Logf("  blobDigest=%s", blobDigest)
	t.Logf("  idxDigest=%s rangeAnnotation=%s", idxDigest, rangeAnnotation)

	return blob, desc
}

// readContentEntry reads all bytes from a content-store entry into memory.
func readContentEntry(t *testing.T, ctx context.Context, cs content.Store, dgst digest.Digest) []byte {
	t.Helper()
	ra, err := cs.ReaderAt(ctx, ocispec.Descriptor{Digest: dgst})
	if err != nil {
		t.Fatalf("open content entry %s: %v", dgst, err)
	}
	defer ra.Close()
	buf := make([]byte, ra.Size())
	if _, err := ra.ReadAt(buf, 0); err != nil && err != io.EOF {
		t.Fatalf("read content entry %s: %v", dgst, err)
	}
	return buf
}

// TestRoundTrip_LazyIngestThenReadDigest is gap-fill test #2.
//
// Property: after a blob is brought up via the LAZY ingest path
// (WriteLazy registers metadata + just the chunk-index payload;
// FillBatch then pulls every chunk through a ByteProvider), reading
// the blob back via Store.ReaderAt reproduces the bytes byte-for-byte
// AND the sha256 of the streamed bytes equals the original
// descriptor digest.
//
// Why this matters: the lazy path reassembles the blob from per-chunk
// content-store entries + the cached trailer.  TestRoundTrip_ZstdChunkedBlob
// covers the same equality for the EAGER path (Writer/Commit of the
// full blob), but the lazy path is a different code path through
// `assembledReader` whose per-chunk segment lookups read from
// different bolt buckets and use a different in-memory chunk-ref list.
// Without this test, a chunk-segment lookup bug (off-by-one,
// misordered chunks, dropped trailer) would only surface during an
// actual pull-and-run, not in unit tests.
//
// We additionally verify the index.OpenReader convenience: streaming
// reads via io.Copy reproduce the same digest as ReadAt-based reads,
// proving the io.NewSectionReader wrapping doesn't introduce any
// drift.
func TestRoundTrip_LazyIngestThenReadDigest(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")

	cs, err := localcs.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new content store: %v", err)
	}
	store := newTestStore(t, cs)

	// Mix small and larger chunks so we exercise the multi-chunk
	// reassembly with non-uniform segment sizes.
	blob, desc := buildZstdChunkedBlob(t, []int{4 * 1024, 16 * 1024, 64 * 1024, 8 * 1024})
	t.Logf("blob: size=%d digest=%s", len(blob), desc.Digest)

	// ── Lazy ingest: chunk-index only, no chunk bytes yet ─────────
	p := &memProvider{name: "lazy-roundtrip", blob: blob}
	if err := store.WriteLazy(ctx, "lazy-"+desc.Digest.String(), desc, p); err != nil {
		t.Fatalf("WriteLazy: %v", err)
	}

	// Confirm the blob is registered but every chunk is missing.
	info, err := store.Info(ctx, desc.Digest)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Size != int64(len(blob)) {
		t.Errorf("Info.Size = %d, want %d", info.Size, len(blob))
	}
	missing, err := store.MissingChunks(ctx, desc.Digest)
	if err != nil {
		t.Fatalf("MissingChunks: %v", err)
	}
	if len(missing) == 0 {
		t.Fatal("MissingChunks returned 0 — lazy ingest must leave chunks unpresent")
	}
	t.Logf("after WriteLazy: %d chunks missing", len(missing))

	// ── Fill every chunk via the batched fetch path ───────────────
	all, err := store.AllChunks(ctx, desc.Digest)
	if err != nil {
		t.Fatalf("AllChunks: %v", err)
	}
	idxs := make([]int, len(all))
	for i := range all {
		idxs[i] = i
	}
	if err := store.FillBatch(ctx, desc.Digest, idxs, p, contentindex.PriorityForeground); err != nil {
		t.Fatalf("FillBatch: %v", err)
	}
	missingAfter, _ := store.MissingChunks(ctx, desc.Digest)
	if len(missingAfter) != 0 {
		t.Fatalf("after FillBatch: %d chunks still missing", len(missingAfter))
	}

	// ── Property 1: ReadAt reproduces blob byte-for-byte ──────────
	ra, err := store.ReaderAt(ctx, desc)
	if err != nil {
		t.Fatalf("ReaderAt: %v", err)
	}
	defer ra.Close()
	if ra.Size() != int64(len(blob)) {
		t.Fatalf("ra.Size() = %d, want %d", ra.Size(), len(blob))
	}
	got := make([]byte, len(blob))
	if _, err := ra.ReadAt(got, 0); err != nil && err != io.EOF {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, blob) {
		for i := range blob {
			if got[i] != blob[i] {
				t.Fatalf("byte mismatch at offset %d after lazy reassembly: got 0x%02x want 0x%02x",
					i, got[i], blob[i])
			}
		}
	}

	// ── Property 2: sha256(ReadAt bytes) == desc.Digest ───────────
	gotDigest := digest.FromBytes(got)
	if gotDigest != desc.Digest {
		t.Fatalf("lazy-path digest mismatch:\n  desc.Digest    = %s\n  sha256(ReadAt) = %s",
			desc.Digest, gotDigest)
	}

	// ── Property 3: streaming via index.OpenReader matches too ────
	// io.Copy through a SectionReader wraps differently than ReadAt;
	// asserting both gives identical results catches regressions in
	// the wrapping helper.
	rc, err := contentindex.OpenReader(ctx, store, desc)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer rc.Close()
	h := sha256.New()
	n, err := io.Copy(h, rc)
	if err != nil {
		t.Fatalf("io.Copy through OpenReader: %v", err)
	}
	if n != int64(len(blob)) {
		t.Errorf("OpenReader copied %d bytes, want %d", n, len(blob))
	}
	streamDigest := digest.NewDigest(digest.SHA256, h)
	if streamDigest != desc.Digest {
		t.Fatalf("OpenReader streaming digest mismatch: got %s want %s", streamDigest, desc.Digest)
	}
	t.Logf("lazy reassembly + streaming digest: %s ✓", streamDigest)
}
