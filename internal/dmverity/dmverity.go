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

// Package dmverity provides functions for working with dm-verity for integrity verification
// using the veritysetup-go library
package dmverity

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type DmverityOptions struct {
	// Salt for hashing, represented as a hex string
	Salt string
	// Hash algorithm to use (default: sha256)
	HashAlgorithm string
	// Size of data blocks in bytes (default: 4096)
	DataBlockSize uint32
	// Size of hash blocks in bytes (default: 4096)
	HashBlockSize uint32
	// Number of data blocks
	DataBlocks uint64
	// Offset of hash area in bytes
	HashOffset uint64
	// Hash type (default: 1)
	HashType uint32
	// NoSuperblock disables superblock usage (matches library's NoSuperblock field)
	NoSuperblock bool
	// UUID for device to use
	UUID string
}

func DefaultDmverityOptions() *DmverityOptions {
	return &DmverityOptions{
		Salt:          "0000000000000000000000000000000000000000000000000000000000000000",
		HashAlgorithm: "sha256",
		DataBlockSize: 4096,
		HashBlockSize: 4096,
		HashType:      1,
		NoSuperblock:  false, // By default, use superblock
	}
}

func MetadataPath(layerBlobPath string) string {
	if strings.HasSuffix(layerBlobPath, ".dmverity") {
		return layerBlobPath
	}
	return layerBlobPath + ".dmverity"
}

// normalizeRootHash strips the optional digest algorithm prefix from a
// dm-verity root hash so it can be hex-decoded.
//
// The root hash flows through this package in two equivalent forms:
//
//   - Bare hex: produced by Format() (returns fmt.Sprintf("%x", ...)) and
//     used in older eager-format sidecars.
//   - "sha256:<hex>": carried by the org.erofs.dmverity.root_digest
//     annotation produced by the converter, persisted verbatim into
//     convert-time sidecars, and threaded through block-mount options
//     as documented on plugins/mount/block.OptDmVerityRootHash.
//
// Both forms must be accepted at the Open / VerifyDevice boundary
// because both are produced and stored elsewhere in the codebase.
// Any other algorithm prefix (sha512:, sha1:, ...) is rejected — the
// dm-verity setup in this codebase is SHA-256 only.
func normalizeRootHash(rootHash string) (string, error) {
	r := strings.TrimSpace(rootHash)
	if r == "" {
		return "", nil
	}
	if idx := strings.IndexByte(r, ':'); idx >= 0 {
		algo := r[:idx]
		if algo != "sha256" {
			return "", fmt.Errorf("dm-verity root hash uses unsupported algorithm %q (only sha256 is supported)", algo)
		}
		r = r[idx+1:]
	}
	return r, nil
}

func DevicePath(name string) string {
	return fmt.Sprintf("/dev/mapper/%s", name)
}

type DmverityMetadata struct {
	RootHash   string `json:"roothash"`
	HashOffset uint64 `json:"hashoffset"`
	// BlockSize is the dm-verity data block size in bytes.  Optional —
	// zero means "use the default 4096".  Older writers (pre-blocksize
	// patch) did not emit this field, so loaders treat 0 identically
	// to the default for backward compatibility.
	BlockSize uint32 `json:"blocksize,omitempty"`
}

// DefaultBlockSize is the dm-verity data block size used when the
// metadata record does not specify one.  Mirrors
// contentindex.DefaultDmVerityBlockSize so the lazy path and the
// existing eager path agree on the default without an import cycle.
const DefaultBlockSize uint32 = 4096

// EffectiveBlockSize returns m.BlockSize when non-zero, otherwise
// DefaultBlockSize.  Use this whenever you need a concrete block size
// to hand to the verity device setup.
func (m *DmverityMetadata) EffectiveBlockSize() uint32 {
	if m == nil || m.BlockSize == 0 {
		return DefaultBlockSize
	}
	return m.BlockSize
}

func ReadMetadata(layerBlobPath string) (*DmverityMetadata, error) {
	metadataPath := MetadataPath(layerBlobPath)
	return ReadMetadataFromPath(metadataPath)
}

// ReadMetadataFromPath reads a DmverityMetadata JSON document from
// the exact path provided (no `.dmverity` suffix transformation).
// Callers that store the sidecar at an unusual filename — e.g. the
// lazy snapshotter, which uses `<snapshot_dir>/layer.dmverity` to
// parallel `layer.indexed` — use this entry point.
func ReadMetadataFromPath(path string) (*DmverityMetadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata file %q: %w", path, err)
	}

	var metadata DmverityMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, fmt.Errorf("failed to parse metadata file %q: %w", path, err)
	}

	if metadata.RootHash == "" {
		return nil, fmt.Errorf("missing root hash in metadata file %q", path)
	}

	return &metadata, nil
}

// WriteMetadata serialises m to path with mode 0644 atomically (write
// to a sibling temp file, then rename).  The atomic write keeps a
// concurrent reader from observing a half-written sidecar in the
// vanishingly rare case where two callers race to materialise the
// same snapshot.
func WriteMetadata(path string, m *DmverityMetadata) error {
	if m == nil {
		return fmt.Errorf("dmverity: WriteMetadata: nil metadata")
	}
	if m.RootHash == "" {
		return fmt.Errorf("dmverity: WriteMetadata: empty root hash")
	}
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("dmverity: marshal metadata: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("dmverity: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("dmverity: rename %s → %s: %w", tmp, path, err)
	}
	return nil
}
