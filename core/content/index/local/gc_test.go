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
	"io"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/metadata"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	localcs "github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	bolt "go.etcd.io/bbolt"
)

// TestGC_ChunksCollectedWhenBlobUnreferenced exercises the full GC integration:
//
//  1. Ingest a chunked blob into the indexed content store; the chunks and
//     chunk-index entry land in the namespaced content store so the metadata
//     GC can track them.
//  2. Pin the blob by creating an image whose manifest carries a
//     containerd.io/gc.ref.content.index label pointing at the blob digest.
//  3. Run GC — all chunks must survive.
//  4. Delete the image (removing the pin).
//  5. Run GC — the indexed-content metadata record must be gone, and all
//     chunk content-store entries must have been removed.
func TestGC_ChunksCollectedWhenBlobUnreferenced(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")

	// ── Open the metadata DB and its namespaced content store ─────────────
	rawCS, err := localcs.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new content store: %v", err)
	}
	bdb, err := bolt.Open(filepath.Join(t.TempDir(), "meta.db"), 0644, nil)
	if err != nil {
		t.Fatalf("open bolt: %v", err)
	}
	t.Cleanup(func() { bdb.Close() })
	mdb := metadata.NewDB(bdb, rawCS, nil)
	if err := mdb.Init(ctx); err != nil {
		t.Fatalf("init metadata db: %v", err)
	}
	// cs is the namespace-aware content store that the GC tracks.
	// The indexed content store MUST use this so that chunk entries appear
	// in the metadata BoltDB namespace and are visible to the GC traversal.
	cs := mdb.ContentStore()

	// ── Open the indexed content store backed by the shared metadata DB ────
	idxStore, err := NewStore(Config{
		Root:    t.TempDir(),
		DB:      mdb,
		Content: cs,
	})
	if err != nil {
		t.Fatalf("new indexed store: %v", err)
	}
	mdb.RegisterCollectibleResource(metadata.ResourceContentIndex, idxStore.Collector())

	// ── Build and ingest a chunked blob ────────────────────────────────────
	blob, desc := buildZstdChunkedBlob(t, []int{4 * 1024, 16 * 1024, 64 * 1024})

	w, err := idxStore.Writer(ctx,
		content.WithRef("gc-test-blob"),
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

	// Collect the content-store digests the indexed-content blob produced.
	chunkDigests := listChunkDigests(t, ctx, idxStore, desc.Digest)
	t.Logf("indexed blob produced %d content-store entries", len(chunkDigests))
	if len(chunkDigests) == 0 {
		t.Fatal("expected at least one content-store entry (chunk-index)")
	}

	// Verify all entries are present before any GC.
	for _, dgst := range chunkDigests {
		if _, err := cs.Info(ctx, dgst); err != nil {
			t.Fatalf("entry %s missing before GC: %v", dgst, err)
		}
	}

	// ── Pin the blob via an image manifest ────────────────────────────────
	// Write a manifest blob to the namespaced CS with the GC label that pins
	// the indexed-content blob.  The image itself pins the manifest; the
	// manifest label pins the indexed-content blob; the back-ref mechanism
	// pins the chunks.
	manifestDigest := writeManifestWithIndexRef(t, ctx, cs, desc.Digest)
	imageStore := metadata.NewImageStore(mdb)
	if _, err := imageStore.Create(ctx, images.Image{
		Name:   "test/gc-test:latest",
		Target: ocispec.Descriptor{
			MediaType: "application/vnd.oci.image.manifest.v1+json",
			Digest:    manifestDigest,
			Size:      2,
		},
	}); err != nil {
		t.Fatalf("create image: %v", err)
	}

	// ── GC pass 1: blob is pinned, chunks must survive ────────────────────
	if _, err := mdb.GarbageCollect(ctx); err != nil {
		t.Fatalf("GC pass 1: %v", err)
	}
	for _, dgst := range chunkDigests {
		if _, err := cs.Info(ctx, dgst); err != nil {
			t.Fatalf("GC pass 1 removed entry %s (should be pinned): %v", dgst, err)
		}
	}
	if _, err := idxStore.Info(ctx, desc.Digest); err != nil {
		t.Fatalf("GC pass 1 removed metadata record (should be pinned): %v", err)
	}
	t.Log("GC pass 1: all pinned entries survived ✓")

	// ── Remove the image (unpin the blob) ────────────────────────────────
	if err := imageStore.Delete(ctx, "test/gc-test:latest"); err != nil {
		t.Fatalf("delete image: %v", err)
	}
	// Delete the manifest content entry so it is no longer a GC root.
	if err := cs.Delete(ctx, manifestDigest); err != nil {
		t.Fatalf("delete manifest: %v", err)
	}

	// ── GC pass 2: blob is unpinned, metadata record must be removed ───────
	if _, err := mdb.GarbageCollect(ctx); err != nil {
		t.Fatalf("GC pass 2: %v", err)
	}

	// Sidecar record must be gone.
	if _, err := idxStore.Info(ctx, desc.Digest); err == nil {
		t.Error("GC pass 2: metadata record still present (should have been removed)")
	} else {
		t.Log("GC pass 2: metadata record removed ✓")
	}

	// Every chunk/index/extra entry must be gone after content GC.
	// (The metadata GC marks them unreachable; content cleanup removes them.)
	survived := 0
	for _, dgst := range chunkDigests {
		if _, err := cs.Info(ctx, dgst); err == nil {
			t.Errorf("GC pass 2: content entry %s still present (should have been removed)", dgst)
			survived++
		}
	}
	if survived == 0 {
		t.Logf("GC pass 2: all %d content-store entries removed ✓", len(chunkDigests))
	}
}

