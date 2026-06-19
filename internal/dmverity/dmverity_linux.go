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

package dmverity

import (
	"fmt"
	"os"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/go-dmverity/pkg/utils"
	"github.com/containerd/go-dmverity/pkg/verity"
)

func IsSupported() (bool, error) {
	if _, err := os.Stat("/sys/module/dm_verity"); err != nil {
		if os.IsNotExist(err) {
			return false, fmt.Errorf("dm_verity module not loaded or built-in")
		}
		return false, fmt.Errorf("failed to check /sys/module/dm_verity: %w", err)
	}

	return true, nil
}

func convertToVerityParams(opts *DmverityOptions) (verity.Params, error) {
	params := verity.DefaultParams()

	if opts != nil {
		if opts.HashAlgorithm != "" {
			params.HashName = opts.HashAlgorithm
		}
		if opts.DataBlockSize > 0 {
			params.DataBlockSize = opts.DataBlockSize
		}
		if opts.HashBlockSize > 0 {
			params.HashBlockSize = opts.HashBlockSize
		}
		if opts.DataBlocks > 0 {
			params.DataBlocks = opts.DataBlocks
		}
		if opts.HashOffset > 0 {
			params.HashAreaOffset = opts.HashOffset
		}
		if opts.HashType > 0 {
			params.HashType = opts.HashType
		}

		if opts.Salt != "" {
			salt, saltSize, err := utils.ApplySalt(opts.Salt, 256)
			if err != nil {
				return params, fmt.Errorf("invalid salt: %w", err)
			}
			params.Salt = salt
			params.SaltSize = saltSize
		}

		if opts.UUID != "" {
			uuidBytes, err := utils.ApplyUUID(opts.UUID, false, opts.NoSuperblock, nil)
			if err != nil {
				return params, fmt.Errorf("invalid UUID: %w", err)
			}
			params.UUID = uuidBytes
		}

		params.NoSuperblock = opts.NoSuperblock
	}

	return params, nil
}

// Format creates a dm-verity hash for a data device and returns the root hash.
// If hashDevice is the same as dataDevice, the hash will be stored on the same device.
func Format(dataDevice, hashDevice string, opts *DmverityOptions) (string, error) {
	if opts == nil {
		opts = DefaultDmverityOptions()
	}

	params, err := convertToVerityParams(opts)
	if err != nil {
		return "", fmt.Errorf("failed to convert options: %w", err)
	}

	if params.DataBlocks == 0 {
		size, err := utils.GetBlockOrFileSize(dataDevice)
		if err != nil {
			return "", fmt.Errorf("failed to get device size: %w", err)
		}
		params.DataBlocks = uint64(size / int64(params.DataBlockSize))
	}

	// IMPORTANT: This may modify params.HashAreaOffset when using superblock mode
	rootDigest, err := verity.Create(&params, dataDevice, hashDevice)
	if err != nil {
		return "", fmt.Errorf("failed to format dm-verity device: %w", err)
	}

	return fmt.Sprintf("%x", rootDigest), nil
}

// noSuperblockParams builds a complete verity.Params for opening a
// no-superblock dm-verity device whose merkle tree begins at hashOffset.
//
// Per veritysetup(8): with --no-superblock the opener must supply the same
// parameters used at format time.  Our converter always formats with
// sha256 / hash-type 1 / no salt / equal data & hash block sizes, so those
// are fixed here.  DataBlocks is derived from hashOffset (= the data extent)
// divided by the block size.  hashOffset must be a positive multiple of the
// block size.
func noSuperblockParams(hashOffset uint64, blockSize uint32) (verity.Params, error) {
	if blockSize == 0 {
		blockSize = 4096
	}
	if !verity.IsBlockSizeValid(blockSize) {
		return verity.Params{}, fmt.Errorf("dmverity: invalid block size %d", blockSize)
	}
	if hashOffset == 0 {
		return verity.Params{}, fmt.Errorf("dmverity: hashOffset required for no-superblock open")
	}
	if hashOffset%uint64(blockSize) != 0 {
		return verity.Params{}, fmt.Errorf("dmverity: hashOffset %d not a multiple of block size %d", hashOffset, blockSize)
	}
	return verity.Params{
		HashName:       "sha256",
		HashType:       1,
		DataBlockSize:  blockSize,
		HashBlockSize:  blockSize,
		DataBlocks:     hashOffset / uint64(blockSize),
		HashAreaOffset: hashOffset,
		NoSuperblock:   true,
	}, nil
}

