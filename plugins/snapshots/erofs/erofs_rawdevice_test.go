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
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/containerd/v2/core/snapshots/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCollectRawDevices verifies that collectRawDevices returns device.*.raw
// files in ascending index order and stops at the first gap.
func TestCollectRawDevices(t *testing.T) {
	root := t.TempDir()
	s := &snapshotter{root: root}
	id := "snap1"
	dir := filepath.Join(root, "snapshots", id)
	require.NoError(t, os.MkdirAll(dir, 0755))

	t.Run("no devices", func(t *testing.T) {
		devs := s.collectRawDevices(id)
		assert.Empty(t, devs)
	})

	t.Run("single device at index 0", func(t *testing.T) {
		p := s.rawDevicePath(id, 0)
		os.WriteFile(p, []byte("data0"), 0644)
		defer os.Remove(p)

		devs := s.collectRawDevices(id)
		require.Len(t, devs, 1)
		assert.Equal(t, p, devs[0])
	})

	t.Run("two contiguous devices", func(t *testing.T) {
		p0 := s.rawDevicePath(id, 0)
		p1 := s.rawDevicePath(id, 1)
		os.WriteFile(p0, []byte("d0"), 0644)
		os.WriteFile(p1, []byte("d1"), 0644)
		defer os.Remove(p0)
		defer os.Remove(p1)

		devs := s.collectRawDevices(id)
		require.Len(t, devs, 2)
		assert.Equal(t, p0, devs[0])
		assert.Equal(t, p1, devs[1])
	})

	t.Run("stops at gap: device 0 and 2 but not 1", func(t *testing.T) {
		p0 := s.rawDevicePath(id, 0)
		p2 := s.rawDevicePath(id, 2)
		os.WriteFile(p0, []byte("d0"), 0644)
		os.WriteFile(p2, []byte("d2"), 0644)
		defer os.Remove(p0)
		defer os.Remove(p2)

		devs := s.collectRawDevices(id)
		// Only device 0 — stops at the gap (index 1 missing).
		require.Len(t, devs, 1)
		assert.Equal(t, p0, devs[0])
	})
}

// TestMountFsMeta_WithRawDevices verifies that mountFsMeta includes raw device
// options when preceding parents have device.*.raw files instead of regular
// layer.erofs files.
func TestMountFsMeta_WithRawDevices(t *testing.T) {
	root := t.TempDir()
	s := &snapshotter{root: root}

	// Create snapshot directories for three parents.
	for _, id := range []string{"p0", "p1", "p2"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, "snapshots", id), 0755))
	}

	// p0: has fsmeta.erofs (the merged/top layer).
	require.NoError(t, os.WriteFile(s.fsMetaPath("p0"), []byte("merged"), 0644))

	// p1: regular EROFS layer (has layer.erofs).
	require.NoError(t, os.WriteFile(s.layerBlobPath("p1"), []byte("layer1"), 0644))

	// p2: raw device layer (has device.0.raw).
	require.NoError(t, os.WriteFile(s.rawDevicePath("p2", 0), []byte("raw0"), 0644))

	snap := storage.Snapshot{ParentIDs: []string{"p0", "p1", "p2"}}
	m, ok := s.mountFsMeta(snap, 0)

	require.True(t, ok, "mountFsMeta should succeed")
	assert.Equal(t, s.fsMetaPath("p0"), m.Source)

	// Expected device options: p2 contributes its raw device, p1 contributes
	// its layer.erofs, p0 contributes its raw devices (none → layer.erofs fallback).
	// Traversal order is bottom→top: p2, p1, p0.
	expectedOpts := []string{
		"ro",
		"loop",
		fmt.Sprintf("device=%s", s.rawDevicePath("p2", 0)),
		fmt.Sprintf("device=%s", s.layerBlobPath("p1")),
		fmt.Sprintf("device=%s", s.layerBlobPath("p0")),
	}
	assert.Equal(t, expectedOpts, m.Options)
}

// TestRawDevicePath verifies the filename format for raw device blobs.
func TestRawDevicePath(t *testing.T) {
	root := "/var/lib/containerd/snapshots"
	s := &snapshotter{root: root}
	p := s.rawDevicePath("abc123", 2)
	assert.Equal(t, "/var/lib/containerd/snapshots/snapshots/abc123/device.2.raw", p)
}

// TestCreateErofsMount_ExtraDevices verifies that createErofsMount adds
// device= options for each extra device passed via the variadic argument.
func TestCreateErofsMount_ExtraDevices(t *testing.T) {
	root := t.TempDir()
	s := &snapshotter{root: root, dmverityMode: "off"}

	blobPath := filepath.Join(root, "meta.erofs")
	os.WriteFile(blobPath, []byte("meta"), 0644)

	dev0 := filepath.Join(root, "device.0.raw")
	dev1 := filepath.Join(root, "device.1.raw")

	m, err := s.createErofsMount(blobPath, dev0, dev1)
	require.NoError(t, err)

	assert.Equal(t, "erofs", m.Type)
	assert.Equal(t, blobPath, m.Source)
	assert.Contains(t, m.Options, "ro")
	assert.Contains(t, m.Options, "loop")
	assert.Contains(t, m.Options, "device="+dev0)
	assert.Contains(t, m.Options, "device="+dev1)
}
