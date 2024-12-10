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

package proxy

import (
	"github.com/containerd/containerd/api/types"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/protobuf"
)

func ActivationInfoToProto(a mount.ActivationInfo) *types.ActivationInfo {
	return &types.ActivationInfo{
		Name:   a.Name,
		Active: ActiveMountToProto(a.Active),
		System: mount.ToProto(a.System),
		Labels: a.Labels,
	}

}

func ActiveMountToProto(a []mount.ActiveMount) []*types.ActiveMount {
	c := make([]*types.ActiveMount, len(a))
	for i, m := range a {
		c[i] = &types.ActiveMount{
			Mount: &types.Mount{
				Type:    m.Type,
				Source:  m.Source,
				Target:  m.Target,
				Options: m.Options,
			},
			MountedAt:  protobuf.ToTimestamp(*m.MountedAt),
			MountPoint: m.MountPoint,
			Data:       m.MountData,
		}
	}
	return c
}
