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

package dmverity

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/testutil"
	"github.com/docker/go-units"
	"github.com/stretchr/testify/assert"
)

const (
	testDeviceName = "test-verity-device"
)

func TestDMVerity(t *testing.T) {
	testutil.RequiresRoot(t)

	supported, err := IsSupported()
	if !supported || err != nil {
		t.Skipf("dm-verity is not supported on this system: %v", err)
	}

	t.Run("IsSupported", func(t *testing.T) {
		supported, err := IsSupported()
		assert.True(t, supported)
		assert.NoError(t, err)
	})

	// The stack is no-superblock only: the merkle tree is stored on the
	// same device as the data, starting at hashOffset (= the data extent),
	// and Open reconstructs every parameter from (hashOffset, blockSize).
	// This mirrors exactly how the block-mount / EROFS-mount handlers open
	// a converted verity layer.
	t.Run("NoSuperblock_SameDevice_RoundTrip", func(t *testing.T) {
		tempDir := t.TempDir()
		_, loopDevice := createLoopbackDevice(t, tempDir, "1Mb")
		defer func() {
			assert.NoError(t, mount.DetachLoopDevice(loopDevice))
		}()

		// 1 MiB data region (256 blocks of 4096); tree stored after it.
		const hashOffset = 1048576
		opts := testOptions(hashOffset)

		rootHash, err := Format(loopDevice, loopDevice, &opts)
		assert.NoError(t, err)
		assert.NotEmpty(t, rootHash)

		// Open the way the mount handlers do: pass hashOffset + block-size
		// opts; NoSuperblock is unconditional inside Open.
		deviceName := testDeviceName + "-nosb-same"
		devicePath, err := Open(loopDevice, deviceName, loopDevice, rootHash, hashOffset,
			&DmverityOptions{DataBlockSize: 4096, HashBlockSize: 4096, HashOffset: hashOffset})
		assert.NoError(t, err)
		assert.Equal(t, "/dev/mapper/"+deviceName, devicePath)

		waitForDevice(t, devicePath)

		err = Close(deviceName)
		assert.NoError(t, err)

		_, err = os.Stat(devicePath)
		assert.True(t, os.IsNotExist(err))
	})
}

func createLoopbackDevice(t *testing.T, dir string, size string) (string, string) {
	t.Helper()
	file, err := os.CreateTemp(dir, "dmverity-tests-")
	assert.NoError(t, err)

	sizeInBytes, err := units.RAMInBytes(size)
	assert.NoError(t, err)

	err = file.Truncate(sizeInBytes * 2)
	assert.NoError(t, err)

	err = file.Close()
	assert.NoError(t, err)

	imagePath := file.Name()

	loopDevice, err := mount.AttachLoopDevice(imagePath)
	assert.NoError(t, err)

	return imagePath, loopDevice
}

// waitForDevice waits for a device-mapper device to appear in /dev/mapper
func waitForDevice(t *testing.T, devicePath string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(devicePath); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("device %s did not appear after waiting", devicePath)
}

// testOptions creates DmverityOptions for testing with common defaults
func testOptions(hashOffset uint64) DmverityOptions {
	return DmverityOptions{
		Salt:          "0000000000000000000000000000000000000000000000000000000000000000",
		HashAlgorithm: "sha256",
		DataBlockSize: 4096,
		HashBlockSize: 4096,
		DataBlocks:    256,
		HashOffset:    hashOffset,
		HashType:      1,
		NoSuperblock:  true,
	}
}

// createTempFile creates a temporary file with optional data, returns file path and cleanup function
func createTempFile(t *testing.T, data []byte) string {
	t.Helper()
	file, err := os.CreateTemp("", "test-data-*.img")
	assert.NoError(t, err)
	if len(data) > 0 {
		_, err = file.Write(data)
		assert.NoError(t, err)
	}
	err = file.Close()
	assert.NoError(t, err)
	t.Cleanup(func() { os.Remove(file.Name()) })
	return file.Name()
}

func TestMetadataPath(t *testing.T) {
	assert.Equal(t, "/path/to/layer.erofs.dmverity", MetadataPath("/path/to/layer.erofs"))
	assert.Equal(t, "/path/to/layer.erofs.dmverity", MetadataPath("/path/to/layer.erofs.dmverity"))
}

func TestDevicePath(t *testing.T) {
	assert.Equal(t, "/dev/mapper/test-device", DevicePath("test-device"))
	assert.Equal(t, "/dev/mapper/containerd-erofs-abc123", DevicePath("containerd-erofs-abc123"))
}

