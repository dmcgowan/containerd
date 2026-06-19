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

// Tests the snapshotter's sidecar-driven block-mount option pipeline:
//
//   layer.indexed (present) + layer.dmverity (present, valid)
//     →
//   block mount with dmverity-roothash/hashoffset/blocksize options
//
// Bypasses NewSnapshotter() to avoid the kernel-EROFS check; these
// tests run on any host.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/containerd/containerd/v2/internal/dmverity"
	blockplugin "github.com/containerd/containerd/v2/plugins/mount/block"
)

func TestNewBlockMount_noSidecarOmitsVerity(t *testing.T) {
	s, _ := snapshotterForMountTest(t)
	s.cacheRoot = t.TempDir()

	// No layer.dmverity → no verity options.
	const snapID = "snap-no-verity"
	snapshotDir(t, s.root, snapID)
	m := s.newBlockMount(snapID, "sha256:abc")
	for _, opt := range m.Options {
		if strings.HasPrefix(opt, "dmverity-") {
			t.Errorf("unexpected verity option %q for sidecar-less snapshot", opt)
		}
	}
	// Sanity: the standard options must still be present.
	requireHasOption(t, m.Options, "blockid=sha256:abc")
	requireHasOption(t, m.Options, "fill=sparse")
}

func TestNewBlockMount_sidecarThreadsAllVerityOptions(t *testing.T) {
	s, _ := snapshotterForMountTest(t)
	s.cacheRoot = t.TempDir()

	const snapID = "snap-with-verity"
	dir := snapshotDir(t, s.root, snapID)
	err := dmverity.WriteMetadata(filepath.Join(dir, "layer.dmverity"), &dmverity.DmverityMetadata{
		RootHash:   "sha256:deadbeef0123",
		HashOffset: 8388608,
		BlockSize:  4096,
	})
	require.NoError(t, err)

	m := s.newBlockMount(snapID, "sha256:xyz")
	requireHasOption(t, m.Options, blockplugin.OptDmVerityRootHash+"sha256:deadbeef0123")
	requireHasOption(t, m.Options, blockplugin.OptDmVerityHashOffset+"8388608")
	requireHasOption(t, m.Options, blockplugin.OptDmVerityBlockSize+"4096")
}

func TestNewBlockMount_sidecarBlockSizeOmittedWhenZero(t *testing.T) {
	// When the sidecar BlockSize is 0 (the marker convention for
	// "use the default 4096" — older sidecars and ones that
	// dropped the optional annotation at convert time), the
	// snapshotter must NOT emit dmverity-blocksize.  The
	// downstream handler then uses dmverity.DefaultBlockSize.
	s, _ := snapshotterForMountTest(t)
	s.cacheRoot = t.TempDir()

	const snapID = "snap-default-blocksize"
	dir := snapshotDir(t, s.root, snapID)
	err := dmverity.WriteMetadata(filepath.Join(dir, "layer.dmverity"), &dmverity.DmverityMetadata{
		RootHash:   "sha256:abcd",
		HashOffset: 1048576,
		// BlockSize: 0 → use default
	})
	require.NoError(t, err)

	m := s.newBlockMount(snapID, "sha256:xyz")
	requireHasOption(t, m.Options, blockplugin.OptDmVerityRootHash+"sha256:abcd")
	requireHasOption(t, m.Options, blockplugin.OptDmVerityHashOffset+"1048576")
	for _, opt := range m.Options {
		if strings.HasPrefix(opt, blockplugin.OptDmVerityBlockSize) {
			t.Errorf("dmverity-blocksize emitted with sidecar BlockSize=0: %q", opt)
		}
	}
}

func TestReadLayerDmverity_absentReturnsNoOk(t *testing.T) {
	s, _ := snapshotterForMountTest(t)
	const snapID = "snap-no-marker"
	snapshotDir(t, s.root, snapID) // dir exists but no layer.dmverity
	m, ok := s.readLayerDmverity(snapID)
	if ok || m != nil {
		t.Errorf("readLayerDmverity = (%v, %v), want (nil, false) for absent sidecar", m, ok)
	}
}

func TestReadLayerDmverity_malformedTreatedAsAbsent(t *testing.T) {
	// A torn / hand-corrupted sidecar must not crash the
	// snapshotter — verity-on-but-broken at marker-read time is
	// downgraded to verity-absent.  The hard-fail policy applies
	// at MOUNT time, where the option-set is the authoritative
	// signal of "verity requested".
	s, _ := snapshotterForMountTest(t)
	const snapID = "snap-bad-marker"
	dir := snapshotDir(t, s.root, snapID)
	path := filepath.Join(dir, "layer.dmverity")
	require.NoError(t, writeBytes(path, []byte("not json")))
	m, ok := s.readLayerDmverity(snapID)
	if ok || m != nil {
		t.Errorf("readLayerDmverity on bad JSON = (%v, %v), want (nil, false)", m, ok)
	}
}

// ── small local helpers ──────────────────────────────────────────────

func requireHasOption(t *testing.T, opts []string, want string) {
	t.Helper()
	for _, o := range opts {
		if o == want {
			return
		}
	}
	t.Errorf("option %q missing from %v", want, opts)
}

func writeBytes(path string, b []byte) error {
	return os.WriteFile(path, b, 0644)
}
