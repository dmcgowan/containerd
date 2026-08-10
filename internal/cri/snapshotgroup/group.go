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

// Package snapshotgroup decides whether the writable snapshots of a
// pod should share a single backing resource, and labels them so that
// they do.
//
// A snapshotter which supports grouping backs every snapshot carrying
// the same group label with one resource, typically a single block
// image holding a directory per container. A sandbox can then attach
// that one device up front and add containers by creating directories
// inside the mounted filesystem, instead of attaching a device per
// container, which a VM may not permit at all.
package snapshotgroup

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/containerd/containerd/v2/core/snapshots"
	criconfig "github.com/containerd/containerd/v2/internal/cri/config"
)

// Capability is advertised by snapshotters which honour
// snapshots.LabelSnapshotGroup.
const Capability = "groups"

// CapabilityLookup reports the capabilities of a snapshotter.
type CapabilityLookup interface {
	GetSnapshotterCapabilities(ctx context.Context, snapshotter string) ([]string, error)
}

// Resolver decides whether grouping applies to a given runtime handler
// and snapshotter, caching the snapshotter capability lookup.
type Resolver struct {
	lookup CapabilityLookup
	cache  sync.Map
}

// NewResolver returns a Resolver which discovers snapshotter support
// through the given lookup.
func NewResolver(lookup CapabilityLookup) *Resolver {
	return &Resolver{lookup: lookup}
}

// Enabled reports whether snapshots created for a pod using this
// runtime handler and snapshotter should be grouped.
//
// In "auto" mode this depends on the snapshotter advertising support.
// Snapshotters which do not support grouping are free to ignore the
// label, but there is no point creating a pod scratch layer for them.
func (r *Resolver) Enabled(ctx context.Context, mode, snapshotter string) (bool, error) {
	switch mode {
	case criconfig.SnapshotGroupingOff:
		return false, nil
	case criconfig.SnapshotGroupingOn:
		return true, nil
	}

	if v, ok := r.cache.Load(snapshotter); ok {
		return v.(bool), nil
	}

	capabilities, err := r.lookup.GetSnapshotterCapabilities(ctx, snapshotter)
	if err != nil {
		return false, fmt.Errorf("failed to get capabilities of snapshotter %q: %w", snapshotter, err)
	}
	supported := slices.Contains(capabilities, Capability)
	r.cache.Store(snapshotter, supported)
	return supported, nil
}

// LabelOpt returns the snapshot option which places a snapshot in the
// pod's group, or nil when grouping does not apply.
//
// Every writable snapshot belonging to a pod carries the same group
// value, which is what lets the snapshotter back them all with one
// resource.
func (r *Resolver) LabelOpt(ctx context.Context, mode, snapshotter, sandboxID string) (snapshots.Opt, error) {
	enabled, err := r.Enabled(ctx, mode, snapshotter)
	if err != nil || !enabled {
		return nil, err
	}
	return snapshots.WithLabels(Labels(sandboxID)), nil
}

// Labels returns the snapshot labels which place a snapshot in a pod's
// group.
func Labels(sandboxID string) map[string]string {
	return map[string]string{
		snapshots.LabelSnapshotGroup: sandboxID,
	}
}

// ScratchKey returns the snapshot key of the writable scratch layer
// which represents a pod's shared backing resource.
//
// The scratch layer has no parent: it is the resource itself rather
// than a layer over an image, so a sandbox receives it as a bare
// device to attach.
func ScratchKey(sandboxID string) string {
	return "sandbox-scratch-" + sandboxID
}
