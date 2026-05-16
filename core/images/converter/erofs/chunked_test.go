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
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/content/index/chunked"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/core/images/archive"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	localcs "github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/klauspost/compress/zstd"
	"github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// TestChunkedBlob_BackwardsCompatibility verifies the core property that makes
// the chunked format backwards-compatible with any standard zstd decoder:
//
// A chunked +zstd blob consists of:
//   [zstd frame 0 (chunk 0)] ... [zstd frame N-1 (chunk N-1)]
//   [zstd skippable frame (chunk index, not a data frame)]
//
// A standard zstd decoder decompresses all data frames in sequence and
// silently passes over the trailing skippable frame, producing the original
// uncompressed image data bytes.
//
// This means that an erofs differ applying a chunked layer via the standard
// zstd decompression path (treating the whole blob as a single zstd stream)
// produces the correct EROFS image — no chunk-index awareness is required.
func TestChunkedBlob_BackwardsCompatibility(t *testing.T) {
	// Use distinct byte patterns so we can verify correct ordering.
	const chunkSize = 4 * 1024
	imageData := make([]byte, chunkSize*4)
	for i := range imageData {
		imageData[i] = byte(i % 251) // non-trivial pattern
	}

	result, err := chunked.Build(
		bytes.NewReader(imageData),
		int64(len(imageData)),
		contentindex.MediaTypeEROFSLayerZstd,
		chunkSize,
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Decompress the full blob with a standard zstd decoder.  The decoder
	// must produce exactly the original image data.
	dec, err := zstd.NewReader(bytes.NewReader(result.Blob))
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
	t.Logf("backwards-compat: %d bytes → zstd decode → %d bytes ✓ (chunk count: %d)",
		len(result.Blob), len(got), len(result.Chunks))
}

// TestChunkedBlob_VariousChunkSizes verifies backwards compatibility across a
// range of chunk sizes (4 KiB to 256 KiB as used in tests, real layers use
// larger chunks).
func TestChunkedBlob_VariousChunkSizes(t *testing.T) {
	for _, chunkSize := range []int{4 * 1024, 16 * 1024, 64 * 1024, 256 * 1024} {
		t.Run(formatBytes(chunkSize), func(t *testing.T) {
			imageSize := chunkSize*3 + chunkSize/2 // non-multiple to test final partial chunk
			imageData := make([]byte, imageSize)
			for i := range imageData {
				imageData[i] = byte(i % 127)
			}

			result, err := chunked.Build(bytes.NewReader(imageData), int64(imageSize),
				contentindex.MediaTypeEROFSLayerZstd, chunkSize)
			if err != nil {
				t.Fatalf("Build(%d): %v", chunkSize, err)
			}

			dec, err := zstd.NewReader(bytes.NewReader(result.Blob))
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
			t.Logf("chunk size %d: blob %d bytes, %d chunks, decompress OK ✓",
				chunkSize, len(result.Blob), len(result.Chunks))
		})
	}
}

// TestExportImport_RoundTrip tests the full export→import cycle for a
// chunked EROFS blob:
//
//  1. Build a chunked +zstd blob (simulating the converter output).
//  2. Write a minimal OCI image manifest to an in-memory content store.
//  3. Export the image to an OCI tar archive.
//  4. Import from the tar archive to a second content store.
//  5. Verify the imported layer blob bytes are identical to the original.
//  6. Verify the imported blob decompresses correctly (backwards compat check).
//  7. Verify the org.erofs.index.* annotations survive the round-trip.
func TestExportImport_RoundTrip(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")

	// ── Step 1: Build chunked blob ────────────────────────────────────────
	const chunkSize = 4 * 1024
	imageData := make([]byte, chunkSize*3)
	for i := range imageData {
		imageData[i] = byte(i % 199)
	}
	result, err := chunked.Build(
		bytes.NewReader(imageData),
		int64(len(imageData)),
		contentindex.MediaTypeEROFSLayerZstd,
		chunkSize,
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	layerDesc := result.Descriptor
	t.Logf("layer: digest=%s size=%d chunks=%d", layerDesc.Digest, layerDesc.Size, len(result.Chunks))

	// ── Step 2: Populate a source content store ────────────────────────────
	srcCS, err := localcs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// Write the layer blob.
	writeBlob(t, ctx, srcCS, layerDesc, result.Blob)

	// Write a minimal config blob.
	configData := []byte(`{"architecture":"amd64","os":"linux"}`)
	configDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageConfig,
		Digest:    digest.FromBytes(configData),
		Size:      int64(len(configData)),
	}
	writeBlob(t, ctx, srcCS, configDesc, configData)

	// Build and write a manifest.
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
	// Find the manifest from the imported index.
	importedManifest, importedLayerDesc := findLayerFromIndex(t, ctx, dstCS, idxDesc)
	_ = importedManifest

	if importedLayerDesc.Digest != layerDesc.Digest {
		t.Fatalf("layer digest changed: got %s want %s", importedLayerDesc.Digest, layerDesc.Digest)
	}
	if importedLayerDesc.Size != layerDesc.Size {
		t.Fatalf("layer size changed: got %d want %d", importedLayerDesc.Size, layerDesc.Size)
	}

	// Read back the layer blob bytes.
	importedLayerBytes := readBlobBytes(t, ctx, dstCS, importedLayerDesc)
	if !bytes.Equal(importedLayerBytes, result.Blob) {
		t.Fatal("imported layer bytes differ from original")
	}
	t.Log("layer blob: byte-for-byte identical after import ✓")

	// ── Step 6: Verify backwards compatibility (zstd decompression) ───────
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

	// ── Step 7: Verify org.erofs.index.* annotations survive ─────────────
	// The import reads the manifest JSON, which carries the descriptor with
	// its Annotations field.  The imported descriptor's annotations must
	// include the chunk-index metadata so that downstream consumers (the
	// indexed content store, the erofs differ) can locate the chunk index.
	if importedLayerDesc.Annotations == nil {
		t.Fatal("imported layer descriptor has no annotations")
	}
	for _, key := range []string{
		contentindex.AnnotationIndexRange,
		contentindex.AnnotationIndexDigest,
		contentindex.AnnotationIndexMediaType,
	} {
		if v := importedLayerDesc.Annotations[key]; v == "" {
			t.Errorf("annotation %s missing or empty after import", key)
		} else {
			t.Logf("  %s = %s ✓", key, v)
		}
	}
	// Verify the chunk-index range annotation survives unchanged.
	if importedLayerDesc.Annotations[contentindex.AnnotationIndexRange] !=
		layerDesc.Annotations[contentindex.AnnotationIndexRange] {
		t.Errorf("AnnotationIndexRange changed: got %q want %q",
			importedLayerDesc.Annotations[contentindex.AnnotationIndexRange],
			layerDesc.Annotations[contentindex.AnnotationIndexRange])
	}
}

// TestExportImport_OrchestratedManifest tests that a chunked EROFS layer
// exported in an OCI tar is importable and that the layer's media type is
// preserved as application/vnd.erofs.layer.v1+zstd.
func TestExportImport_MediaTypePreserved(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")

	imageData := make([]byte, 8*1024)
	for i := range imageData {
		imageData[i] = byte(i)
	}
	result, err := chunked.Build(
		bytes.NewReader(imageData), int64(len(imageData)),
		contentindex.MediaTypeEROFSLayerZstd, 4*1024,
	)
	if err != nil {
		t.Fatal(err)
	}

	cs, err := localcs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	writeBlob(t, ctx, cs, result.Descriptor, result.Blob)
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
		Layers:    []ocispec.Descriptor{result.Descriptor},
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

	_, layerDesc := findLayerFromIndex(t, ctx, dstCS, idxDesc)
	if layerDesc.MediaType != contentindex.MediaTypeEROFSLayerZstd {
		t.Errorf("media type: got %q want %q", layerDesc.MediaType, contentindex.MediaTypeEROFSLayerZstd)
	} else {
		t.Logf("media type preserved: %s ✓", layerDesc.MediaType)
	}
}

// TestTarLayer_ToChunkedBlob_BackwardsCompat tests that the chunk builder
// produces the right format when given a tar archive as image data (simulating
// what mkfs.erofs would produce on a real tar layer conversion).
//
// We build a synthetic tar archive → treat it as "EROFS image data" → chunk
// it → verify standard zstd decode reproduces the tar → proves the format
// is self-consistent and backwards-compatible.
func TestTarLayer_ToChunkedBlob_BackwardsCompat(t *testing.T) {
	// Build a minimal tar archive to serve as "image data".
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	content := []byte("hello from erofs chunked test")
	tw.WriteHeader(&tar.Header{Name: "hello.txt", Mode: 0644, Size: int64(len(content))})
	tw.Write(content)
	tw.Close()
	tarData := tarBuf.Bytes()

	result, err := chunked.Build(
		bytes.NewReader(tarData),
		int64(len(tarData)),
		contentindex.MediaTypeEROFSLayerZstd,
		4*1024, // chunk size larger than the tar; produces a single chunk
	)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Decompress with standard zstd.
	dec, err := zstd.NewReader(bytes.NewReader(result.Blob))
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

	// Verify the decoded content is a valid tar archive.
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

// ── Helpers ───────────────────────────────────────────────────────────────────

func writeBlob(t *testing.T, ctx context.Context, cs content.Store, desc ocispec.Descriptor, data []byte) {
	t.Helper()
	if _, err := cs.Info(ctx, desc.Digest); err == nil {
		return // already exists
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

// findLayerFromIndex reads the OCI index, finds the first manifest, and
// returns the manifest and its first layer descriptor.
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

	// Find the first manifest (skip any index-of-indexes).
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


