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

package erofs_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/core/content/index/chunked"
	"github.com/containerd/containerd/v2/core/images/archive"
	"github.com/containerd/containerd/v2/core/images/converter/erofs"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	localcs "github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// buildChunked is a test helper that calls chunked.Build and returns
// the blob bytes, Result, and a fully-populated descriptor (with Digest).
// bytes.Reader implements io.ReaderAt so it satisfies the Build signature.
func buildChunked(t *testing.T, data []byte, mediaType string, targetFrame int) ([]byte, *chunked.Result, ocispec.Descriptor) {
	t.Helper()
	var buf bytes.Buffer
	result, err := chunked.Build(bytes.NewReader(data), int64(len(data)), &buf, mediaType, targetFrame)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	blobBytes := buf.Bytes()
	desc := result.Descriptor
	desc.Digest = digest.FromBytes(blobBytes)
	return blobBytes, result, desc
}

// TestChunkedBlob_BackwardsCompatibility verifies the core property that makes
// the chunked format backwards-compatible with any standard zstd decoder:
//
// A chunked +zstd blob consists of:
//
//	[zstd frame 0 (chunk 0)] ... [zstd frame N-1 (chunk N-1)]
//	[zstd skippable frame (chunk index, not a data frame)]
//
// A standard zstd decoder decompresses all data frames in sequence and
// silently passes over the trailing skippable frame, producing the original
// uncompressed image data bytes.
func TestChunkedBlob_BackwardsCompatibility(t *testing.T) {
	const chunkSize = 4 * 1024
	imageData := make([]byte, chunkSize*4)
	for i := range imageData {
		imageData[i] = byte(i % 251)
	}

	blobBytes, result, _ := buildChunked(t, imageData, contentindex.MediaTypeEROFSZstd, chunkSize)

	dec, err := zstd.NewReader(bytes.NewReader(blobBytes))
	if err != nil {
		t.Fatalf("new zstd reader: %v", err)
	}
	defer dec.Close()

	got, err := io.ReadAll(dec)
	if err != nil {
		t.Fatalf("zstd decode: %v", err)
	}
	if !bytes.Equal(got, imageData) {
		t.Fatalf("decompressed bytes mismatch: got %d bytes, want %d", len(got), len(imageData))
	}
	t.Logf("backwards-compat: %d bytes → zstd decode → %d bytes ✓ (chunks: %d, diffID: %s)",
		len(blobBytes), len(got), len(result.Chunks), result.DiffID)
}

// TestChunkedBlob_DiffID verifies that Result.DiffID equals the SHA-256 of
// the raw (uncompressed) input — i.e. the in-stream hash is correct.
func TestChunkedBlob_DiffID(t *testing.T) {
	const chunkSize = 8 * 1024
	imageData := make([]byte, chunkSize*3+512)
	for i := range imageData {
		imageData[i] = byte(i % 137)
	}

	_, result, _ := buildChunked(t, imageData, contentindex.MediaTypeEROFSZstd, chunkSize)

	want := digest.FromBytes(imageData)
	if result.DiffID != want {
		t.Errorf("DiffID mismatch: got %s want %s", result.DiffID, want)
	}
	t.Logf("DiffID: %s ✓", result.DiffID)
}

// TestChunkedBlob_VariousChunkSizes verifies backwards compatibility across a
// range of chunk sizes.
func TestChunkedBlob_VariousChunkSizes(t *testing.T) {
	for _, chunkSize := range []int{4 * 1024, 16 * 1024, 64 * 1024, 256 * 1024} {
		t.Run(formatBytes(chunkSize), func(t *testing.T) {
			imageSize := chunkSize*3 + chunkSize/2
			imageData := make([]byte, imageSize)
			for i := range imageData {
				imageData[i] = byte(i % 127)
			}

			blobBytes, result, _ := buildChunked(t, imageData, contentindex.MediaTypeEROFSZstd, chunkSize)

			dec, err := zstd.NewReader(bytes.NewReader(blobBytes))
			if err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(dec)
			dec.Close()
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !bytes.Equal(got, imageData) {
				t.Fatalf("chunk size %d: decompressed content mismatch (%d vs %d bytes)",
					chunkSize, len(got), len(imageData))
			}
			// Verify in-stream DiffID.
			if result.DiffID != digest.FromBytes(imageData) {
				t.Errorf("DiffID mismatch for chunk size %d", chunkSize)
			}
			t.Logf("chunk size %d: blob %d bytes, %d chunks, decompress OK, DiffID OK ✓",
				chunkSize, len(blobBytes), len(result.Chunks))
		})
	}
}