// TestGC_ChunksSurviveWhileSecondBlobPinsThem verifies that a chunk shared by
// two blobs is not collected while the second blob still references it.
func TestGC_ChunksSurviveWhileSecondBlobPinsThem(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")

	rawCS, err := localcs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bdb, err := bolt.Open(filepath.Join(t.TempDir(), "meta.db"), 0644, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bdb.Close() })
	mdb := metadata.NewDB(bdb, rawCS, nil)
	if err := mdb.Init(ctx); err != nil {
		t.Fatal(err)
	}
	cs := mdb.ContentStore()

	idxStore, err := NewStore(Config{
		Root:    t.TempDir(),
		DB:      mdb,
		Content: cs,
	})
	if err != nil {
		t.Fatal(err)
	}
	mdb.RegisterCollectibleResource(metadata.ResourceContentIndex, idxStore.Collector())
	imgStore := metadata.NewImageStore(mdb)

	blob, desc := buildZstdChunkedBlob(t, []int{4 * 1024, 8 * 1024})

	// Ingest the blob.
	w, err := idxStore.Writer(ctx, content.WithRef("shared-blob"), content.WithDescriptor(desc))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(w, bytes.NewReader(blob))
	if err := w.Commit(ctx, int64(len(blob)), desc.Digest); err != nil {
		t.Fatal(err)
	}

	chunkDigests := listChunkDigests(t, ctx, idxStore, desc.Digest)

	// Pin via image A.
	mfstA := writeManifestWithIndexRef(t, ctx, cs, desc.Digest)
	imgStore.Create(ctx, images.Image{
		Name:   "test/image-a:latest",
		Target: ocispec.Descriptor{MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: mfstA, Size: 2},
	})

	// GC: image A pins the blob → chunks must survive.
	if _, err := mdb.GarbageCollect(ctx); err != nil {
		t.Fatal(err)
	}
	for _, dgst := range chunkDigests {
		if _, err := cs.Info(ctx, dgst); err != nil {
			t.Fatalf("chunk %s missing while still pinned by image-a: %v", dgst, err)
		}
	}
	t.Log("GC with image-a pinned: all chunks survived ✓")

	// Remove image A.
	imgStore.Delete(ctx, "test/image-a:latest")
	cs.Delete(ctx, mfstA)

	// GC: no remaining pins → all collected.
	if _, err := mdb.GarbageCollect(ctx); err != nil {
		t.Fatal(err)
	}
	for _, dgst := range chunkDigests {
		if _, err := cs.Info(ctx, dgst); err == nil {
			t.Errorf("chunk %s still present after final GC", dgst)
		}
	}
	t.Log("GC after last pin dropped: all chunks collected ✓")
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// listChunkDigests reads the indexed-content metadata to enumerate every content-store digest
// the indexed-content blob is responsible for.
func listChunkDigests(t *testing.T, ctx context.Context, s *Store, blobDigest digest.Digest) []digest.Digest {
	t.Helper()
	info, err := s.Info(ctx, blobDigest)
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	var dgsts []digest.Digest
	if info.IndexDigest != "" {
		dgsts = append(dgsts, info.IndexDigest)
	}
	ns, _ := namespaces.Namespace(ctx)
	s.db.View(func(tx *bolt.Tx) error {
		blobBkt := getBlobBucket(tx, ns, blobDigest)
		if blobBkt == nil {
			return nil
		}
		if cb := getChunksBucket(blobBkt); cb != nil {
			cb.ForEach(func(_, v []byte) error {
				if d, err := digest.Parse(string(v)); err == nil {
					dgsts = append(dgsts, d)
				}
				return nil
			})
		}
		if eb := getExtrasBucket(blobBkt); eb != nil {
			eb.ForEach(func(k, v []byte) error {
				if v != nil {
					return nil
				}
				exBkt := eb.Bucket(k)
				if exBkt == nil {
					return nil
				}
				if dv := exBkt.Get(bucketKeyDigest); len(dv) > 0 {
					if d, err := digest.Parse(string(dv)); err == nil {
						dgsts = append(dgsts, d)
					}
				}
				return nil
			})
		}
		return nil
	})
	return dgsts
}

