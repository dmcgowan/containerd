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
	"errors"
	"fmt"

	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

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
			plugins.ContentIndexPlugin,
			plugins.CachePlugin,
		},
		InitFn: func(ic *plugin.InitContext) (any, error) {
			// Resolve the indexed content store (for Info lookups).
			idx, err := ic.GetSingle(plugins.ContentIndexPlugin)
			if err != nil {
				return nil, fmt.Errorf("block mount handler: get content index store: %w", err)
			}
			indexStore, ok := idx.(contentindex.Store)
			if !ok {
				return nil, errors.New("block mount handler: index plugin is not a contentindex.Store")
			}

			// Resolve the shared cache.
			cs, err := ic.GetSingle(plugins.CachePlugin)
			if err != nil {
				return nil, fmt.Errorf("block mount handler: get cache: %w", err)
			}
			c, ok := cs.(cache.Cache)
			if !ok {
				return nil, errors.New("block mount handler: cache plugin is not a cache.Cache")
			}
			return NewHandler(indexStore, c), nil
		},
	})
}

// blockHandlerID is the plugin ID used when registering the block mount handler.
// Exported so other packages can reference the string without importing this package.
const blockHandlerID = "block"

// MountType is the mount type string the handler accepts.
const MountType = "block"

// Option-key constants recognised by both the daemon and shim block
// handlers.  Centralising them here keeps producers (snapshotter) and
// consumers (handlers) in lockstep without sharing a struct.
const (
	// OptTarget names the filesystem to mount over the (loop or
	// file-backed) block device, e.g. "target=erofs".  Default
	// "erofs" is implied if absent.
	OptTarget = "target="

	// OptBlockID is the daemon-side cache key — typically the
	// post-conversion blob digest.  Required.
	OptBlockID = "blockid="

	// OptFill is advisory: "fill=sparse" signals to the shim that
	// the backing file has holes the daemon will fill on demand
	// via the BlockCache Fill stream.  The daemon handler ignores
	// this option (it always knows from the cache handle).
	OptFill = "fill="

	// OptDmVerityRootHash carries the dm-verity merkle-tree root
	// digest, formatted "sha256:<hex>" (matching the format used
	// by the org.erofs.dmverity.root_digest annotation).
	// Presence triggers the verity branch in both handlers.
	OptDmVerityRootHash = "dmverity-roothash="

	// OptDmVerityHashOffset is the uncompressed byte offset where
	// the dm-verity superblock + merkle tree begins (= EROFS
	// filesystem size in bytes).  Required when
	// OptDmVerityRootHash is present.
	OptDmVerityHashOffset = "dmverity-hashoffset="

	// OptDmVerityBlockSize is the dm-verity data block size.
	// Optional; both handlers default to 4096 when absent.
	OptDmVerityBlockSize = "dmverity-blocksize="
)

// DmVerityOptions bundles the three options the block-mount producers
// emit when the underlying lazy layer carries a verity sidecar.
// Construct via DmVerityOpts and append to the block-mount options.
type DmVerityOptions struct {
	RootHash   string // "sha256:<hex>" — must be non-empty to enable verity
	HashOffset uint64
	BlockSize  uint32 // 0 → default 4096
}

// DmVerityOpts returns the option strings for the given verity
// parameters, in the order the daemon/shim handlers expect (the
// order is not actually significant — both handlers parse keyed
// options unordered — but stable ordering makes test assertions
// easier).  Returns nil when v.RootHash is empty (verity disabled).
func DmVerityOpts(v DmVerityOptions) []string {
	if v.RootHash == "" {
		return nil
	}
	opts := []string{
		OptDmVerityRootHash + v.RootHash,
		fmt.Sprintf("%s%d", OptDmVerityHashOffset, v.HashOffset),
	}
	if v.BlockSize != 0 {
		opts = append(opts, fmt.Sprintf("%s%d", OptDmVerityBlockSize, v.BlockSize))
	}
	return opts
}

// NewBlockMount constructs a mount.Mount entry for a block-backed filesystem.
//
//   - source is the local path to the sparse backing file (e.g.
//     "/var/lib/containerd/.../block-cache/<hex>/data").  The shim uses this
//     path directly with losetup without any additional RPC.
//
//   - extraOpts may include:
//     "blockid=<digest>"  — the daemon-side cache key for the Fill stream Hello.
//     "fill=sparse"       — the backing file has holes that need on-demand filling.
//     "target=<fs>"       — filesystem type to mount over the loop device (default: erofs).
//     "ro"                — read-only mount flag.
//     "dmverity-roothash=…", "dmverity-hashoffset=…", "dmverity-blocksize=…" — see
//        DmVerityOpts.  When present the handler sets up a dm-verity
//        target between the loop device and the EROFS mount and
//        HARD-FAILS if the kernel can't honour the request.
func NewBlockMount(source string, extraOpts ...string) coremount.Mount {
	opts := append([]string{"target=erofs", "ro"}, extraOpts...)
	return coremount.Mount{
		Type:    MountType,
		Source:  source,
		Options: opts,
	}
}