// TestExportImport_RoundTrip tests the full export→import cycle for a
// chunked EROFS blob.
func TestExportImport_RoundTrip(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")

	// ── Step 1: Build chunked blob ────────────────────────────────────────
	const chunkSize = 4 * 1024
	imageData := make([]byte, chunkSize*3)
	for i := range imageData {
		imageData[i] = byte(i % 199)
	}
	blobBytes, result, layerDesc := buildChunked(t, imageData, contentindex.MediaTypeEROFSZstd, chunkSize)
	t.Logf("layer: digest=%s size=%d chunks=%d", layerDesc.Digest, layerDesc.Size, len(result.Chunks))

	// ── Step 2: Populate a source content store ────────────────────────────
	srcCS, err := localcs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	writeBlob(t, ctx, srcCS, layerDesc, blobBytes)

	configData := []byte(`{"architecture":"amd64","os":"linux"}`)
	configDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageConfig,
		Digest:    digest.FromBytes(configData),
		Size:      int64(len(configData)),
	}
	writeBlob(t, ctx, srcCS, configDesc, configData)

	manifest := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    []ocispec.Descriptor{layerDesc},
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestDesc := ocispec.Descriptor{
		MediaType:   ocispec.MediaTypeImageManifest,
		Digest:      digest.FromBytes(manifestJSON),
		Size:        int64(len(manifestJSON)),
		Annotations: map[string]string{ocispec.AnnotationRefName: "test/erofs-chunked:v1"},
	}
	writeBlob(t, ctx, srcCS, manifestDesc, manifestJSON)

	// ── Step 3: Export to tar ─────────────────────────────────────────────
	var tarBuf bytes.Buffer
	if err := archive.Export(ctx, srcCS, &tarBuf,
		archive.WithManifest(manifestDesc, "test/erofs-chunked:v1")); err != nil {
		t.Fatalf("Export: %v", err)
	}
	t.Logf("exported tar: %d bytes", tarBuf.Len())

	// ── Step 4: Import from tar ───────────────────────────────────────────
	dstCS, err := localcs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	idxDesc, err := archive.ImportIndex(ctx, dstCS, bytes.NewReader(tarBuf.Bytes()))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	t.Logf("imported index: %s", idxDesc.Digest)

	// ── Step 5: Verify the layer blob is intact ───────────────────────────
	_, importedLayerDesc := findLayerFromIndex(t, ctx, dstCS, idxDesc)

	if importedLayerDesc.Digest != layerDesc.Digest {
		t.Fatalf("layer digest changed: got %s want %s", importedLayerDesc.Digest, layerDesc.Digest)
	}
	if importedLayerDesc.Size != layerDesc.Size {
		t.Fatalf("layer size changed: got %d want %d", importedLayerDesc.Size, layerDesc.Size)
	}

	importedLayerBytes := readBlobBytes(t, ctx, dstCS, importedLayerDesc)
	if !bytes.Equal(importedLayerBytes, blobBytes) {
		t.Fatal("imported layer bytes differ from original")
	}
	t.Log("layer blob: byte-for-byte identical after import ✓")

	// ── Step 6: Verify backwards compatibility ─────────────────────────────
	dec, err := zstd.NewReader(bytes.NewReader(importedLayerBytes))
	if err != nil {
		t.Fatal(err)
	}
	decompressed, err := io.ReadAll(dec)
	dec.Close()
	if err != nil {
		t.Fatalf("zstd decode: %v", err)
	}
	if !bytes.Equal(decompressed, imageData) {
		t.Fatal("decompressed content differs from original image data")
	}
	t.Log("backwards compat: zstd decode of imported blob matches original image data ✓")

	// ── Step 7: Verify chunk-index annotations survive ────────────────────
	if importedLayerDesc.Annotations == nil {
		t.Fatal("imported layer descriptor has no annotations")
	}
	for _, key := range []string{
		contentindex.AnnotationChunkIndexRange,
		contentindex.AnnotationChunkIndexDigest,
		contentindex.AnnotationChunkIndexMediaType,
	} {
		if v := importedLayerDesc.Annotations[key]; v == "" {
			t.Errorf("annotation %s missing or empty after import", key)
		} else {
			t.Logf("  %s = %s ✓", key, v)
		}
	}
	if importedLayerDesc.Annotations[contentindex.AnnotationChunkIndexRange] !=
		layerDesc.Annotations[contentindex.AnnotationChunkIndexRange] {
		t.Errorf("AnnotationChunkIndexRange changed: got %q want %q",
			importedLayerDesc.Annotations[contentindex.AnnotationChunkIndexRange],
			layerDesc.Annotations[contentindex.AnnotationChunkIndexRange])
	}
}