func TestReadMetadata(t *testing.T) {
	tmpDir := t.TempDir()

	createMetadataFile := func(filename, content string) string {
		layerBlob := tmpDir + "/" + strings.TrimSuffix(filename, ".dmverity")
		os.WriteFile(tmpDir+"/"+filename, []byte(content), 0644)
		return layerBlob
	}

	// Valid case
	layerBlob := createMetadataFile("layer.erofs.dmverity", `{"roothash":"abc123def456789012345678901234567890123456789012345678901234","hashoffset":12288}`)
	metadata, err := ReadMetadata(layerBlob)
	assert.NoError(t, err)
	assert.Equal(t, "abc123def456789012345678901234567890123456789012345678901234", metadata.RootHash)
	assert.Equal(t, uint64(12288), metadata.HashOffset)

	// Valid case with pretty-printed JSON
	layerBlob = createMetadataFile("layer2.erofs.dmverity", `{
  "roothash": "def456789012345678901234567890123456789012345678901234567890",
  "hashoffset": 16384
}`)
	metadata, err = ReadMetadata(layerBlob)
	assert.NoError(t, err)
	assert.Equal(t, "def456789012345678901234567890123456789012345678901234567890", metadata.RootHash)
	assert.Equal(t, uint64(16384), metadata.HashOffset)

	// Error: empty root hash
	layerBlob = createMetadataFile("layer3.erofs.dmverity", `{"roothash":"","hashoffset":12288}`)
	_, err = ReadMetadata(layerBlob)
	assert.ErrorContains(t, err, "missing root hash")

	// Error: missing root hash field
	layerBlob = createMetadataFile("layer4.erofs.dmverity", `{"hashoffset":12288}`)
	_, err = ReadMetadata(layerBlob)
	assert.ErrorContains(t, err, "missing root hash")

	// Error: invalid JSON
	layerBlob = createMetadataFile("layer5.erofs.dmverity", `not valid json`)
	_, err = ReadMetadata(layerBlob)
	assert.ErrorContains(t, err, "failed to parse")

	// Error: file not found
	_, err = ReadMetadata(tmpDir + "/nonexistent.erofs")
	assert.ErrorContains(t, err, "failed to read metadata file")
}

// TestErrorHandling tests error cases for dm-verity functions
func TestErrorHandling(t *testing.T) {
	testutil.RequiresRoot(t)

	isSupported, err := IsSupported()
	if err != nil || !isSupported {
		t.Skip("dm-verity not supported on this system")
	}

	t.Run("Open_EmptyRootHash", func(t *testing.T) {
		dataFile := createTempFile(t, nil)
		_, err := Open(dataFile, "test-device", dataFile, "", 0, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "rootHash cannot be empty")
	})

	t.Run("Open_InvalidRootHash", func(t *testing.T) {
		dataFile := createTempFile(t, nil)
		_, err := Open(dataFile, "test-device", dataFile, "not-a-valid-hex-string", 0, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid root hash")
	})

	t.Run("Open_NonexistentDevice", func(t *testing.T) {
		_, err = Open("/nonexistent/device.img", "test-device", "/nonexistent/device.img", "abc123", 4096, nil)
		assert.Error(t, err)
	})

	t.Run("Open_ZeroHashOffset", func(t *testing.T) {
		// No-superblock open requires a non-zero hashOffset (the tree
		// location); zero is rejected before any device I/O.
		dataFile := createTempFile(t, nil)
		_, err := Open(dataFile, "test-device", dataFile, "abc123def456", 0, nil)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "hashOffset required")
	})

	t.Run("Open_InvalidSalt", func(t *testing.T) {
		dataFile := createTempFile(t, nil)
		opts := DefaultDmverityOptions()
		opts.Salt = "invalid-hex-string"
		// Non-zero, block-aligned hashOffset so parsing reaches the salt.
		_, err := Open(dataFile, "test-device", dataFile, "abc123def456", 4096, opts)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid salt")
	})

	t.Run("Close_NonexistentDevice", func(t *testing.T) {
		err := Close("nonexistent-device-12345")
		assert.Error(t, err)
	})

	t.Run("VerifyDevice_EmptyRootHash", func(t *testing.T) {
		err := VerifyDevice("test-device", "")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid root hash")
	})

	t.Run("VerifyDevice_InvalidRootHash", func(t *testing.T) {
		err := VerifyDevice("test-device", "not-valid-hex")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid root hash")
	})

	t.Run("VerifyDevice_NonexistentDevice", func(t *testing.T) {
		err := VerifyDevice("nonexistent-device-12345", "abc123def456")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "verification failed")
	})

	t.Run("Format_InvalidSalt", func(t *testing.T) {
		dataFile := createTempFile(t, make([]byte, 1024*1024))
		opts := DefaultDmverityOptions()
		opts.Salt = "invalid-hex-string-not-valid"
		_, err := Format(dataFile, dataFile, opts)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid salt")
	})

	t.Run("Format_InvalidUUID", func(t *testing.T) {
		dataFile := createTempFile(t, make([]byte, 1024*1024))
		opts := DefaultDmverityOptions()
		opts.UUID = "not-a-valid-uuid-format"
		_, err := Format(dataFile, dataFile, opts)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid UUID")
	})
}
