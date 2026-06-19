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

// Package plugin registers the io.containerd.cache.v1 plugin that owns the
// host-wide sparse-file cache for lazy-ingested EROFS layers.
//
// One LocalCache instance is created per containerd daemon and shared with
// every consumer that needs to materialise lazy chunk bytes on disk:
//
//   - plugins/mount/block         — loop-mounts the backing file
//   - plugins/snapshots/erofs     — emits the backing-file path in the block
//                                   mount descriptor's Source field
//   - plugins/services/blockcache — relays Fill requests from the shim
//                                   fanotify supervisor to the cache
//   - plugins/transfer            — calls Warm after lazy pull so chunks
//                                   stream into the cache in the background
//                                   before the container is run
//
// The cache holds no garbage-collection state of its own.  It is a pure
// digest-keyed byte store (<root>/blobs/<hex>/{data,present.bm}).  Lifetime is
// governed entirely by the indexed-content store: the plugin registers the
// cache's Remove with the index store via SetBlobRemover, and the index
// store's GC collector calls it when the indexed blob a cache caches becomes
// unreferenced in every namespace.  This keeps all lazy-load metadata in the
// single indexed-content bolt DB.
//
// The Meta.Exports["root"] entry advertises the absolute path of the cache
// state root so dependent plugins can derive backing-file paths without a
// live Attach (see cache.BackingFilePath).
package plugin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	"github.com/containerd/containerd/v2/core/content"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/core/content/index/cache"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/opencontainers/go-digest"
)

// blobRemoverSetter is the optional interface the indexed-content store
// implements so the cache can register its on-disk removal with the store's
// GC collector.  When the index store collects an unreferenced blob it calls
// the registered remover with the blob digest.
type blobRemoverSetter interface {
	SetBlobRemover(func(digest.Digest) error)
}

func init() {
	registry.Register(&plugin.Registration{
		Type: plugins.CachePlugin,
		ID:   "local",
		Requires: []plugin.Type{
			plugins.ContentPlugin,
			plugins.ContentIndexPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (any, error) {
			cs, err := ic.GetSingle(plugins.ContentPlugin)
			if err != nil {
				return nil, fmt.Errorf("cache plugin: get content store: %w", err)
			}
			contentStore, ok := cs.(content.Store)
			if !ok {
				return nil, errors.New("cache plugin: content plugin is not a content.Store")
			}

			idx, err := ic.GetSingle(plugins.ContentIndexPlugin)
			if err != nil {
				return nil, fmt.Errorf("cache plugin: get content index store: %w", err)
			}
			indexStore, ok := idx.(contentindex.Store)
			if !ok {
				return nil, errors.New("cache plugin: index plugin is not a contentindex.Store")
			}

			// The cache state root lives inside the plugin's own subdirectory.
			// Cache directories are keyed purely by blob digest and created on
			// demand at Attach time.
			pluginRoot := ic.Properties[plugins.PropertyRootDir]
			// 0755 (not 0700) on the cache plugin root + blobs/ so debug
			// tooling — most notably lazy-viz running as the invoking
			// user while containerd runs as root under
			// containerd-testenv --root — can list per-blob cache
			// directories and SEEK_DATA/SEEK_HOLE the sparse data
			// files.  Contents are decompressed image layers already
			// reachable via the registry pull; no secrets live here.
			if err := os.MkdirAll(pluginRoot, 0755); err != nil {
				return nil, fmt.Errorf("cache plugin: create root: %w", err)
			}
			root := filepath.Join(pluginRoot, "blobs")
			if err := os.MkdirAll(root, 0755); err != nil {
				return nil, fmt.Errorf("cache plugin: create blob root: %w", err)
			}

			c := cache.New(root, indexStore, contentStore)

			// The cache holds no GC state of its own.  Instead, register its
			// Remove with the indexed-content store so the store's GC
			// collector reclaims the on-disk cache when the indexed blob it
			// caches becomes unreferenced in every namespace.
			if setter, ok := idx.(blobRemoverSetter); ok {
				setter.SetBlobRemover(c.Remove)
			}

			// Export the root so other plugins can derive backing-file paths
			// without holding a live Attach.
			ic.Meta.Exports["root"] = root
			return c, nil
		},
	})
}
