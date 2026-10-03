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
	"iter"
	"os"
	"runtime"
	"slices"
	"testing"

	bootapi "github.com/containerd/containerd/api/runtime/bootstrap/v1"
	"github.com/containerd/containerd/api/types"
	srvconfig "github.com/containerd/containerd/v2/cmd/containerd/server/config"
	"github.com/containerd/containerd/v2/version"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testPath = "/tmp/path/for/testing"

func TestCreateTopLevelDirectoriesErrorsWithSamePathForRootAndState(t *testing.T) {
	path := testPath
	err := CreateTopLevelDirectories(&srvconfig.Config{
		Root:  path,
		State: path,
	})
	assert.EqualError(t, err, "root and state must be different paths")
}

func TestCreateTopLevelDirectoriesWithEmptyStatePath(t *testing.T) {
	statePath := ""
	rootPath := testPath
	err := CreateTopLevelDirectories(&srvconfig.Config{
		Root:  rootPath,
		State: statePath,
	})
	assert.EqualError(t, err, "state must be specified")
}

func TestCreateTopLevelDirectoriesWithEmptyRootPath(t *testing.T) {
	statePath := testPath
	rootPath := ""
	err := CreateTopLevelDirectories(&srvconfig.Config{
		Root:  rootPath,
		State: statePath,
	})
	assert.EqualError(t, err, "root must be specified")
}

func TestMigration(t *testing.T) {
	registry.Reset()
	defer registry.Reset()

	configVersion := version.ConfigVersion - 1

	type testConfig struct {
		Migrated    string `toml:"migrated"`
		NotMigrated string `toml:"notmigrated"`
	}

	registry.Register(&plugin.Registration{
		Type:   "io.containerd.test",
		ID:     "t1",
		Config: &testConfig{},
		InitFn: func(ic *plugin.InitContext) (any, error) {
			c, ok := ic.Config.(*testConfig)
			if !ok {
				t.Error("expected first plugin to have configuration")
			} else {
				if c.Migrated != "" {
					t.Error("expected first plugin to have empty value for migrated config")
				}
				if c.NotMigrated != "don't migrate me" {
					t.Errorf("expected first plugin does not have correct value for not migrated config: %q", c.NotMigrated)
				}
			}
			return nil, nil
		},
	})
	registry.Register(&plugin.Registration{
		Type: "io.containerd.new",
		Requires: []plugin.Type{
			"io.containerd.test", // Ensure this test runs second
		},
		ID:     "t2",
		Config: &testConfig{},
		InitFn: func(ic *plugin.InitContext) (any, error) {
			c, ok := ic.Config.(*testConfig)
			if !ok {
				t.Error("expected second plugin to have configuration")
			} else {
				if c.Migrated != "migrate me" {
					t.Errorf("expected second plugin does not have correct value for migrated config: %q", c.Migrated)
				}
				if c.NotMigrated != "" {
					t.Error("expected second plugin to have empty value for not migrated config")
				}
			}
			return nil, nil
		},
		ConfigMigration: func(ctx context.Context, v int, plugins map[string]any) error {
			if v != configVersion {
				t.Errorf("unexpected version: %d", v)
			}
			t1, ok := plugins["io.containerd.test.t1"]
			if !ok {
				t.Error("plugin not set as expected")
				return nil
			}
			conf, ok := t1.(map[string]any)
			if !ok {
				t.Errorf("unexpected config value: %v", t1)
				return nil
			}
			newconf := map[string]any{
				"migrated": conf["migrated"],
			}
			delete(conf, "migrated")
			plugins["io.containerd.new.t2"] = newconf

			return nil
		},
	})

	config := &srvconfig.Config{}
	config.Version = configVersion
	config.Plugins = map[string]any{
		"io.containerd.test.t1": map[string]any{
			"migrated":    "migrate me",
			"notmigrated": "don't migrate me",
		},
	}
	td := t.TempDir()
	b, err := toml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	configPath := td + "/config.toml"
	if err := os.WriteFile(configPath, b, 0700); err != nil {
		t.Fatal(err)
	}

	g := registry.Graph(func(*plugin.Registration) bool { return false })
	plugins := func() iter.Seq[plugin.Registration] {
		return slices.Values(g)
	}
	config = &srvconfig.Config{
		Version: version.ConfigVersion,
	}
	if err := srvconfig.LoadConfigWithPlugins(t.Context(), configPath, plugins, config); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	_, err = New(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
}

func TestApplyPluginMeta(t *testing.T) {
	info := &types.PluginInfo{
		Exports:      map[string]string{"root": "/shim/root", "shared": "from-shim"},
		Capabilities: []string{"shim-cap"},
		Platforms: []*types.Platform{
			{OS: "linux", Architecture: "arm64"},
		},
	}
	boot := &bootapi.BootstrapResult{Protocol: "grpc", Address: "/run/example.sock"}
	require.NoError(t, boot.AddExtension(info))

	ic := &plugin.InitContext{Context: context.Background(), Meta: &plugin.Meta{}}
	applyPluginMeta(ic, boot,
		map[string]string{"shared": "from-config"},
		[]string{"config-cap"},
		v1.Platform{OS: "linux", Architecture: "amd64"},
	)

	// Shim-reported export present, config overrides the shared key, address set.
	assert.Equal(t, "/shim/root", ic.Meta.Exports["root"])
	assert.Equal(t, "from-config", ic.Meta.Exports["shared"])
	assert.Equal(t, "/run/example.sock", ic.Meta.Exports["address"])

	// Config capability first, then shim-reported.
	assert.Equal(t, []string{"config-cap", "shim-cap"}, ic.Meta.Capabilities)

	// Config platform plus shim-reported platform.
	require.Len(t, ic.Meta.Platforms, 2)
	assert.Equal(t, "amd64", ic.Meta.Platforms[0].Architecture)
	assert.Equal(t, "arm64", ic.Meta.Platforms[1].Architecture)
}

func TestApplyPluginMetaAddressNotOverridable(t *testing.T) {
	boot := &bootapi.BootstrapResult{Protocol: "grpc", Address: "/run/real.sock"}
	ic := &plugin.InitContext{Context: context.Background(), Meta: &plugin.Meta{}}
	// Even if config tries to set address, the shim's real address wins.
	applyPluginMeta(ic, boot, map[string]string{"address": "/run/fake.sock"}, nil, v1.Platform{})
	assert.Equal(t, "/run/real.sock", ic.Meta.Exports["address"])
}

func TestLoadPluginsManagedProxyRejectsShimAndAddress(t *testing.T) {
	_, err := LoadPlugins(context.Background(), &srvconfig.Config{
		ProxyPlugins: map[string]srvconfig.ProxyPlugin{
			"bad": {
				Type:    "snapshot",
				Shim:    "io.containerd.snapshotter.example.v1",
				Address: "/run/example.sock",
			},
		},
	})
	assert.ErrorContains(t, err, "mutually exclusive")
}

func TestSetTempDirEnv(t *testing.T) {
	const tempDir = "/tmp/path/for/testing/temp"

	var keys []string
	if runtime.GOOS == "windows" {
		keys = []string{"TEMP", "TMP", "SystemTemp"}
	} else {
		keys = []string{"TMPDIR"}
	}
	for _, k := range keys {
		t.Setenv(k, "")
	}

	setTempDirEnv(tempDir)

	for _, k := range keys {
		if got := os.Getenv(k); got != tempDir {
			t.Errorf("expected %s=%q, got %q", k, tempDir, got)
		}
	}
}
