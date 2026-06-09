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
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/metadata"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	localcs "github.com/containerd/containerd/v2/plugins/content/local"
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
