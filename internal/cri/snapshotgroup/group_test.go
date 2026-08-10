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

package snapshotgroup

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/containerd/containerd/v2/core/snapshots"
	criconfig "github.com/containerd/containerd/v2/internal/cri/config"
)

type fakeLookup struct {
	capabilities map[string][]string
	err          error
	calls        int
}

func (f *fakeLookup) GetSnapshotterCapabilities(_ context.Context, snapshotter string) ([]string, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.capabilities[snapshotter], nil
}

func TestResolverEnabled(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mode        string
		snapshotter string
		want        bool
		wantCalls   int
	}{
		{
			name:        "AutoWithSupport",
			mode:        criconfig.SnapshotGroupingAuto,
			snapshotter: "erofs",
			want:        true,
			wantCalls:   1,
		},
		{
			name:        "AutoWithoutSupport",
			mode:        criconfig.SnapshotGroupingAuto,
			snapshotter: "overlayfs",
			want:        false,
			wantCalls:   1,
		},
		{
			name:        "AutoUnknownSnapshotter",
			mode:        criconfig.SnapshotGroupingAuto,
			snapshotter: "nothere",
			want:        false,
			wantCalls:   1,
		},
		{
			// "on" must not need an introspection call: it is how an
			// operator forces grouping for a snapshotter which does
			// not advertise the capability.
			name:        "OnSkipsLookup",
			mode:        criconfig.SnapshotGroupingOn,
			snapshotter: "overlayfs",
			want:        true,
			wantCalls:   0,
		},
		{
			name:        "OffSkipsLookup",
			mode:        criconfig.SnapshotGroupingOff,
			snapshotter: "erofs",
			want:        false,
			wantCalls:   0,
		},
		{
			// An empty mode is what a runtime handler configured
			// before this option existed will have.
			name:        "EmptyModeBehavesAsAuto",
			mode:        "",
			snapshotter: "erofs",
			want:        true,
			wantCalls:   1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookup := &fakeLookup{capabilities: map[string][]string{
				"erofs":     {"remap-ids", "rebase", Capability},
				"overlayfs": {"remap-ids", "rebase"},
			}}
			r := NewResolver(lookup)

			got, err := r.Enabled(context.Background(), tc.mode, tc.snapshotter)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.wantCalls, lookup.calls)
		})
	}
}

func TestResolverCachesLookup(t *testing.T) {
	lookup := &fakeLookup{capabilities: map[string][]string{"erofs": {Capability}}}
	r := NewResolver(lookup)

	for range 3 {
		got, err := r.Enabled(context.Background(), criconfig.SnapshotGroupingAuto, "erofs")
		require.NoError(t, err)
		assert.True(t, got)
	}
	assert.Equal(t, 1, lookup.calls, "capability lookup should be cached per snapshotter")
}

func TestResolverLookupError(t *testing.T) {
	lookup := &fakeLookup{err: errors.New("introspection unavailable")}
	r := NewResolver(lookup)

	_, err := r.Enabled(context.Background(), criconfig.SnapshotGroupingAuto, "erofs")
	assert.Error(t, err)

	// A forced mode must still work without introspection.
	got, err := r.Enabled(context.Background(), criconfig.SnapshotGroupingOn, "erofs")
	require.NoError(t, err)
	assert.True(t, got)
}

func TestResolverLabelOpt(t *testing.T) {
	lookup := &fakeLookup{capabilities: map[string][]string{
		"erofs":     {Capability},
		"overlayfs": nil,
	}}
	r := NewResolver(lookup)
	ctx := context.Background()

	opt, err := r.LabelOpt(ctx, criconfig.SnapshotGroupingAuto, "erofs", "pod-1")
	require.NoError(t, err)
	require.NotNil(t, opt)

	var info snapshots.Info
	require.NoError(t, opt(&info))
	assert.Equal(t, "pod-1", info.Labels[snapshots.LabelSnapshotGroup])

	// Snapshotters without support get no label at all, so they are
	// not asked to honour something they will ignore.
	opt, err = r.LabelOpt(ctx, criconfig.SnapshotGroupingAuto, "overlayfs", "pod-1")
	require.NoError(t, err)
	assert.Nil(t, opt)
}

// The group label must be an inherited label, otherwise the metadata
// snapshotter strips it before the backend ever sees it.
func TestGroupLabelIsInherited(t *testing.T) {
	filtered := snapshots.FilterInheritedLabels(Labels("pod-1"))
	assert.Equal(t, "pod-1", filtered[snapshots.LabelSnapshotGroup])
}

func TestScratchKeyIsPerSandbox(t *testing.T) {
	a := ScratchKey("pod-1")
	b := ScratchKey("pod-2")
	assert.NotEqual(t, a, b)
	// Must not collide with the pause container's snapshot key, which
	// is the sandbox id itself.
	assert.NotEqual(t, "pod-1", a)
}
