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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/content/index/chunked"
	"github.com/containerd/containerd/v2/core/content/index/registry"
	"github.com/containerd/containerd/v2/core/content/index/testutil"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	localcs "github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	ocispecv "github.com/opencontainers/image-spec/specs-go"
)

// TestRoundTrip_ConvertPushPullUnpack is the full end-to-end test:
//
//  1. Build a chunked +zstd blob from synthetic "image data" using the
//     chunked.Build helper (simulating what the converter does).
//  2. Push the blob and a minimal OCI manifest to an in-memory registry.
//  3. Pull the blob from the registry and ingest it into the indexed content
//     store via the registry provider.
//  4. Use the indexed store's ReaderAt (the assembled reader) to reproduce
//     the original blob byte-for-byte.
//  5. Verify the sequential digest matches the pushed blob's descriptor digest.
//  6. Verify that each chunk can be read independently via the assembled reader
//     (simulating the per-chunk fetch path used by the erofs differ).
func TestRoundTrip_ConvertPushPullUnpack(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")

	// ── Content store and indexed store ───────────────────────────────────
	cs, err := localcs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	idxStore := newTestStore(t, cs)

	// ── Step 1: Build the chunked blob ────────────────────────────────────
	// Simulate "image data" as random bytes (would normally be an EROFS image).
	const chunkSize = 4 * 1024 // 4 KiB chunks for the test
	imageSizes := []int{4 * 1024, 16 * 1024, 64 * 1024, 256 * 1024}
	// Total image data = sum of chunk sizes as uncompressed input.
	var imageData []byte
	for _, sz := range imageSizes {
		chunk := make([]byte, sz)
		// Fill with recognisable patterns so we can verify content.
		for i := range chunk {
			chunk[i] = byte(i % 251)
		}
		imageData = append(imageData, chunk...)
	}

	result, err := chunked.Build(
		bytes.NewReader(imageData),
		int64(len(imageData)),
		contentindex.MediaTypeEROFSZstd,
		chunkSize,
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Logf("built blob: size=%d chunks=%d", len(result.Blob), len(result.Chunks))

	// ── Step 2: Push to in-memory registry ───────────────────────────────
	reg := testutil.NewMemRegistry()
	srv := httptest.NewServer(reg)
	t.Cleanup(srv.Close)

	host := srv.Listener.Addr().String()
	repo := "test/myimage"
	tag := "v1"

	resolver := docker.NewResolver(docker.ResolverOptions{
		Hosts: func(h string) ([]docker.RegistryHost, error) {
			return []docker.RegistryHost{{
				Client:       srv.Client(),
				Host:         host,
				Scheme:       "http",
				Capabilities: docker.HostCapabilityPull | docker.HostCapabilityResolve | docker.HostCapabilityPush,
			}}, nil
		},
	})

	// Push the layer blob.
	ref := fmt.Sprintf("%s/%s:%s", host, repo, tag)
	pusher, err := resolver.Pusher(ctx, ref)
	if err != nil {
		t.Fatalf("Pusher: %v", err)
	}
	layerDesc := result.Descriptor
	pushWriter, err := pusher.Push(ctx, layerDesc)
	if err != nil {
		t.Fatalf("Push layer: %v", err)
	}
	if _, err := pushWriter.Write(result.Blob); err != nil {
		t.Fatalf("Write layer: %v", err)
	}
	if err := pushWriter.Commit(ctx, layerDesc.Size, layerDesc.Digest); err != nil {
		t.Fatalf("Commit layer: %v", err)
	}
	t.Logf("pushed layer: %s", layerDesc.Digest)

	// Build and push a minimal OCI image manifest referencing the layer.
	manifest := ocispec.Manifest{
		Versioned: ocispecv.Versioned{SchemaVersion: 2},
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Config: ocispec.Descriptor{
			MediaType: "application/vnd.oci.image.config.v1+json",
			Digest:    digest.FromBytes([]byte("{}")),
			Size:      2,
		},
		Layers: []ocispec.Descriptor{layerDesc},
	}
	manifestJSON, _ := json.Marshal(manifest)
	manifestDesc := ocispec.Descriptor{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Digest:    digest.FromBytes(manifestJSON),
		Size:      int64(len(manifestJSON)),
	}
	manifestPusher, err := resolver.Pusher(ctx, ref)
	if err != nil {
		t.Fatalf("Pusher manifest: %v", err)
	}
	mw, err := manifestPusher.Push(ctx, manifestDesc)
	if err != nil {
		t.Fatalf("Push manifest: %v", err)
	}
	mw.Write(manifestJSON)
	if err := mw.Commit(ctx, manifestDesc.Size, manifestDesc.Digest); err != nil {
		t.Fatalf("Commit manifest: %v", err)
	}
	t.Logf("pushed manifest: %s", manifestDesc.Digest)

	// Verify the registry has both blobs.
	if !reg.HasBlob(layerDesc.Digest.String()) {
		t.Fatal("registry missing layer blob after push")
	}
	if !reg.HasBlob(manifestDesc.Digest.String()) {
		t.Fatal("registry missing manifest blob after push")
	}

	// ── Step 3: Pull from registry and ingest into indexed store ─────────
	// Resolve the layer's fetcher from the registry.
	_, resolvedDesc, err := resolver.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	t.Logf("resolved: %s", resolvedDesc.Digest)

	fetcher, err := resolver.Fetcher(ctx, ref)
	if err != nil {
		t.Fatalf("Fetcher: %v", err)
	}

	// Use the registry provider to download the layer blob.
	provider := registry.New(fetcher, "registry:"+host+"/"+repo, registry.Config{})

	// Open the blob via the provider (downloads it to memory).
	layerRA, err := provider.Open(ctx, layerDesc)
	if err != nil {
		t.Fatalf("provider.Open: %v", err)
	}
	defer layerRA.Close()

	// Ingest the downloaded blob into the indexed content store.
	w, err := idxStore.Writer(ctx,
		content.WithRef("pull-layer"),
		content.WithDescriptor(layerDesc),
	)
	if err != nil {
		t.Fatalf("indexed Writer: %v", err)
	}
	if _, err := io.Copy(w, io.NewSectionReader(layerRA, 0, layerRA.Size())); err != nil {
		t.Fatalf("copy to indexed writer: %v", err)
	}
	if err := w.Commit(ctx, layerDesc.Size, layerDesc.Digest); err != nil {
		t.Fatalf("indexed Commit: %v", err)
	}
	t.Log("pull: blob ingested into indexed store ✓")

	// ── Step 4: ReaderAt reproduces the original blob byte-for-byte ───────
	ra, err := idxStore.ReaderAt(ctx, layerDesc)
	if err != nil {
		t.Fatalf("ReaderAt: %v", err)
	}
	defer ra.Close()

	if ra.Size() != int64(len(result.Blob)) {
		t.Fatalf("assembled reader size %d != blob size %d", ra.Size(), len(result.Blob))
	}

	got := make([]byte, len(result.Blob))
	if _, err := ra.ReadAt(got, 0); err != nil && err != io.EOF {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, result.Blob) {
		for i := range result.Blob {
			if got[i] != result.Blob[i] {
				t.Fatalf("blob mismatch at offset %d: got 0x%02x want 0x%02x", i, got[i], result.Blob[i])
			}
		}
	}
	t.Log("byte-for-byte reproduction: ✓")

	// ── Step 5: Sequential digest matches descriptor digest ──────────────
	h := digest.SHA256.Digester()
	if _, err := io.Copy(h.Hash(), io.NewSectionReader(ra, 0, ra.Size())); err != nil {
		t.Fatalf("hash: %v", err)
	}
	seqDgst := h.Digest()
	if seqDgst != layerDesc.Digest {
		t.Fatalf("sequential digest mismatch: got %s want %s", seqDgst, layerDesc.Digest)
	}
	t.Logf("sequential digest: %s ✓", seqDgst)

	// ── Step 6: Verify per-chunk reads (simulates the erofs differ path) ──
	// The erofs differ calls content.NewReader(ra) which uses the sequential
	// read path.  Here we also verify that small per-chunk reads work.
	for i, chunkRef := range result.Chunks {
		// Read the chunk's on-blob bytes from the assembled reader.
		chunkLen := chunkRef.OnBlobEnd - chunkRef.OnBlobStart
		buf := make([]byte, chunkLen)
		n, err := ra.ReadAt(buf, chunkRef.OnBlobStart)
		if err != nil && err != io.EOF {
			t.Errorf("chunk %d ReadAt: %v", i, err)
			continue
		}
		// Verify it matches the original blob bytes.
		if !bytes.Equal(buf[:n], result.Blob[chunkRef.OnBlobStart:chunkRef.OnBlobEnd]) {
			t.Errorf("chunk %d content mismatch", i)
		}
	}
	t.Logf("per-chunk reads: %d chunks verified ✓", len(result.Chunks))
}

// TestRoundTrip_UnpackSequentialStream verifies that the assembled reader
// produces a sequential stream that the erofs differ can consume.
//
// The erofs differ calls content.NewReader(ra) and passes the resulting
// io.Reader to its decompression/conversion pipeline.  This test verifies
// that reading the full assembled reader sequentially in small chunks (as
// io.Copy does) produces the correct bytes in the correct order.
func TestRoundTrip_UnpackSequentialStream(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")

	cs, err := localcs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	idxStore := newTestStore(t, cs)

	// Build a blob with mixed chunk sizes.
	imageData := make([]byte, 4*1024+16*1024+64*1024)
	for i := range imageData {
		imageData[i] = byte(i % 251)
	}
	result, err := chunked.Build(
		bytes.NewReader(imageData),
		int64(len(imageData)),
		contentindex.MediaTypeEROFSZstd,
		4*1024,
	)
	if err != nil {
		t.Fatal(err)
	}

	// Ingest.
	w, err := idxStore.Writer(ctx, content.WithRef("unpack-test"), content.WithDescriptor(result.Descriptor))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(w, bytes.NewReader(result.Blob))
	if err := w.Commit(ctx, int64(len(result.Blob)), result.Descriptor.Digest); err != nil {
		t.Fatal(err)
	}

	// Get the assembled reader.
	ra, err := idxStore.ReaderAt(ctx, result.Descriptor)
	if err != nil {
		t.Fatal(err)
	}
	defer ra.Close()

	// Read using io.Copy with a small buffer (the real differ reads in 32KiB chunks).
	var buf bytes.Buffer
	sr := io.NewSectionReader(ra, 0, ra.Size())
	copied, err := io.CopyBuffer(&buf, sr, make([]byte, 32*1024))
	if err != nil {
		t.Fatalf("sequential copy: %v", err)
	}
	if copied != int64(len(result.Blob)) {
		t.Fatalf("copied %d bytes, want %d", copied, len(result.Blob))
	}
	if !bytes.Equal(buf.Bytes(), result.Blob) {
		t.Fatal("sequential stream content mismatch")
	}

	// Verify the sequential hash.
	h := digest.SHA256.Digester()
	h.Hash().Write(buf.Bytes())
	if h.Digest() != result.Descriptor.Digest {
		t.Fatalf("sequential digest mismatch: got %s want %s", h.Digest(), result.Descriptor.Digest)
	}
	t.Logf("sequential stream: %d bytes, digest %s ✓", copied, h.Digest())
}

// TestRoundTrip_PushPullViaHTTP verifies push and pull against the in-memory
// registry at the HTTP level, without the indexed content store.  This test
// acts as a sanity check that the MemRegistry correctly implements the OCI
// distribution protocol.
func TestRoundTrip_PushPullViaHTTP(t *testing.T) {
	reg := testutil.NewMemRegistry()
	srv := httptest.NewServer(reg)
	t.Cleanup(srv.Close)

	host := srv.Listener.Addr().String()

	// Push a blob directly.
	blobData := []byte("hello-indexed-content")
	dgst := digest.FromBytes(blobData)

	// POST to initiate upload.
	postURL := fmt.Sprintf("http://%s/v2/myrepo/blobs/uploads/", host)
	resp, err := srv.Client().Post(postURL, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST upload: got %d", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if location == "" {
		t.Fatal("no Location header from POST upload")
	}

	// PUT to complete upload.
	putURL := fmt.Sprintf("http://%s%s?digest=%s", host, location, dgst)
	req, _ := http.NewRequest(http.MethodPut, putURL, bytes.NewReader(blobData))
	resp2, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("PUT upload: got %d", resp2.StatusCode)
	}

	// GET the blob back.
	getURL := fmt.Sprintf("http://%s/v2/myrepo/blobs/%s", host, dgst)
	resp3, err := srv.Client().Get(getURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("GET blob: got %d", resp3.StatusCode)
	}
	gotData, _ := io.ReadAll(resp3.Body)
	if !bytes.Equal(gotData, blobData) {
		t.Fatal("blob content mismatch after push/pull")
	}
	t.Logf("push/pull via HTTP: %s ✓", dgst)
}