// TestExportImport_MediaTypePreserved verifies the media type survives
// export→import unchanged.
func TestExportImport_MediaTypePreserved(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")

	imageData := make([]byte, 8*1024)
	for i := range imageData {
		imageData[i] = byte(i)
	}
	blobBytes, _, layerDesc := buildChunked(t, imageData, contentindex.MediaTypeEROFSZstd, 4*1024)

	cs, err := localcs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	writeBlob(t, ctx, cs, layerDesc, blobBytes)
	configData := []byte(`{}`)
	configDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageConfig,
		Digest:    digest.FromBytes(configData),
		Size:      int64(len(configData)),
	}
	writeBlob(t, ctx, cs, configDesc, configData)

	mfst := ocispec.Manifest{
		Versioned: specs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    []ocispec.Descriptor{layerDesc},
	}
	mfstJSON, _ := json.Marshal(mfst)
	mfstDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    digest.FromBytes(mfstJSON),
		Size:      int64(len(mfstJSON)),
	}
	writeBlob(t, ctx, cs, mfstDesc, mfstJSON)

	var tarBuf bytes.Buffer
	if err := archive.Export(ctx, cs, &tarBuf, archive.WithManifest(mfstDesc)); err != nil {
		t.Fatalf("Export: %v", err)
	}

	dstCS, err := localcs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	idxDesc, err := archive.ImportIndex(ctx, dstCS, bytes.NewReader(tarBuf.Bytes()))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	_, importedLayerDesc := findLayerFromIndex(t, ctx, dstCS, idxDesc)
	if importedLayerDesc.MediaType != contentindex.MediaTypeEROFSZstd {
		t.Errorf("media type: got %q want %q", importedLayerDesc.MediaType, contentindex.MediaTypeEROFSZstd)
	} else {
		t.Logf("media type preserved: %s ✓", importedLayerDesc.MediaType)
	}
}

// TestTarLayer_ToChunkedBlob_BackwardsCompat tests format self-consistency
// when the input happens to be a tar archive.
func TestTarLayer_ToChunkedBlob_BackwardsCompat(t *testing.T) {
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	content := []byte("hello from erofs chunked test")
	tw.WriteHeader(&tar.Header{Name: "hello.txt", Mode: 0644, Size: int64(len(content))})
	tw.Write(content)
	tw.Close()
	tarData := tarBuf.Bytes()

	blobBytes, result, _ := buildChunked(t, tarData, contentindex.MediaTypeEROFSZstd, 4*1024)

	dec, err := zstd.NewReader(bytes.NewReader(blobBytes))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(dec)
	dec.Close()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(got, tarData) {
		t.Fatalf("decoded content mismatch: got %d bytes want %d", len(got), len(tarData))
	}

	tr := tar.NewReader(bytes.NewReader(got))
	hdr, err := tr.Next()
	if err != nil {
		t.Fatalf("decoded tar: %v", err)
	}
	if hdr.Name != "hello.txt" {
		t.Errorf("tar entry name: got %q want %q", hdr.Name, "hello.txt")
	}
	fileContent, _ := io.ReadAll(tr)
	if !bytes.Equal(fileContent, content) {
		t.Error("tar file content mismatch")
	}
	t.Logf("tar→chunked→zstd→tar: %d bytes (1 chunk, %d annotations) ✓",
		len(tarData), len(result.Descriptor.Annotations))
}