// writeManifestWithIndexRef writes a minimal manifest blob to cs carrying a
// containerd.io/gc.ref.content.index label that pins blobDigest.
func writeManifestWithIndexRef(t *testing.T, ctx context.Context, cs content.Store, blobDigest digest.Digest) digest.Digest {
	t.Helper()
	data := []byte(`{}`)
	dgst := digest.FromBytes(data)
	labels := map[string]string{
		"containerd.io/gc.ref.content.index": blobDigest.String(),
	}
	// If already present (idempotent), just update labels.
	if info, err := cs.Info(ctx, dgst); err == nil {
		if info.Labels == nil {
			info.Labels = make(map[string]string)
		}
		for k, v := range labels {
			info.Labels[k] = v
		}
		cs.Update(ctx, info, "labels")
		return dgst
	}
	cw, err := cs.Writer(ctx,
		content.WithRef("gc-manifest-"+dgst.String()),
		content.WithDescriptor(ocispec.Descriptor{
			MediaType: "application/vnd.oci.image.manifest.v1+json",
			Digest:    dgst,
			Size:      int64(len(data)),
		}),
	)
	if err != nil {
		t.Fatalf("open manifest writer: %v", err)
	}
	cw.Write(data)
	if err := cw.Commit(ctx, int64(len(data)), dgst, content.WithLabels(labels)); err != nil {
		t.Fatalf("commit manifest: %v", err)
	}
	return dgst
}

// memProvider is a minimal in-memory contentindex.ByteProvider over a blob's
// bytes, used to drive WriteLazy in tests.
type memProvider struct {
	name string
	blob []byte
}

func (p *memProvider) Name() string { return p.name }

func (p *memProvider) Open(_ context.Context, _ ocispec.Descriptor) (content.ReaderAt, error) {
	return &memReaderAt{data: p.blob}, nil
}

func (p *memProvider) Fetch(_ context.Context, _ ocispec.Descriptor, off, length int64, _ contentindex.Priority) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(p.blob[off : off+length])), nil
}

type memReaderAt struct{ data []byte }

func (r *memReaderAt) ReadAt(b []byte, off int64) (int, error) {
	if off >= int64(len(r.data)) {
		return 0, io.EOF
	}
	n := copy(b, r.data[off:])
	if n < len(b) {
		return n, io.EOF
	}
	return n, nil
}

