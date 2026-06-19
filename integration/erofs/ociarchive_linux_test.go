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

//go:build linux

// ociarchive_linux_test.go provides helpers for serving OCI image archives
// (exported by containerd's archive.Export) directly from an in-process
// MemRegistry.  This lets tests pull images via the standard Docker registry
// protocol without any external process or network dependency.
//
// Workflow:
//
//  1. Build a converter.Convert to produce EROFS layers in the daemon's
//     content store.
//  2. Export the converted image to an OCI layout tar (archive.Export).
//  3. Call newOCIArchiveRegistry(t, tarBuf, "myimage:v1") to get a *localReg
//     that serves all the blobs and manifests from the archive.
//  4. Use reg.fetcher(...) or reg.resolver to pull from the daemon.
package erofs

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"path"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/remotes"
	dockerremotes "github.com/containerd/containerd/v2/core/remotes/docker"
	indextestutil "github.com/containerd/containerd/v2/core/content/index/testutil"
	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// newOCIArchiveRegistry reads an OCI layout tar from r, populates a fresh
// MemRegistry with all blobs and manifests, and returns a *localReg whose
// resolver points at the httptest server.
//
// The image is served under the given repo and tag, e.g. "erofs/alpine:lazy".
// All manifests referenced from the archive's index.json are inserted.
func newOCIArchiveRegistry(t *testing.T, r io.Reader, repo, tag string) *localReg {
	t.Helper()

	reg := indextestutil.NewMemRegistry()

	// Read the tar and collect all blobs and the index.json.
	blobs := make(map[string][]byte) // path inside tar → bytes
	var indexJSON []byte

	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("ociarchive: read tar: %v", err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("ociarchive: read entry %s: %v", hdr.Name, err)
		}
		name := path.Clean(hdr.Name)
		if name == ocispec.ImageIndexFile || name == "index.json" {
			indexJSON = data
			continue
		}
		// blobs/sha256/<hex>
		if strings.HasPrefix(name, "blobs/") {
			// store keyed by "sha256:<hex>"
			parts := strings.SplitN(name, "/", 3)
			if len(parts) == 3 {
				key := parts[1] + ":" + parts[2]
				blobs[key] = data
			}
		}
	}

	if indexJSON == nil {
		t.Fatalf("ociarchive: no index.json found in archive")
	}

	// Push all blobs.
	for dgstStr, data := range blobs {
		reg.PutBlob(data)
		// Verify digest matches.
		dgst := godigest.Digest(dgstStr)
		computed := godigest.SHA256.FromBytes(data)
		if computed != dgst {
			t.Fatalf("ociarchive: blob %s digest mismatch: computed %s", dgstStr, computed)
		}
	}

	// Parse index.json and insert manifests.
	var index ocispec.Index
	if err := json.Unmarshal(indexJSON, &index); err != nil {
		t.Fatalf("ociarchive: parse index.json: %v", err)
	}

	// For each manifest in the index:
	for _, mDesc := range index.Manifests {
		mData, ok := blobs[mDesc.Digest.String()]
		if !ok {
			t.Fatalf("ociarchive: manifest blob %s not found", mDesc.Digest)
		}

		// Insert under the tag if this is the top-level manifest or index,
		// and also by its digest.
		reg.PutManifest(repo, mDesc.Digest.String(), mData, mDesc.MediaType)
	}

	// Also insert the index itself as a manifest so "resolve by tag" works.
	indexDgst := reg.PutManifest(repo, tag, indexJSON, ocispec.MediaTypeImageIndex)
	_ = indexDgst

	// Start the HTTP server.
	srv := httptest.NewServer(reg)
	t.Cleanup(srv.Close)
	host := srv.Listener.Addr().String()

	resolver := dockerremotes.NewResolver(dockerremotes.ResolverOptions{
		Hosts: func(h string) ([]dockerremotes.RegistryHost, error) {
			return []dockerremotes.RegistryHost{{
				Client:       srv.Client(),
				Host:         host,
				Scheme:       "http",
				Capabilities: dockerremotes.HostCapabilityPull |
					dockerremotes.HostCapabilityResolve |
					dockerremotes.HostCapabilityPush,
			}}, nil
		},
	})

	return &localReg{
		srv:      srv,
		reg:      reg,
		host:     host,
		resolver: resolver,
	}
}

// imageRef returns the full image reference for a given repo+tag served by
// this registry, e.g. "127.0.0.1:12345/erofs/alpine:lazy".
func (r *localReg) imageRef(repo, tag string) string {
	return fmt.Sprintf("%s/%s:%s", r.host, repo, tag)
}

// resolverFor returns a remotes.Resolver that routes to this registry for
// any hostname (used when the daemon needs to pull from us by our address).
func (r *localReg) resolverFor(_ string) remotes.Resolver {
	return r.resolver
}
