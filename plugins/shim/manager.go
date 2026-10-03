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

// Package shim registers the generic shim manager plugin. The manager itself
// lives in core/shim/manager; this package wires it into the containerd plugin
// graph. It is deliberately independent of the task manager: the shim manager is
// also used by the sandbox controller and by managed proxy plugins.
package shim

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/containerd/log"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"

	"github.com/containerd/containerd/v2/core/shim/manager"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/containerd/v2/version"
)

func init() {
	// The shim manager is not only for the task manager; the "shim" sandbox
	// controller and managed proxy plugins also use it to manage shims, so it is
	// registered as an independent plugin.
	registry.Register(&plugin.Registration{
		Type:   plugins.ShimPlugin,
		ID:     "manager",
		Config: &manager.ShimConfig{},
		InitFn: func(ic *plugin.InitContext) (any, error) {
			config := ic.Config.(*manager.ShimConfig)

			// Allow configurable directory
			if config.SocketDir != "" {
				if !filepath.IsAbs(config.SocketDir) {
					return nil, fmt.Errorf("socket_dir must be an absolute path: %q", config.SocketDir)
				}
				config.SocketDir = filepath.Clean(config.SocketDir)
				if len(config.SocketDir) > manager.MaxSocketDirLen {
					return nil, fmt.Errorf("socket_dir length must be no longer than %d characters", manager.MaxSocketDirLen)
				}
			} else {
				config.SocketDir = manager.DefaultSocketDir()
				if config.SocketDir == "" {
					return nil, errors.New("failed to find a suitable socket directory for shim, please configure one")
				}
			}

			return manager.NewShimManager(&manager.ManagerConfig{
				Address:      ic.Properties[plugins.PropertyGRPCAddress],
				TTRPCAddress: ic.Properties[plugins.PropertyTTRPCAddress],
				SocketDir:    config.SocketDir,
				ShimEnv:      config.Env,
				State:        ic.Properties[plugins.PropertyStateDir],
				Root:         ic.Properties[plugins.PropertyRootDir],
			})
		},
		ConfigMigration: func(ctx context.Context, configVersion int, pluginConfigs map[string]any) error {
			// Migrate configurations from io.containerd.runtime.v2.task
			// if the configVersion >= 3 please make sure the config is under io.containerd.shim.v1.manager.
			if configVersion >= version.ConfigVersion {
				return nil
			}
			const originalPluginName = string(plugins.RuntimePluginV2) + ".task"
			original, ok := pluginConfigs[originalPluginName]
			if !ok {
				return nil
			}
			src := original.(map[string]any)
			dest := map[string]any{}

			if v, ok := src["sched_core"]; ok {
				if schedCore, ok := v.(bool); schedCore {
					dest["env"] = []string{"SCHED_CORE=1"}
				} else if !ok {
					log.G(ctx).Warnf("skipping migration for non-boolean 'sched_core' value %v", v)
				}

				delete(src, "sched_core")
			}

			const newPluginName = string(plugins.ShimPlugin) + ".manager"
			pluginConfigs[originalPluginName] = src
			pluginConfigs[newPluginName] = dest
			return nil
		},
	})
}
