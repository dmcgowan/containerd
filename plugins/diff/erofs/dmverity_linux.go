//go:build linux

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
	"encoding/json"
	"fmt"
	"os"

	"github.com/containerd/go-dmverity/pkg/verity"
	"github.com/containerd/log"

	"github.com/containerd/containerd/v2/internal/dmverity"
)

// getDmverityOptions returns dm-verity options configured for this differ instance.
// The block size is determined by the differ's mode:
// - Tar index mode requires 512-byte blocks to match EROFS and dm-verity constraints
// - Regular mode uses 4096-byte blocks (standard page size)
func (s *erofsDiff) getDmverityOptions() *dmverity.DmverityOptions {
	opts := dmverity.DefaultDmverityOptions()

	// Tar index mode requires 512-byte blocks because:
	// 1. EROFS tar index mode uses 512-byte metadata blocks (mkfs.erofs --tar=i)
	// 2. dm-verity sets the virtual block device logical_block_size to match the data block size
	// 3. EROFS requires its block size (512) to be >= the underlying block device's logical_block_size
	// Using 4096-byte dm-verity blocks would set logical_block_size=4096, causing EROFS sb_set_blocksize(512) to fail
	if s.enableTarIndex {
		opts.DataBlockSize = 512
		opts.HashBlockSize = 512
	}
	// Regular mode uses the default 4096-byte blocks (standard page size)

	// Format without an on-disk superblock, matching the rest of the
	// stack (see internal/erofsutils.VerityWriter and internal/dmverity.Open):
	// every parameter is carried in the .dmverity sidecar, so the superblock
	// is redundant and is not emitted.
	opts.NoSuperblock = true

	return opts
}

// formatDmverityLayer formats an EROFS layer with dm-verity hash tree
func (s *erofsDiff) formatDmverityLayer(ctx context.Context, layerBlobPath string) error {
	metadataPath := dmverity.MetadataPath(layerBlobPath)
	if _, err := os.Stat(metadataPath); err == nil {
		log.G(ctx).WithField("path", layerBlobPath).Debug("Layer already formatted with dm-verity, skipping")
		return nil
	}

	fileInfo, err := os.Stat(layerBlobPath)
	if err != nil {
		return fmt.Errorf("failed to stat layer blob: %w", err)
	}

	opts := s.getDmverityOptions()
	blockSize := int64(opts.DataBlockSize)
	fileSize := fileInfo.Size()

	// dm-verity requires the hash area to start at a block-aligned offset
	dataBlocks := (fileSize + blockSize - 1) / blockSize
	hashOffset := uint64(dataBlocks * blockSize)

	opts.HashOffset = hashOffset
	opts.DataBlocks = uint64(dataBlocks)

	hashTreeSize, err := verity.GetHashTreeSize(&verity.Params{
		HashName:      opts.HashAlgorithm,
		DataBlockSize: opts.DataBlockSize,
		HashBlockSize: opts.HashBlockSize,
		DataBlocks:    opts.DataBlocks,
		HashType:      opts.HashType,
	})
	if err != nil {
		return fmt.Errorf("failed to calculate hash tree size: %w", err)
	}

	// No-superblock mode: the hash tree begins directly at hashOffset
	// (no superblock block precedes it).
	requiredSize := hashOffset + hashTreeSize
	if err := os.Truncate(layerBlobPath, int64(requiredSize)); err != nil {
		return fmt.Errorf("failed to pre-allocate space for hash tree: %w", err)
	}

	rootHash, err := dmverity.Format(layerBlobPath, layerBlobPath, opts)
	if err != nil {
		return fmt.Errorf("failed to format dm-verity: %w", err)
	}

	// Persist hashOffset (= where the hash tree starts) and the block size
	// so the no-superblock opener can reconstruct the format parameters.
	metadata := dmverity.DmverityMetadata{
		RootHash:   rootHash,
		HashOffset: hashOffset,
		BlockSize:  opts.DataBlockSize,
	}
	metadataBytes, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal dm-verity metadata: %w", err)
	}
	if err := os.WriteFile(metadataPath, metadataBytes, 0644); err != nil {
		return fmt.Errorf("failed to write dm-verity metadata: %w", err)
	}

	log.G(ctx).WithFields(log.Fields{
		"path":       layerBlobPath,
		"size":       fileSize,
		"blockSize":  opts.DataBlockSize,
		"hashOffset": hashOffset,
		"rootHash":   rootHash,
	}).Info("Successfully formatted dm-verity layer")

	return nil
}
