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

package v2

import (
	"context"

	"github.com/containerd/typeurl/v2"

	shimmanager "github.com/containerd/containerd/v2/core/shim/manager"
)

// The generic shim manager moved to [github.com/containerd/containerd/v2/core/shim/manager].
// These aliases keep existing importers of core/runtime/v2 compiling.
//
// Deprecated: use github.com/containerd/containerd/v2/core/shim/manager directly.
type (
	// ShimManager manages running shim processes.
	//
	// Deprecated: use manager.ShimManager.
	ShimManager = shimmanager.ShimManager

	// ShimInstance represents a running shim process.
	//
	// Deprecated: use manager.ShimInstance.
	ShimInstance = shimmanager.ShimInstance

	// Bundle is a shim bundle on disk.
	//
	// Deprecated: use manager.Bundle.
	Bundle = shimmanager.Bundle

	// ManagerConfig configures a shim manager.
	//
	// Deprecated: use manager.ManagerConfig.
	ManagerConfig = shimmanager.ManagerConfig

	// ShimConfig is the shim manager plugin configuration.
	//
	// Deprecated: use manager.ShimConfig.
	ShimConfig = shimmanager.ShimConfig

	// StartConfig configures a shim start.
	//
	// Deprecated: use manager.StartConfig.
	StartConfig = shimmanager.StartConfig

	// LoadConfig configures reloading a shim.
	//
	// Deprecated: use manager.LoadConfig.
	LoadConfig = shimmanager.LoadConfig
)

// CurrentShimVersion is the latest shim version supported by containerd.
//
// Deprecated: use manager.CurrentShimVersion.
const CurrentShimVersion = shimmanager.CurrentShimVersion

// NewShimManager creates a manager for v2 shims.
//
// Deprecated: use manager.NewShimManager.
func NewShimManager(config *ManagerConfig) (*ShimManager, error) {
	return shimmanager.NewShimManager(config)
}

// NewBundle returns a new bundle on disk.
//
// Deprecated: use manager.NewBundle.
func NewBundle(ctx context.Context, root, state, id string, spec typeurl.Any) (*Bundle, error) {
	return shimmanager.NewBundle(ctx, root, state, id, spec)
}

// LoadBundle loads an existing bundle from disk.
//
// Deprecated: use manager.LoadBundle.
func LoadBundle(ctx context.Context, root, id string) (*Bundle, error) {
	return shimmanager.LoadBundle(ctx, root, id)
}
