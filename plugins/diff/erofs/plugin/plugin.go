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

package plugin

import (
	"fmt"

	"github.com/containerd/platforms"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	"github.com/containerd/containerd/v2/core/metadata"
	"github.com/containerd/containerd/v2/internal/dmverity"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/containerd/v2/plugins/diff/erofs"
)

// Config represents configuration for the erofs differ plugin.
type Config struct {
	// MkfsOptions is retained for configuration-file compatibility but is
	// now a no-op: the EROFS differ uses a pure-Go implementation and does
	// not invoke mkfs.erofs.
	MkfsOptions []string `toml:"mkfs_options"`

	// EnableTarIndex enables the tar index mode where only filesystem
	// metadata is stored inline and file content is referenced by position
	// in the original tar stream.
	EnableTarIndex bool `toml:"enable_tar_index"`

	// EnableDmverity enables dm-verity formatting for EROFS layers (Linux only).
	EnableDmverity bool `toml:"enable_dmverity"`
}

func init() {
	registry.Register(&plugin.Registration{
		Type: plugins.DiffPlugin,
		ID:   "erofs",
		Requires: []plugin.Type{
			plugins.MetadataPlugin,
		},
		Config: &Config{},
		InitFn: func(ic *plugin.InitContext) (any, error) {
			// The EROFS differ now uses pure-Go implementations (go-erofs +
			// continuity/tarconv).  No mkfs.erofs binary is required.
			md, err := ic.GetSingle(plugins.MetadataPlugin)
			if err != nil {
				return nil, err
			}

			p := platforms.DefaultSpec()
			p.OS = "linux"
			ic.Meta.Platforms = append(ic.Meta.Platforms, p)
			// Select this differ for EROFS native images by default.
			p.OSFeatures = []string{"erofs"}
			ic.Meta.Platforms = append(ic.Meta.Platforms, p)
			cs := md.(*metadata.DB).ContentStore()
			config := ic.Config.(*Config)

			var opts []erofs.DifferOpt

			if config.EnableTarIndex {
				opts = append(opts, erofs.WithTarIndexMode())
			}

			if config.EnableDmverity {
				supported, err := dmverity.IsSupported()
				if err != nil {
					return nil, fmt.Errorf("dm-verity support check failed: %w", err)
				}
				if !supported {
					return nil, fmt.Errorf("dm-verity is not supported on this system (dm_verity module not loaded): %w", plugin.ErrSkipPlugin)
				}
				opts = append(opts, erofs.WithDmverity())
			}

			return erofs.NewErofsDiffer(cs, opts...), nil
		},
	})
}

// Ensure fmt import is used (ErrSkipPlugin wrapping uses it).
var _ = fmt.Sprintf
