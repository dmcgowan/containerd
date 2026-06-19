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

package erofs

import (
	"context"
	"os"
	"path"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/internal/dmverity"
)

func TestWriteLazyDmverityMarker_absentAnnotationsNoOp(t *testing.T) {
	dir := t.TempDir()
	// A descriptor with NO dmverity annotations must not produce a
	// sidecar.  This is the "verity disabled at convert time"
	// pathway; lazy ingest must still succeed.
	desc := ocispec.Descriptor{
		Digest:      "sha256:abc",
		Annotations: map[string]string{},
	}
	if err := writeLazyDmverityMarker(context.Background(), dir, desc); err != nil {
		t.Fatalf("writeLazyDmverityMarker: %v", err)
	}
	if _, err := os.Stat(path.Join(dir, "layer.dmverity")); !os.IsNotExist(err) {
		t.Errorf("sidecar created for non-verity descriptor; stat err = %v", err)
	}
}

func TestWriteLazyDmverityMarker_partialAnnotationsNoOp(t *testing.T) {
	// Either annotation missing → no sidecar.  Hard-fail at mount
	// time is the policy, but at convert/differ time we simply
	// don't claim verity.  Avoids false-positive verity at mount.
	for _, tc := range []struct {
		name        string
		annotations map[string]string
	}{
		{"only roothash", map[string]string{index.AnnotationDmVerityRootDigest: "sha256:abc"}},
		{"only hashoffset", map[string]string{index.AnnotationDmVerityHashOffset: "4096"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			desc := ocispec.Descriptor{Digest: "sha256:abc", Annotations: tc.annotations}
			if err := writeLazyDmverityMarker(context.Background(), dir, desc); err != nil {
				t.Fatalf("writeLazyDmverityMarker: %v", err)
			}
			if _, err := os.Stat(path.Join(dir, "layer.dmverity")); !os.IsNotExist(err) {
				t.Errorf("sidecar created for partial annotations; stat err = %v", err)
			}
		})
	}
}

func TestWriteLazyDmverityMarker_fullRoundTrip(t *testing.T) {
	dir := t.TempDir()
	desc := ocispec.Descriptor{
		Digest: "sha256:abc",
		Annotations: map[string]string{
			index.AnnotationDmVerityRootDigest: "sha256:deadbeef0123",
			index.AnnotationDmVerityHashOffset: "8388608",
			index.AnnotationDmVerityBlockSize:  "4096",
		},
	}
	if err := writeLazyDmverityMarker(context.Background(), dir, desc); err != nil {
		t.Fatalf("writeLazyDmverityMarker: %v", err)
	}
	got, err := dmverity.ReadMetadataFromPath(path.Join(dir, "layer.dmverity"))
	if err != nil {
		t.Fatalf("ReadMetadataFromPath: %v", err)
	}
	if got.RootHash != "sha256:deadbeef0123" {
		t.Errorf("RootHash = %q", got.RootHash)
	}
	if got.HashOffset != 8388608 {
		t.Errorf("HashOffset = %d", got.HashOffset)
	}
	if got.BlockSize != 4096 {
		t.Errorf("BlockSize = %d", got.BlockSize)
	}
}

func TestWriteLazyDmverityMarker_blockSizeOmittedDefaults(t *testing.T) {
	// The block-size annotation is omitted when the source descriptor
	// used the default 4096 (see annotations.go DefaultDmVerityBlockSize).
	// The marker must still be written; downstream code applies the
	// default via EffectiveBlockSize.
	dir := t.TempDir()
	desc := ocispec.Descriptor{
		Digest: "sha256:abc",
		Annotations: map[string]string{
			index.AnnotationDmVerityRootDigest: "sha256:dead",
			index.AnnotationDmVerityHashOffset: "1048576",
			// no AnnotationDmVerityBlockSize
		},
	}
	if err := writeLazyDmverityMarker(context.Background(), dir, desc); err != nil {
		t.Fatalf("writeLazyDmverityMarker: %v", err)
	}
	got, err := dmverity.ReadMetadataFromPath(path.Join(dir, "layer.dmverity"))
	if err != nil {
		t.Fatalf("ReadMetadataFromPath: %v", err)
	}
	if got.BlockSize != 0 {
		t.Errorf("BlockSize (raw) = %d, want 0 (annotation absent)", got.BlockSize)
	}
	if eb := got.EffectiveBlockSize(); eb != dmverity.DefaultBlockSize {
		t.Errorf("EffectiveBlockSize() = %d, want %d", eb, dmverity.DefaultBlockSize)
	}
}

