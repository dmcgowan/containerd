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

package manager

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	bootapi "github.com/containerd/containerd/api/runtime/bootstrap/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequireGRPC(t *testing.T) {
	assert.Error(t, requireGRPC(nil))
	assert.Error(t, requireGRPC(&bootapi.BootstrapResult{Protocol: "ttrpc"}))
	assert.NoError(t, requireGRPC(&bootapi.BootstrapResult{Protocol: "grpc"}))
	assert.NoError(t, requireGRPC(&bootapi.BootstrapResult{Protocol: "GRPC"}))
}

// TestCleanupPlugins verifies that bundles for plugins no longer in the keep set
// are removed, while kept ones are left in place. The shim delete invocation
// fails for these fake bundles (no runtime), so cleanup falls back to removing
// the bundle directory, which is the behavior under test.
func TestCleanupPlugins(t *testing.T) {
	state := t.TempDir()
	root := t.TempDir()
	m, err := NewShimManager(&ManagerConfig{State: state, Root: root})
	require.NoError(t, err)

	nsState := filepath.Join(state, PluginNamespace)
	require.NoError(t, os.MkdirAll(filepath.Join(nsState, "keepme"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(nsState, "removeme"), 0o700))
	// A minimal bootstrap.json so LoadBundle succeeds.
	for _, name := range []string{"keepme", "removeme"} {
		require.NoError(t, os.WriteFile(filepath.Join(nsState, name, "bootstrap.json"),
			[]byte(`{"version":2,"protocol":"grpc","address":"/dev/null"}`), 0o600))
	}

	err = m.CleanupPlugins(context.Background(), []string{"keepme"})
	require.NoError(t, err)

	_, err = os.Stat(filepath.Join(nsState, "keepme"))
	assert.NoError(t, err, "kept plugin bundle should remain")

	_, err = os.Stat(filepath.Join(nsState, "removeme"))
	assert.True(t, os.IsNotExist(err), "removed plugin bundle should be gone")
}

func TestCleanupPluginsNoStateDir(t *testing.T) {
	m, err := NewShimManager(&ManagerConfig{State: filepath.Join(t.TempDir(), "missing")})
	require.NoError(t, err)
	assert.NoError(t, m.CleanupPlugins(context.Background(), nil))
}
