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

// helpers_test.go provides cross-platform test helpers used by snapshotter
// tests and other suites.  These helpers have no platform-specific imports.
package erofs

import (
	"testing"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/platforms"
	godigest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

// erofsPM returns a platform matcher for the local architecture with
// os.features=["erofs"], as required by the erofs-image-spec §5.1.
func erofsPM() platforms.MatchComparer {
	spec := platforms.DefaultSpec()
	spec.OSFeatures = []string{"erofs"}
	return platforms.OnlyStrict(spec)
}

// chainIDs derives the OCI chain ID for each layer in the manifest by reading
// the org.erofs.uncompressed-digest annotation (erofs-image-spec §5.2) or
// falling back to the content-store label / live decompression.
// This function uses only cross-platform packages.
func chainIDs(t *testing.T, c *containerd.Client, layers []ocispec.Descriptor) []string {
	t.Helper()
	ctx, cancel := testContext(t)
	defer cancel()

	var (
		chain []godigest.Digest
		ids   []string
	)
	for _, l := range layers {
		diffID, err := images.UncompressedDigestFromDescriptor(ctx, c.ContentStore(), l)
		require.NoError(t, err, "compute diff ID for layer %s", l.Digest)
		chain = append(chain, diffID)
		ids = append(ids, identity.ChainID(chain).String())
	}
	return ids
}