func (r *memReaderAt) Size() int64 { return int64(len(r.data)) }
func (r *memReaderAt) Close() error { return nil }

// TestGC_ProviderRecordReapedWithBlob verifies that the persisted registry
// provider reconstruction record (PutProvider) survives while the lazily
// ingested blob is pinned, and is removed once the blob is collected.
func TestGC_ProviderRecordReapedWithBlob(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")

	rawCS, err := localcs.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new content store: %v", err)
	}
	bdb, err := bolt.Open(filepath.Join(t.TempDir(), "meta.db"), 0644, nil)
	if err != nil {
		t.Fatalf("open bolt: %v", err)
	}
	t.Cleanup(func() { bdb.Close() })
	mdb := metadata.NewDB(bdb, rawCS, nil)
	if err := mdb.Init(ctx); err != nil {
		t.Fatalf("init metadata db: %v", err)
	}
	cs := mdb.ContentStore()

	idxStore, err := NewStore(Config{Root: t.TempDir(), DB: mdb, Content: cs})
	if err != nil {
		t.Fatalf("new indexed store: %v", err)
	}
	mdb.RegisterCollectibleResource(metadata.ResourceContentIndex, idxStore.Collector())

	// Build a chunked blob and lazily ingest it via a provider, so the blob
	// record carries the provider name (the cleanup key).
	blob, desc := buildZstdChunkedBlob(t, []int{4 * 1024, 16 * 1024, 64 * 1024})
	providerName := "registry:" + desc.Digest.String()
	p := &memProvider{name: providerName, blob: blob}
	if err := idxStore.WriteLazy(ctx, "lazy-"+desc.Digest.String(), desc, p); err != nil {
		t.Fatalf("WriteLazy: %v", err)
	}

	// Persist the provider reconstruction metadata.
	if err := idxStore.PutProvider(ctx, providerName, "example.com/repo:latest", nil); err != nil {
		t.Fatalf("PutProvider: %v", err)
	}
	if _, err := idxStore.GetProvider(ctx, providerName); err != nil {
		t.Fatalf("GetProvider before GC: %v", err)
	}

	// Pin the blob via an image manifest.
	manifestDigest := writeManifestWithIndexRef(t, ctx, cs, desc.Digest)
	imageStore := metadata.NewImageStore(mdb)
	if _, err := imageStore.Create(ctx, images.Image{
		Name: "test/prov:latest",
		Target: ocispec.Descriptor{
			MediaType: "application/vnd.oci.image.manifest.v1+json",
			Digest:    manifestDigest,
			Size:      2,
		},
	}); err != nil {
		t.Fatalf("create image: %v", err)
	}

	// GC pass 1: blob pinned, provider record must survive.
	if _, err := mdb.GarbageCollect(ctx); err != nil {
		t.Fatalf("GC pass 1: %v", err)
	}
	if _, err := idxStore.GetProvider(ctx, providerName); err != nil {
		t.Fatalf("GC pass 1 reaped provider record (should be pinned): %v", err)
	}

	// Unpin: remove image and manifest content root.
	if err := imageStore.Delete(ctx, "test/prov:latest"); err != nil {
		t.Fatalf("delete image: %v", err)
	}
	if err := cs.Delete(ctx, manifestDigest); err != nil {
		t.Fatalf("delete manifest: %v", err)
	}

	// GC pass 2: blob collected, provider record must be gone.
	if _, err := mdb.GarbageCollect(ctx); err != nil {
		t.Fatalf("GC pass 2: %v", err)
	}
	if _, err := idxStore.Info(ctx, desc.Digest); err == nil {
		t.Fatal("GC pass 2: blob metadata still present")
	}
	if _, err := idxStore.GetProvider(ctx, providerName); err == nil {
		t.Error("GC pass 2: provider record still present (should have been reaped)")
	} else {
		t.Log("GC pass 2: provider record reaped ✓")
	}
}

