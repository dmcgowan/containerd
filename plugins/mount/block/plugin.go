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

// Package block registers a containerd mount handler for the "block" mount type.
//
// The handler is identified as "block" and registered under the
// io.containerd.mount-handler.v1 plugin type. The mount manager discovers it
// at startup via the MountHandlerPlugin registry entry.
//
// Dependencies:
//   - io.containerd.content.index.v1  (the indexed content store)
//   - A cache.LocalCache instance created from the same store and the
//     containerd content store
//
// The handler itself is stateless; all mutable mount state lives in the
// Handler value returned by NewHandler.
package block

import (
	"fmt"
	"path/filepath"

	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/content/index/cache"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	coremount "github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/plugins"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: plugins.MountHandlerPlugin,
		ID:   "block",
		Requires: []plugin.Type{
			plugins.ContentPlugin,
			plugins.ContentIndexPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (any, error) {
			// Resolve the content store.
			cs, err := ic.GetSingle(plugins.ContentPlugin)
			if err != nil {
				return nil, fmt.Errorf("block mount handler: get content store: %w", err)
			}
			contentStore, ok := cs.(content.Store)
			if !ok {
				return nil, fmt.Errorf("block mount handler: content plugin is not a content.Store")
			}

			// Resolve the indexed content store.
			idx, err := ic.GetSingle(plugins.ContentIndexPlugin)
			if err != nil {
				return nil, fmt.Errorf("block mount handler: get content index store: %w", err)
			}
			indexStore, ok := idx.(contentindex.Store)
			if !ok {
				return nil, fmt.Errorf("block mount handler: index plugin is not a contentindex.Store")
			}

			// Determine the state root for the cache.
			root := filepath.Join(ic.Properties[plugins.PropertyRootDir], "block-cache")

			c := cache.New(root, indexStore, contentStore)
			return NewHandler(indexStore, c), nil
		},
	})
}

// blockHandlerID is the plugin ID used when registering the block mount handler.
// Exported so other packages can reference the string without importing this package.
const blockHandlerID = "block"

// MountType is the mount type string the handler accepts.
const MountType = "block"

// NewBlockMount constructs a mount.Mount entry for a lazy-loaded blob.
func NewBlockMount(blobDigest string, extraOpts ...string) coremount.Mount {
	opts := append([]string{"target=erofs", "ro"}, extraOpts...)
	return coremount.Mount{
		Type:    MountType,
		Source:  blobDigest,
		Options: opts,
	}
}