// Open creates a read-only device-mapper target for transparent integrity
// verification.  It always operates in no-superblock mode: dm-verity images
// produced by this project carry no on-disk superblock (see
// internal/erofsutils.VerityWriter), so every parameter is supplied
// out-of-band here.  The merkle tree begins directly at hashOffset (= the
// EROFS data size); the block size defaults to 4096 and may be overridden
// via opts.  DataBlocks is derived as hashOffset/blockSize.
//
// opts may be nil (all defaults).  Any opts.NoSuperblock value is ignored —
// no-superblock is unconditional.
func Open(dataDevice string, name string, hashDevice string, rootHash string, hashOffset uint64, opts *DmverityOptions) (string, error) {
	if rootHash == "" {
		return "", fmt.Errorf("rootHash cannot be empty")
	}

	hexHash, err := normalizeRootHash(rootHash)
	if err != nil {
		return "", fmt.Errorf("invalid root hash: %w", err)
	}
	rootDigest, err := utils.ParseRootHash(hexHash)
	if err != nil {
		return "", fmt.Errorf("invalid root hash: %w", err)
	}

	blockSize := uint32(4096)
	if opts != nil && opts.DataBlockSize > 0 {
		blockSize = opts.DataBlockSize
	}
	params, err := noSuperblockParams(hashOffset, blockSize)
	if err != nil {
		return "", err
	}
	// Allow an explicit salt/hash-algorithm override via opts for the
	// (uncommon) eager-format path, but keep the no-superblock defaults
	// otherwise.  The converter never sets these.
	if opts != nil {
		if opts.HashAlgorithm != "" {
			params.HashName = opts.HashAlgorithm
		}
		if opts.Salt != "" {
			salt, saltSize, serr := utils.ApplySalt(opts.Salt, 256)
			if serr != nil {
				return "", fmt.Errorf("invalid salt: %w", serr)
			}
			params.Salt = salt
			params.SaltSize = saltSize
		}
	}

	loopParams := mount.LoopParams{
		Readonly:  true,
		Autoclear: true,
	}

	dataLoop, err := mount.SetupLoop(dataDevice, loopParams)
	if err != nil {
		return "", fmt.Errorf("failed to setup loop device for data: %w", err)
	}
	dataLoopDevice := dataLoop.Name()

	var hashLoop *os.File
	var hashLoopDevice string
	if hashDevice != dataDevice {
		hashLoop, err = mount.SetupLoop(hashDevice, loopParams)
		if err != nil {
			dataLoop.Close()
			return "", fmt.Errorf("failed to setup loop device for hash: %w", err)
		}
		hashLoopDevice = hashLoop.Name()
	} else {
		hashLoopDevice = dataLoopDevice
	}

	devicePath, err := verity.Open(&params, name, dataLoopDevice, hashLoopDevice, rootDigest, "", nil)
	if err != nil {
		dataLoop.Close()
		if hashLoop != nil {
			hashLoop.Close()
		}
		return "", fmt.Errorf("failed to open dm-verity device: %w", err)
	}

	// Close file handles now that dm-verity holds a kernel reference to the loop devices.
	dataLoop.Close()
	if hashLoop != nil {
		hashLoop.Close()
	}

	return devicePath, nil
}

func Close(name string) error {
	if err := verity.Close(name); err != nil {
		return fmt.Errorf("failed to close dm-verity device: %w", err)
	}
	return nil
}

// VerifyDevice ensures an existing dm-verity device matches the expected metadata and is healthy.
func VerifyDevice(name string, rootHash string) error {
	hexHash, err := normalizeRootHash(rootHash)
	if err != nil {
		return fmt.Errorf("invalid root hash: %w", err)
	}
	rootDigest, err := utils.ParseRootHash(hexHash)
	if err != nil {
		return fmt.Errorf("invalid root hash: %w", err)
	}

	// Use library's Check to verify device status and root hash
	if !verity.Check(name, rootDigest) {
		return fmt.Errorf("dm-verity device %q verification failed", name)
	}

	return nil
}