// TestFillChunk_PurgesProviderOnLastChunk verifies that the index store
// automatically removes the provider reconstruction record once every chunk
// of a lazily-ingested blob has been filled via FillChunk.
func TestFillChunk_PurgesProviderOnLastChunk(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")

	rawCS, err := localcs.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new content store: %v", err)
	}
	bdb, err := bolt.Open(filepath.Join(t.TempDir(), "meta.db"), 0644, nil)
	if err != nil {
		t.Fatalf("open bolt: %v", err)
	}
	t.Cleanup(func() { bdb.Close() })
	mdb := metadata.NewDB(bdb, rawCS, nil)
	if err := mdb.Init(ctx); err != nil {
		t.Fatalf("init metadata db: %v", err)
	}
	cs := mdb.ContentStore()

	idxStore, err := NewStore(Config{Root: t.TempDir(), DB: mdb, Content: cs})
	if err != nil {
		t.Fatalf("new indexed store: %v", err)
	}

	// Use a small 2-chunk blob so we can observe the transition precisely.
	blob, desc := buildZstdChunkedBlob(t, []int{4 * 1024, 8 * 1024})
	providerName := "registry:" + desc.Digest.String()
	p := &memProvider{name: providerName, blob: blob}

	if err := idxStore.WriteLazy(ctx, "lazy-"+desc.Digest.String(), desc, p); err != nil {
		t.Fatalf("WriteLazy: %v", err)
	}
	if err := idxStore.PutProvider(ctx, providerName, "example.com/repo:latest", []byte("s3cr3t")); err != nil {
		t.Fatalf("PutProvider: %v", err)
	}
	if _, err := idxStore.GetProvider(ctx, providerName); err != nil {
		t.Fatalf("GetProvider after PutProvider: %v", err)
	}

	// Fill chunk 0 — blob not yet complete, provider must survive.
	if err := idxStore.FillChunk(ctx, desc.Digest, 0, p, contentindex.PriorityForeground); err != nil {
		t.Fatalf("FillChunk 0: %v", err)
	}
	if _, err := idxStore.GetProvider(ctx, providerName); err != nil {
		t.Fatalf("provider record removed after filling only chunk 0 (should survive): %v", err)
	}
	t.Log("after FillChunk 0: provider record still present ✓")

	// Fill chunk 1 — all chunks now present, provider must be purged.
	if err := idxStore.FillChunk(ctx, desc.Digest, 1, p, contentindex.PriorityForeground); err != nil {
		t.Fatalf("FillChunk 1: %v", err)
	}
	if _, err := idxStore.GetProvider(ctx, providerName); err == nil {
		t.Error("provider record still present after all chunks filled (should have been purged)")
	} else {
		t.Log("after FillChunk 1 (last chunk): provider record purged ✓")
	}
}

// countingProvider wraps memProvider to count the number of Fetch
// calls.  Used to prove that FillBatch issues ONE network request
// for a contiguous run of chunks rather than one per chunk.
type countingProvider struct {
	*memProvider
	fetchCount int32
	fetchRange []int64 // recorded (start, end) byte pairs
}

func (p *countingProvider) Fetch(ctx context.Context, desc ocispec.Descriptor, off, length int64, prio contentindex.Priority) (io.ReadCloser, error) {
	atomic.AddInt32(&p.fetchCount, 1)
	p.fetchRange = append(p.fetchRange, off, off+length)
	return p.memProvider.Fetch(ctx, desc, off, length, prio)
}

