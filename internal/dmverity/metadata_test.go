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

// Cross-platform tests for the metadata-sidecar helpers.  These cover
// JSON round-trip, the BlockSize default, and the backward-compatible
// load of older sidecars that omit the blocksize field.  No kernel
// dependency: tests run on every supported GOOS.
package dmverity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteMetadata_roundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "layer.dmverity")
	want := &DmverityMetadata{
		RootHash:   "sha256:abcdef0123456789",
		HashOffset: 8388608, // 8 MiB
		BlockSize:  4096,
	}
	if err := WriteMetadata(path, want); err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}
	got, err := ReadMetadataFromPath(path)
	if err != nil {
		t.Fatalf("ReadMetadataFromPath: %v", err)
	}
	if got.RootHash != want.RootHash {
		t.Errorf("RootHash = %q, want %q", got.RootHash, want.RootHash)
	}
	if got.HashOffset != want.HashOffset {
		t.Errorf("HashOffset = %d, want %d", got.HashOffset, want.HashOffset)
	}
	if got.BlockSize != want.BlockSize {
		t.Errorf("BlockSize = %d, want %d", got.BlockSize, want.BlockSize)
	}
}

func TestWriteMetadata_backwardCompatNoBlockSize(t *testing.T) {
	// An older sidecar (pre-blocksize patch) omits the blocksize
	// field.  Loaders must accept it and report the default via
	// EffectiveBlockSize() so downstream code never sees zero.
	dir := t.TempDir()
	path := filepath.Join(dir, "old.dmverity")
	const oldJSON = `{"roothash":"sha256:deadbeef","hashoffset":1024}`
	if err := os.WriteFile(path, []byte(oldJSON), 0644); err != nil {
		t.Fatalf("write old sidecar: %v", err)
	}
	got, err := ReadMetadataFromPath(path)
	if err != nil {
		t.Fatalf("ReadMetadataFromPath: %v", err)
	}
	if got.BlockSize != 0 {
		t.Errorf("BlockSize (raw) = %d, want 0 (absent in JSON)", got.BlockSize)
	}
	if eb := got.EffectiveBlockSize(); eb != DefaultBlockSize {
		t.Errorf("EffectiveBlockSize() = %d, want DefaultBlockSize = %d", eb, DefaultBlockSize)
	}
}

func TestEffectiveBlockSize(t *testing.T) {
	cases := []struct {
		name string
		in   uint32
		want uint32
	}{
		{"zero → default", 0, DefaultBlockSize},
		{"non-zero passes through", 8192, 8192},
		{"non-power-of-two passes through", 7000, 7000}, // honor caller verbatim
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &DmverityMetadata{BlockSize: tc.in}
			if got := m.EffectiveBlockSize(); got != tc.want {
				t.Errorf("EffectiveBlockSize() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestEffectiveBlockSize_nilReceiver(t *testing.T) {
	// A nil *DmverityMetadata must report DefaultBlockSize rather
	// than panicking.  Callers that thread "no verity here" via
	// `nil` would otherwise have to special-case before consulting
	// the block size.
	var m *DmverityMetadata
	if got := m.EffectiveBlockSize(); got != DefaultBlockSize {
		t.Errorf("(nil).EffectiveBlockSize() = %d, want %d", got, DefaultBlockSize)
	}
}

func TestWriteMetadata_rejectsEmptyRootHash(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.dmverity")
	err := WriteMetadata(path, &DmverityMetadata{HashOffset: 1024})
	if err == nil {
		t.Fatal("WriteMetadata accepted empty RootHash; want error")
	}
}

func TestWriteMetadata_rejectsNil(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.dmverity")
	if err := WriteMetadata(path, nil); err == nil {
		t.Fatal("WriteMetadata accepted nil metadata; want error")
	}
}

func TestWriteMetadata_atomicRename(t *testing.T) {
	// Two concurrent writers to the same path must end up with
	// EXACTLY ONE valid sidecar (last-write-wins by rename); the
	// readers must never observe a half-written file.  We can't
	// easily fake a torn-write at filesystem level in a portable
	// unit test, but we can prove the rename happens by verifying
	// no `.tmp` file is left behind after a successful write.
	dir := t.TempDir()
	path := filepath.Join(dir, "x.dmverity")
	if err := WriteMetadata(path, &DmverityMetadata{
		RootHash:   "sha256:1",
		HashOffset: 4096,
	}); err != nil {
		t.Fatalf("WriteMetadata: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp leak: stat err = %v (want NotExist)", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("sidecar missing after WriteMetadata: %v", err)
	}
}

func TestReadMetadataFromPath_missing(t *testing.T) {
	_, err := ReadMetadataFromPath(filepath.Join(t.TempDir(), "absent"))
	if err == nil {
		t.Fatal("ReadMetadataFromPath of missing file returned nil; want error")
	}
}