func TestWriteLazyDmverityMarker_malformedHashOffsetIgnored(t *testing.T) {
	// Garbage hash_offset → log warn, no sidecar, no error.  Lazy
	// ingest must continue; mount-time will simply lack verity.
	dir := t.TempDir()
	desc := ocispec.Descriptor{
		Digest: "sha256:abc",
		Annotations: map[string]string{
			index.AnnotationDmVerityRootDigest: "sha256:dead",
			index.AnnotationDmVerityHashOffset: "not-a-number",
		},
	}
	if err := writeLazyDmverityMarker(context.Background(), dir, desc); err != nil {
		t.Fatalf("writeLazyDmverityMarker should not fail on bad offset: %v", err)
	}
	if _, err := os.Stat(path.Join(dir, "layer.dmverity")); !os.IsNotExist(err) {
		t.Errorf("sidecar created from malformed offset; stat err = %v", err)
	}
}

// Eager-path counterparts.  These exercise the convert-annotations →
// `<layerBlob>.dmverity` sidecar plumbing that lets the existing
// EROFS mount plugin activate dm-verity on raw EROFS layers (no
// chunk index, no lazy ingest) produced with convert's verity-on
// defaults.

func TestWriteEagerDmverityMarker_absentAnnotationsNoOp(t *testing.T) {
	dir := t.TempDir()
	layerBlob := path.Join(dir, "layer.erofs")
	desc := ocispec.Descriptor{
		Digest:      "sha256:abc",
		Annotations: map[string]string{},
	}
	if err := writeEagerDmverityMarker(context.Background(), layerBlob, desc); err != nil {
		t.Fatalf("writeEagerDmverityMarker: %v", err)
	}
	if _, err := os.Stat(dmverity.MetadataPath(layerBlob)); !os.IsNotExist(err) {
		t.Errorf("sidecar created for non-verity descriptor; stat err = %v", err)
	}
}

func TestWriteEagerDmverityMarker_fullRoundTrip(t *testing.T) {
	dir := t.TempDir()
	layerBlob := path.Join(dir, "layer.erofs")
	desc := ocispec.Descriptor{
		Digest: "sha256:abc",
		Annotations: map[string]string{
			index.AnnotationDmVerityRootDigest: "sha256:f00d",
			index.AnnotationDmVerityHashOffset: "16777216",
			index.AnnotationDmVerityBlockSize:  "4096",
		},
	}
	if err := writeEagerDmverityMarker(context.Background(), layerBlob, desc); err != nil {
		t.Fatalf("writeEagerDmverityMarker: %v", err)
	}
	// Sidecar must live at the path the EROFS mount plugin reads
	// via dmverity.MetadataPath(layerBlob).
	sidecar := dmverity.MetadataPath(layerBlob)
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("eager sidecar missing: %v", err)
	}
	got, err := dmverity.ReadMetadataFromPath(sidecar)
	if err != nil {
		t.Fatalf("ReadMetadataFromPath: %v", err)
	}
	if got.RootHash != "sha256:f00d" {
		t.Errorf("RootHash = %q", got.RootHash)
	}
	if got.HashOffset != 16777216 {
		t.Errorf("HashOffset = %d", got.HashOffset)
	}
}

func TestWriteEagerDmverityMarker_malformedHashOffsetIgnored(t *testing.T) {
	dir := t.TempDir()
	layerBlob := path.Join(dir, "layer.erofs")
	desc := ocispec.Descriptor{
		Digest: "sha256:abc",
		Annotations: map[string]string{
			index.AnnotationDmVerityRootDigest: "sha256:f00d",
			index.AnnotationDmVerityHashOffset: "garbage",
		},
	}
	if err := writeEagerDmverityMarker(context.Background(), layerBlob, desc); err != nil {
		t.Fatalf("writeEagerDmverityMarker should not fail on bad offset: %v", err)
	}
	if _, err := os.Stat(dmverity.MetadataPath(layerBlob)); !os.IsNotExist(err) {
		t.Errorf("sidecar created from malformed offset; stat err = %v", err)
	}
}
