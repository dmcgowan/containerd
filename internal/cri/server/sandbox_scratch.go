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

package server

import (
	"context"
	"fmt"
	"maps"

	"github.com/containerd/log"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1"

	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	criconfig "github.com/containerd/containerd/v2/internal/cri/config"
	"github.com/containerd/containerd/v2/internal/cri/snapshotgroup"
)

// createPodScratchSnapshot creates the writable scratch layer which
// backs every container in a pod, and returns the mounts describing
// it.
//
// The scratch layer has no parent, so it is the pod's backing resource
// itself rather than a layer over an image. Handing it to the sandbox
// at creation time lets a VM based runtime attach a correctly sized
// device up front, instead of having to reserve one and swap it out
// later, or guess at a size. Containers are then added as directories
// inside the mounted filesystem.
//
// This only applies to sandboxes managed by a shim. With the in
// process podsandbox controller the pause container's snapshot is the
// first member of the group and there is nothing to hand over.
//
// Returns nil mounts when grouping does not apply, in which case the
// sandbox is created without a rootfs as before.
func (c *criService) createPodScratchSnapshot(ctx context.Context, id string, ociRuntime criconfig.Runtime, config *runtime.PodSandboxConfig, lease leases.Lease) ([]mount.Mount, error) {
	if ociRuntime.Sandboxer == string(criconfig.ModePodSandbox) {
		return nil, nil
	}

	snapshotter := c.RuntimeSnapshotter(ctx, ociRuntime)
	enabled, err := c.snapshotGroups.Enabled(ctx, ociRuntime.SnapshotGrouping, snapshotter)
	if err != nil || !enabled {
		return nil, err
	}

	// Inherited annotations carry through any snapshot settings the pod
	// asked for, notably containerd.io/snapshot/max-size, which sizes
	// the resource for the pod as a whole. The snapshotter falls back
	// to its own default when none is given.
	labels := snapshots.FilterInheritedLabels(config.GetAnnotations())
	if labels == nil {
		labels = map[string]string{}
	}
	maps.Copy(labels, snapshotgroup.Labels(id))

	// The snapshot is created under the pod's lease so that it is
	// released when the pod is removed.
	ctx = leases.WithLease(ctx, lease.ID)

	key := snapshotgroup.ScratchKey(id)
	mounts, err := c.client.SnapshotService(snapshotter).Prepare(ctx, key, "", snapshots.WithLabels(labels))
	if err != nil {
		return nil, fmt.Errorf("failed to create scratch snapshot for sandbox %q: %w", id, err)
	}

	log.G(ctx).WithFields(log.Fields{
		"podsandboxid": id,
		"snapshotter":  snapshotter,
		"key":          key,
	}).Debug("created pod scratch snapshot")

	return mounts, nil
}

// scratchSnapshotBackRef returns the sandbox label which ties the
// sandbox's mount activation to its scratch snapshot, so the
// activation is collected with the snapshot if the sandbox is never
// cleanly shut down.
func scratchSnapshotBackRef(snapshotter, sandboxID string) (string, string) {
	return "containerd.io/gc.bref.snapshot." + snapshotter, snapshotgroup.ScratchKey(sandboxID)
}