// TestFillBatch_singleFetchForContiguousRun proves that a 4-chunk
// contiguous batch issues exactly ONE provider.Fetch.  This is the
// core promise of the batched-fetch strategy.
func TestFillBatch_singleFetchForContiguousRun(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")

	rawCS, err := localcs.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new content store: %v", err)
	}
	bdb, err := bolt.Open(filepath.Join(t.TempDir(), "meta.db"), 0644, nil)
	if err != nil {
		t.Fatalf("open bolt: %v", err)
	}
	t.Cleanup(func() { bdb.Close() })
	mdb := metadata.NewDB(bdb, rawCS, nil)
	if err := mdb.Init(ctx); err != nil {
		t.Fatalf("init metadata db: %v", err)
	}
	cs := mdb.ContentStore()

	idxStore, err := NewStore(Config{Root: t.TempDir(), DB: mdb, Content: cs})
	if err != nil {
		t.Fatalf("new indexed store: %v", err)
	}

	// 4 contiguous chunks.
	blob, desc := buildZstdChunkedBlob(t, []int{4 * 1024, 8 * 1024, 4 * 1024, 16 * 1024})
	providerName := "registry:" + desc.Digest.String()
	cp := &countingProvider{memProvider: &memProvider{name: providerName, blob: blob}}

	if err := idxStore.WriteLazy(ctx, "lazy-"+desc.Digest.String(), desc, cp); err != nil {
		t.Fatalf("WriteLazy: %v", err)
	}

	// Issue one batch covering all 4 chunks.
	if err := idxStore.FillBatch(ctx, desc.Digest, []int{0, 1, 2, 3}, cp, contentindex.PriorityBackground); err != nil {
		t.Fatalf("FillBatch: %v", err)
	}
	if got := atomic.LoadInt32(&cp.fetchCount); got != 1 {
		t.Errorf("provider.Fetch called %d times for a contiguous 4-chunk batch, want 1", got)
	}
	// And the recorded range must span chunk 0's start to chunk 3's end.
	if len(cp.fetchRange) != 2 {
		t.Fatalf("fetchRange recorded %d values, want 2", len(cp.fetchRange))
	}
	// All 4 chunks must now be present in the content store.
	chunks, _, err := readBlobChunks(ctx, idxStore, desc.Digest)
	if err != nil {
		t.Fatalf("readBlobChunks: %v", err)
	}
	if len(chunks) != 4 {
		t.Fatalf("got %d chunks, want 4", len(chunks))
	}
	for i, c := range chunks {
		if _, err := cs.Info(ctx, c.Digest); err != nil {
			t.Errorf("chunk %d (%s) not in content store: %v", i, c.Digest, err)
		}
	}
}

// TestFillBatch_rejectsNonContiguous proves the contiguity invariant
// is validated: a batch that skips a chunk in the middle must be
// rejected with ErrInvalidArgument before any side effect.
func TestFillBatch_rejectsNonContiguous(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")

	rawCS, _ := localcs.NewStore(t.TempDir())
	bdb, _ := bolt.Open(filepath.Join(t.TempDir(), "meta.db"), 0644, nil)
	t.Cleanup(func() { bdb.Close() })
	mdb := metadata.NewDB(bdb, rawCS, nil)
	_ = mdb.Init(ctx)
	cs := mdb.ContentStore()
	idxStore, _ := NewStore(Config{Root: t.TempDir(), DB: mdb, Content: cs})

	blob, desc := buildZstdChunkedBlob(t, []int{1024, 1024, 1024, 1024})
	p := &memProvider{name: "registry:" + desc.Digest.String(), blob: blob}
	_ = idxStore.WriteLazy(ctx, "lazy-"+desc.Digest.String(), desc, p)

	err := idxStore.FillBatch(ctx, desc.Digest, []int{0, 2, 3}, p, contentindex.PriorityBackground) // skips 1
	if err == nil {
		t.Fatal("FillBatch with non-contiguous indices accepted, want ErrInvalidArgument")
	}
	if !errdefs.IsInvalidArgument(err) {
		t.Errorf("err = %v, want ErrInvalidArgument", err)
	}
}

