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

// Package plugin registers the in-tree implementation of the indexed
// content store as the "io.containerd.content.index.v1" plugin and
// wires it into containerd's metadata-driven garbage collector.
//
// The plugin requires:
//   - MetadataPlugin: provides the *metadata.DB used to register the
//     GC collector for the "containerd.io/gc.ref.content.index.*"
//     label namespace.
//   - ContentPlugin: provides the content store the indexed content
//     store delegates chunk and (optionally) blob storage to.
//
// Optional dependencies:
//   - ContentIndexProviderPlugin (zero or more): byte providers
//     consulted in registration order when the store needs to source
//     blob bytes from a non-local location (registry, cloud volume,
//     P2P).
package plugin

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/containerd/containerd/v2/core/content"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/core/content/index/local"
	"github.com/containerd/containerd/v2/core/metadata"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: plugins.ContentIndexPlugin,
		ID:   "local",
		Requires: []plugin.Type{
			plugins.MetadataPlugin,
			plugins.ContentPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (any, error) {
			md, err := ic.GetSingle(plugins.MetadataPlugin)
			if err != nil {
				return nil, err
			}
			cs, err := ic.GetSingle(plugins.ContentPlugin)
			if err != nil {
				return nil, err
			}
			contentStore, ok := cs.(content.Store)
			if !ok {
				return nil, fmt.Errorf("content-index: content plugin does not implement content.Store: got %T", cs)
			}

			providers, err := ic.GetByType(plugins.ContentIndexProviderPlugin)
			if err != nil && !errors.Is(err, plugin.ErrPluginNotFound) {
				return nil, err
			}
			var byteProviders []contentindex.ByteProvider
			for id, p := range providers {
				bp, ok := p.(contentindex.ByteProvider)
				if !ok {
					return nil, fmt.Errorf("content-index: provider %q does not implement ByteProvider: got %T", id, p)
				}
				byteProviders = append(byteProviders, bp)
			}

			root := ic.Properties[plugins.PropertyRootDir]
			if root == "" {
				return nil, fmt.Errorf("content-index: root directory not configured")
			}
			storeRoot := filepath.Join(root, "content-index")

			store, err := local.NewStore(local.Config{
				Root:      storeRoot,
				Content:   contentStore,
				Providers: byteProviders,
			})
			if err != nil {
				return nil, fmt.Errorf("content-index: open store: %w", err)
			}

			db, ok := md.(*metadata.DB)
			if !ok {
				store.Close()
				return nil, fmt.Errorf("content-index: metadata plugin does not expose *metadata.DB: got %T", md)
			}
			db.RegisterCollectibleResource(metadata.ResourceContentIndex, store.Collector())

			ic.Meta.Exports["root"] = storeRoot
			return store, nil
		},
	})
}