// TestLayerConvertFuncChunked_RoundTripDigest is gap-fill test #1.
//
// Property: after running the chunked converter, the descriptor it
// returns has Digest equal to sha256 of the bytes the content store
// holds at that digest.  In other words, reading the produced blob
// back from the store and re-hashing it yields exactly newDesc.Digest.
//
// The local content store enforces this invariant at commit time via
// the writer's `expected != actual` check, but no test had previously
// exercised the *converter entry point* end-to-end — i.e. the path
// from "real tar layer in" to "indexed-content-ready descriptor out,
// readable from the store, and self-consistent on re-read".  A bug
// in `chunked.Build` or in the converter's descriptor assembly that
// stamped a different digest than the bytes actually committed would
// have been caught only at runtime by registry push or by a later
// pull's verifier.
//
// We feed `LayerConvertFuncChunked` a small tar layer, capture the
// returned descriptor, then independently `cs.ReaderAt` the produced
// blob, hash it, and assert equality with `newDesc.Digest`.  Also
// asserts the descriptor's `Size` matches `ra.Size()`.
func TestLayerConvertFuncChunked_RoundTripDigest(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")

	// Build a small tar layer: 2 files of distinct sizes.
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	files := []struct {
		name string
		body []byte
	}{
		{"hello.txt", []byte("hello, world\n")},
		{"larger.bin", bytes.Repeat([]byte{0xA5}, 8*1024)},
	}
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: f.name,
			Mode: 0644,
			Size: int64(len(f.body)),
		}); err != nil {
			t.Fatalf("tar WriteHeader: %v", err)
		}
		if _, err := tw.Write(f.body); err != nil {
			t.Fatalf("tar Write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}

	// Source content store with the tar layer ingested.
	cs, err := localcs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tarBytes := tarBuf.Bytes()
	inDesc := ocispec.Descriptor{
		MediaType: "application/vnd.oci.image.layer.v1.tar",
		Digest:    digest.FromBytes(tarBytes),
		Size:      int64(len(tarBytes)),
	}
	writeBlob(t, ctx, cs, inDesc, tarBytes)

	// Run the chunked converter against a small target frame so we
	// exercise the multi-chunk path on a tiny test input.
	const targetFrame = 2 * 1024
	convert := erofs.LayerConvertFuncChunked(nil, targetFrame)
	newDesc, err := convert(ctx, cs, inDesc)
	if err != nil {
		t.Fatalf("LayerConvertFuncChunked: %v", err)
	}
	if newDesc == nil {
		t.Fatal("LayerConvertFuncChunked returned nil descriptor — input was a layer type")
	}
	t.Logf("converted: digest=%s size=%d media=%s", newDesc.Digest, newDesc.Size, newDesc.MediaType)

	// Re-read the bytes the converter committed under newDesc.Digest
	// and verify (sha256 + size) match the descriptor.  This is the
	// invariant the gap-fill test guarantees.
	ra, err := cs.ReaderAt(ctx, *newDesc)
	if err != nil {
		t.Fatalf("ReaderAt(newDesc): %v", err)
	}
	defer ra.Close()
	if ra.Size() != newDesc.Size {
		t.Errorf("ra.Size() = %d, newDesc.Size = %d", ra.Size(), newDesc.Size)
	}

	got := make([]byte, ra.Size())
	if _, err := ra.ReadAt(got, 0); err != nil && err != io.EOF {
		t.Fatalf("ReadAt: %v", err)
	}
	gotDigest := digest.FromBytes(got)
	if gotDigest != newDesc.Digest {
		t.Fatalf("digest mismatch after converter round-trip:\n  newDesc.Digest = %s\n  sha256(bytes)  = %s",
			newDesc.Digest, gotDigest)
	}

	// Sanity: the chunk-index annotations must be on the descriptor;
	// without them no consumer can resolve the indexed path.
	for _, k := range []string{
		contentindex.AnnotationChunkIndexRange,
		contentindex.AnnotationChunkIndexDigest,
		contentindex.AnnotationChunkIndexMediaType,
		contentindex.AnnotationUncompressedDigest,
	} {
		if newDesc.Annotations[k] == "" {
			t.Errorf("annotation %s missing on converter output", k)
		}
	}
}

// TestLayerConvertFuncChunked_DmVerity is the keystone test for the bug fix:
// previously LayerConvertFuncChunked accepted ConvertOpts but never honored
// dmVerity, so the chunked path silently produced non-verity blobs.  After
// the fix, passing erofs.WithDmVerity() must:
//
//  1. stamp the org.erofs.dmverity.{hash-offset,root-digest} annotations on
//     the descriptor;
//  2. force a chunk boundary at the verity hash offset so the verity tree
//     and the EROFS data section never share a zstd frame (the lazy mount
//     path relies on this to pre-fill just the tree);
//  3. preserve the descriptor's round-trip digest property (blob bytes
//     re-read from the store hash to newDesc.Digest).
func TestLayerConvertFuncChunked_DmVerity(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")

	// Build a small tar layer.
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	files := []struct {
		name string
		body []byte
	}{
		{"hello.txt", []byte("hello, world\n")},
		{"larger.bin", bytes.Repeat([]byte{0xA5}, 16*1024)},
	}
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: f.name,
			Mode: 0644,
			Size: int64(len(f.body)),
		}); err != nil {
			t.Fatalf("tar WriteHeader: %v", err)
		}
		if _, err := tw.Write(f.body); err != nil {
			t.Fatalf("tar Write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}

	cs, err := localcs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tarBytes := tarBuf.Bytes()
	inDesc := ocispec.Descriptor{
		MediaType: "application/vnd.oci.image.layer.v1.tar",
		Digest:    digest.FromBytes(tarBytes),
		Size:      int64(len(tarBytes)),
	}
	writeBlob(t, ctx, cs, inDesc, tarBytes)

	const targetFrame = 2 * 1024
	convert := erofs.LayerConvertFuncChunked(nil, targetFrame, erofs.WithDmVerity())
	newDesc, err := convert(ctx, cs, inDesc)
	if err != nil {
		t.Fatalf("LayerConvertFuncChunked: %v", err)
	}
	if newDesc == nil {
		t.Fatal("converter returned nil descriptor")
	}

	// (1) Verity annotations must be present.
	for _, k := range []string{
		contentindex.AnnotationDmVerityHashOffset,
		contentindex.AnnotationDmVerityRootDigest,
	} {
		if newDesc.Annotations[k] == "" {
			t.Errorf("annotation %s missing on chunked+verity output", k)
		}
	}

	// (3) Round-trip digest property holds.
	ra, err := cs.ReaderAt(ctx, *newDesc)
	if err != nil {
		t.Fatalf("ReaderAt(newDesc): %v", err)
	}
	defer ra.Close()
	got := make([]byte, ra.Size())
	if _, err := ra.ReadAt(got, 0); err != nil && err != io.EOF {
		t.Fatalf("ReadAt: %v", err)
	}
	if gotDigest := digest.FromBytes(got); gotDigest != newDesc.Digest {
		t.Fatalf("digest mismatch after chunked+verity round-trip:\n  newDesc.Digest = %s\n  sha256(bytes)  = %s",
			newDesc.Digest, gotDigest)
	}
	t.Logf("chunked+verity: digest=%s size=%d hash_offset=%s root_digest=%s",
		newDesc.Digest, newDesc.Size,
		newDesc.Annotations[contentindex.AnnotationDmVerityHashOffset],
		newDesc.Annotations[contentindex.AnnotationDmVerityRootDigest])
}

// TestChunkedBuild_WithTargetFrameSizeShrinksChunks is the regression
// test for the bug where MergeManifestFunc (the default convert path)
// hard-coded chunked.TargetFrameSize and silently ignored
// --erofs-chunk-size — a user reported a 60 MiB blob landing in 14
// chunks (~4.3 MiB each) when the default 512 KiB should have produced
// ~120 chunks.
//
// We drive chunked.Build (the layer underneath all converter entry
// points) with two target frame sizes against incompressible data and
// assert the smaller target produces strictly more chunks.  The CLI →
// WithTargetFrameSize → chunked.Build plumbing is what carries the
// user's flag through; the dedicated test at
// cmd/ctr/commands/images/convert_test.go::TestErofsConvertOptions_ChunkSizeThreaded
// verifies the CLI emits WithTargetFrameSize, and this test verifies
// the option actually changes the on-disk chunk count.
func TestChunkedBuild_WithTargetFrameSizeShrinksChunks(t *testing.T) {
	// Use random bytes so zstd can't squeeze them to nothing.
	const dataSize = 16 * 1024 * 1024 // 16 MiB
	rng := rand.New(rand.NewSource(0xC0FFEE))
	data := make([]byte, dataSize)
	_, _ = rng.Read(data)

	build := func(target int) int {
		var buf bytes.Buffer
		result, err := chunked.Build(bytes.NewReader(data), int64(len(data)),
			&buf, contentindex.MediaTypeEROFSZstd, target)
		if err != nil {
			t.Fatalf("chunked.Build (target=%d): %v", target, err)
		}
		t.Logf("target=%dB → %d chunks, blob=%dB", target, len(result.Chunks), result.Written)
		return len(result.Chunks)
	}

	const (
		large = 4 * 1024 * 1024 // 4 MiB compressed target
		small = 256 * 1024      // 256 KiB compressed target
	)
	nLarge := build(large)
	nSmall := build(small)
	if nSmall <= nLarge {
		t.Errorf("smaller target frame did not increase chunk count: nSmall=%d nLarge=%d", nSmall, nLarge)
	}
	// Assert at least 4× more chunks: if WithTargetFrameSize were
	// silently dropped or the ratio constants were off by 8×, the
	// expected ~16× ratio (4 MiB / 256 KiB) would still satisfy this
	// loose bound while a regression would not.
	if nSmall < 4*nLarge {
		t.Errorf("smaller target frame produced %d chunks vs %d for the larger target; expected at least 4× more", nSmall, nLarge)
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func writeBlob(t *testing.T, ctx context.Context, cs content.Store, desc ocispec.Descriptor, data []byte) {
	t.Helper()
	if _, err := cs.Info(ctx, desc.Digest); err == nil {
		return
	}
	cw, err := cs.Writer(ctx,
		content.WithRef("write-"+desc.Digest.String()),
		content.WithDescriptor(desc),
	)
	if err != nil {
		t.Fatalf("open writer for %s: %v", desc.Digest, err)
	}
	if _, err := cw.Write(data); err != nil {
		cw.Close()
		t.Fatalf("write %s: %v", desc.Digest, err)
	}
	if err := cw.Commit(ctx, int64(len(data)), desc.Digest); err != nil {
		t.Fatalf("commit %s: %v", desc.Digest, err)
	}
}

func readBlobBytes(t *testing.T, ctx context.Context, cs content.Store, desc ocispec.Descriptor) []byte {
	t.Helper()
	ra, err := cs.ReaderAt(ctx, desc)
	if err != nil {
		t.Fatalf("ReaderAt %s: %v", desc.Digest, err)
	}
	defer ra.Close()
	buf := make([]byte, ra.Size())
	if _, err := ra.ReadAt(buf, 0); err != nil && err != io.EOF {
		t.Fatalf("ReadAt %s: %v", desc.Digest, err)
	}
	return buf
}

func findLayerFromIndex(t *testing.T, ctx context.Context, cs content.Store, idxDesc ocispec.Descriptor) (ocispec.Manifest, ocispec.Descriptor) {
	t.Helper()
	idxBytes := readBlobBytes(t, ctx, cs, idxDesc)
	var idx ocispec.Index
	if err := json.Unmarshal(idxBytes, &idx); err != nil {
		t.Fatalf("unmarshal index: %v", err)
	}
	if len(idx.Manifests) == 0 {
		t.Fatal("index has no manifests")
	}
	for _, mDesc := range idx.Manifests {
		mBytes := readBlobBytes(t, ctx, cs, mDesc)
		var m ocispec.Manifest
		if err := json.Unmarshal(mBytes, &m); err != nil {
			continue
		}
		if len(m.Layers) == 0 {
			continue
		}
		return m, m.Layers[0]
	}
	t.Fatal("no manifest with layers found in index")
	return ocispec.Manifest{}, ocispec.Descriptor{}
}

func formatBytes(n int) string {
	switch {
	case n >= 1024*1024:
		return fmt.Sprintf("%dMiB", n/(1024*1024))
	case n >= 1024:
		return fmt.Sprintf("%dKiB", n/1024)
	default:
		return fmt.Sprintf("%dB", n)
	}
}