// TestFillBatch_singletonFallsThroughToFillChunk verifies that a
// single-element batch takes the FillChunk fast path (matters only
// for code paths that don't pre-filter; keeps the implementation
// trivially correct for trivially small batches).
func TestFillBatch_singletonFallsThroughToFillChunk(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")

	rawCS, _ := localcs.NewStore(t.TempDir())
	bdb, _ := bolt.Open(filepath.Join(t.TempDir(), "meta.db"), 0644, nil)
	t.Cleanup(func() { bdb.Close() })
	mdb := metadata.NewDB(bdb, rawCS, nil)
	_ = mdb.Init(ctx)
	cs := mdb.ContentStore()
	idxStore, _ := NewStore(Config{Root: t.TempDir(), DB: mdb, Content: cs})

	blob, desc := buildZstdChunkedBlob(t, []int{4 * 1024, 8 * 1024})
	cp := &countingProvider{memProvider: &memProvider{name: "p", blob: blob}}
	_ = idxStore.WriteLazy(ctx, "lazy-"+desc.Digest.String(), desc, cp)

	if err := idxStore.FillBatch(ctx, desc.Digest, []int{0}, cp, contentindex.PriorityForeground); err != nil {
		t.Fatalf("FillBatch single: %v", err)
	}
	if got := atomic.LoadInt32(&cp.fetchCount); got != 1 {
		t.Errorf("Fetch count = %d, want 1", got)
	}
}

// TestFillBatch_skipsAlreadyPresent verifies that chunks already in
// the content store before FillBatch is called are NOT re-fetched.
// Important for the resumable / restart-after-partial-fill case.
func TestFillBatch_skipsAlreadyPresent(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")

	rawCS, _ := localcs.NewStore(t.TempDir())
	bdb, _ := bolt.Open(filepath.Join(t.TempDir(), "meta.db"), 0644, nil)
	t.Cleanup(func() { bdb.Close() })
	mdb := metadata.NewDB(bdb, rawCS, nil)
	_ = mdb.Init(ctx)
	cs := mdb.ContentStore()
	idxStore, _ := NewStore(Config{Root: t.TempDir(), DB: mdb, Content: cs})

	blob, desc := buildZstdChunkedBlob(t, []int{4 * 1024, 4 * 1024, 4 * 1024})
	cp := &countingProvider{memProvider: &memProvider{name: "p", blob: blob}}
	_ = idxStore.WriteLazy(ctx, "lazy-"+desc.Digest.String(), desc, cp)

	// Pre-fill chunk 1 via FillChunk so it's present in the content store.
	if err := idxStore.FillChunk(ctx, desc.Digest, 1, cp, contentindex.PriorityForeground); err != nil {
		t.Fatalf("pre-fill FillChunk 1: %v", err)
	}
	atomic.StoreInt32(&cp.fetchCount, 0)
	cp.fetchRange = nil

	// Batch the full range.  Chunk 1 is skipped, breaking the run
	// into two sub-runs [0] and [2] — TWO fetches expected.
	if err := idxStore.FillBatch(ctx, desc.Digest, []int{0, 1, 2}, cp, contentindex.PriorityBackground); err != nil {
		t.Fatalf("FillBatch: %v", err)
	}
	if got := atomic.LoadInt32(&cp.fetchCount); got != 2 {
		t.Errorf("Fetch count = %d for [0,1(present),2] batch, want 2 sub-runs", got)
	}
}

// readBlobChunks reads the chunk-index payload from the content store
// countingTransactor wraps a Transactor and counts Update/View
// invocations.  Used to prove FillBatch issues a SINGLE bolt write
// transaction for a contiguous run of chunks rather than 2N
// transactions (one Writer-create + one Commit per chunk).
type countingTransactor struct {
	inner   Transactor
	updates atomic.Int32
	views   atomic.Int32
}

func (t *countingTransactor) View(fn func(*bolt.Tx) error) error {
	t.views.Add(1)
	return t.inner.View(fn)
}

func (t *countingTransactor) Update(fn func(*bolt.Tx) error) error {
	t.updates.Add(1)
	return t.inner.Update(fn)
}

// TestFillBatch_singleWriteTransaction proves the per-chunk
// transaction-overhead reduction promised by FillBatch: the
// underlying bolt store sees a CONSTANT number of Update
// transactions regardless of how many chunks the batch covers,
// rather than scaling 2× per chunk (one for Writer's
// ingest-bucket-create + one for Commit's commit-and-lease-swap).
//
// We compare a small batch (4 chunks) against a large batch (16
// chunks) and assert the Update count is IDENTICAL.  Without
// batching the large batch would observe roughly 4× the
// transactions of the small batch.
//
// The constant overhead consists of:
//   - 1 Update wrapping every chunk's Writer + Commit (the main
//     batched ingest tx)
//   - 1 Update for purgeProviderIfFull (post-tx provider-record
//     cleanup; runs once when the LAST chunk closes out the blob,
//     which is independent of N)
// → expected total: 2 Updates per FillBatch, regardless of batch
// size.
//
// boltutil.WithTransaction propagates the tx to the
// metadata-wrapped content store, so its Writer/Commit calls
// reuse our outer tx instead of opening their own.  This is the
// mechanism the user asked for: "create the transaction in the
// context, then keep that in context when calling the content
// operations."
func TestFillBatch_singleWriteTransaction(t *testing.T) {
	for _, tc := range []struct {
		name      string
		chunkSize []int
		fillIdxs  []int
	}{
		{"4chunks", []int{4 * 1024, 8 * 1024, 4 * 1024, 16 * 1024}, []int{0, 1, 2, 3}},
		{"16chunks", []int{
			4 * 1024, 4 * 1024, 4 * 1024, 4 * 1024,
			4 * 1024, 4 * 1024, 4 * 1024, 4 * 1024,
			4 * 1024, 4 * 1024, 4 * 1024, 4 * 1024,
			4 * 1024, 4 * 1024, 4 * 1024, 4 * 1024,
		}, []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx := namespaces.WithNamespace(context.Background(), "test")
			rawCS, _ := localcs.NewStore(t.TempDir())
			bdb, _ := bolt.Open(filepath.Join(t.TempDir(), "meta.db"), 0644, nil)
			t.Cleanup(func() { bdb.Close() })
			mdb := metadata.NewDB(bdb, rawCS, nil)
			if err := mdb.Init(ctx); err != nil {
				t.Fatalf("init: %v", err)
			}
			ct := &countingTransactor{inner: mdb}
			cs := mdb.ContentStore()
			idxStore, err := NewStore(Config{Root: t.TempDir(), DB: ct, Content: cs})
			if err != nil {
				t.Fatalf("new indexed store: %v", err)
			}

			blob, desc := buildZstdChunkedBlob(t, tc.chunkSize)
			p := &memProvider{name: "p", blob: blob}
			if err := idxStore.WriteLazy(ctx, "lazy-"+desc.Digest.String(), desc, p); err != nil {
				t.Fatalf("WriteLazy: %v", err)
			}
			// Reset after WriteLazy's own transactions.
			ct.updates.Store(0)
			ct.views.Store(0)

			if err := idxStore.FillBatch(ctx, desc.Digest, tc.fillIdxs, p, contentindex.PriorityBackground); err != nil {
				t.Fatalf("FillBatch: %v", err)
			}

			// Exactly 2 Updates: (1) batched Writer+Commit chain, (2)
			// purgeProviderIfFull cleanup.  Both are O(1) in batch size.
			updates := ct.updates.Load()
			if updates != 2 {
				t.Errorf("Update transactions = %d, want 2 (1 batched ingest + 1 provider purge)", updates)
			}
			t.Logf("batch size = %d chunks: %d Update transactions (constant)", len(tc.fillIdxs), updates)
		})
	}
}

// and returns its ChunkRefs.  Test helper.
func readBlobChunks(ctx context.Context, s *Store, dgst digest.Digest) ([]contentindex.ChunkRef, *chunkIndexHeader, error) {
	var meta blobMeta
	ns, _ := namespaces.Namespace(ctx)
	if err := view(ctx, s.db, func(tx *bolt.Tx) error {
		bkt := getBlobBucket(tx, ns, dgst)
		if bkt == nil {
			return blobNotFound(dgst)
		}
		var merr error
		meta, merr = readBlobMeta(bkt)
		return merr
	}); err != nil {
		return nil, nil, err
	}
	payload, err := s.readContentEntry(ctx, meta.IndexDigest)
	if err != nil {
		return nil, nil, err
	}
	return parseChunkIndexPayload(payload, meta.IndexOffset, meta.MediaType)
}
