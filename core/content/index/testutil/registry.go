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

// Package testutil provides test helpers for the indexed content store,
// including a minimal in-memory OCI registry that can be used as both a push
// and pull target without requiring a running daemon.
package testutil

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/opencontainers/go-digest"
)

// MemRegistry is a minimal, thread-safe, in-memory OCI Distribution registry.
//
// Supported endpoints:
//
//	HEAD /v2/<repo>/blobs/<digest>
//	GET  /v2/<repo>/blobs/<digest>
//	POST /v2/<repo>/blobs/uploads/
//	PUT  /v2/<repo>/blobs/uploads/<id>?digest=<d>
//	PUT  /v2/<repo>/manifests/<ref>
//	GET  /v2/<repo>/manifests/<ref>
//	HEAD /v2/<repo>/manifests/<ref>
//
// It is intended for unit tests only; it makes no attempt to enforce OCI spec
// conformance beyond what the containerd pusher/fetcher requires.
type MemRegistry struct {
	mu        sync.RWMutex
	blobs     map[string][]byte           // digest → bytes
	manifests map[string]map[string][]byte // repo → (ref|digest) → bytes
	uploads   map[string][]byte           // upload id → accumulated bytes
	mediaTypes map[string]string          // digest → media type (for manifests)
}

// NewMemRegistry returns an initialised, empty registry.
func NewMemRegistry() *MemRegistry {
	return &MemRegistry{
		blobs:      make(map[string][]byte),
		manifests:  make(map[string]map[string][]byte),
		uploads:    make(map[string][]byte),
		mediaTypes: make(map[string]string),
	}
}

// ServeHTTP dispatches OCI distribution API requests.
func (r *MemRegistry) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Path
	method := req.Method

	// /v2/ ping
	if path == "/v2/" || path == "/v2" {
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		w.WriteHeader(http.StatusOK)
		return
	}

	// /v2/<repo>/blobs/uploads[/<id>]
	if idx := strings.Index(path, "/blobs/uploads"); idx > 0 {
		r.handleUpload(w, req, path, method, idx)
		return
	}

	// /v2/<repo>/blobs/<digest>
	if idx := strings.Index(path, "/blobs/sha256:"); idx > 0 || strings.Contains(path, "/blobs/") {
		r.handleBlob(w, req, path, method)
		return
	}

	// /v2/<repo>/manifests/<ref>
	if strings.Contains(path, "/manifests/") {
		r.handleManifest(w, req, path, method)
		return
	}

	http.Error(w, "not found", http.StatusNotFound)
}

func (r *MemRegistry) handleBlob(w http.ResponseWriter, req *http.Request, path, method string) {
	// path: /v2/<repo>/blobs/<digest>
	parts := strings.SplitN(path, "/blobs/", 2)
	if len(parts) != 2 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	dgstStr := parts[1]

	r.mu.RLock()
	data, ok := r.blobs[dgstStr]
	r.mu.RUnlock()

	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	dgst, _ := digest.Parse(dgstStr)
	w.Header().Set("Docker-Content-Digest", dgst.String())
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))

	switch method {
	case http.MethodHead:
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		// Support Range requests so the docker fetcher can do range reads.
		rangeHdr := req.Header.Get("Range")
		if rangeHdr != "" {
			var start, end int
			if _, err := fmt.Sscanf(rangeHdr, "bytes=%d-%d", &start, &end); err == nil {
				if end >= len(data) {
					end = len(data) - 1
				}
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
				w.Header().Set("Content-Length", fmt.Sprintf("%d", end-start+1))
				w.WriteHeader(http.StatusPartialContent)
				w.Write(data[start : end+1])
				return
			}
		}
		w.WriteHeader(http.StatusOK)
		w.Write(data)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (r *MemRegistry) handleUpload(w http.ResponseWriter, req *http.Request, path, method string, uploadIdx int) {
	repoPath := path[:uploadIdx]
	rest := path[uploadIdx+len("/blobs/uploads"):]
	// rest is either "" (POST) or "/<id>" (PUT/PATCH)

	switch method {
	case http.MethodPost:
		// Initiate upload.
		id := fmt.Sprintf("upload-%d", len(r.uploads))
		r.mu.Lock()
		r.uploads[id] = nil
		r.mu.Unlock()
		w.Header().Set("Location", fmt.Sprintf("%s/blobs/uploads/%s", repoPath, id))
		w.Header().Set("Range", "0-0")
		w.WriteHeader(http.StatusAccepted)

	case http.MethodPut:
		// Complete upload.
		id := strings.TrimPrefix(rest, "/")
		dgstStr := req.URL.Query().Get("digest")

		body, err := io.ReadAll(req.Body)
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusInternalServerError)
			return
		}

		r.mu.Lock()
		existing := r.uploads[id]
		combined := append(existing, body...)
		delete(r.uploads, id)
		r.blobs[dgstStr] = combined
		r.mu.Unlock()

		dgst, _ := digest.Parse(dgstStr)
		w.Header().Set("Docker-Content-Digest", dgst.String())
		w.WriteHeader(http.StatusCreated)

	case http.MethodPatch:
		// Append data to ongoing upload.
		id := strings.TrimPrefix(rest, "/")
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.uploads[id] = append(r.uploads[id], body...)
		size := len(r.uploads[id])
		r.mu.Unlock()

		w.Header().Set("Range", fmt.Sprintf("0-%d", size))
		w.WriteHeader(http.StatusAccepted)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (r *MemRegistry) handleManifest(w http.ResponseWriter, req *http.Request, path, method string) {
	// path: /v2/<repo>/manifests/<ref>
	parts := strings.SplitN(path, "/manifests/", 2)
	if len(parts) != 2 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	repo := strings.TrimPrefix(parts[0], "/v2/")
	ref := parts[1]

	switch method {
	case http.MethodPut:
		body, err := io.ReadAll(req.Body)
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusInternalServerError)
			return
		}
		dgst := digest.FromBytes(body)
		mediaType := req.Header.Get("Content-Type")
		if mediaType == "" {
			mediaType = "application/vnd.oci.image.manifest.v1+json"
		}

		r.mu.Lock()
		if r.manifests[repo] == nil {
			r.manifests[repo] = make(map[string][]byte)
		}
		r.manifests[repo][ref] = body
		r.manifests[repo][dgst.String()] = body
		// Also store as a blob so it can be fetched by digest.
		r.blobs[dgst.String()] = body
		r.mediaTypes[dgst.String()] = mediaType
		r.mu.Unlock()

		w.Header().Set("Docker-Content-Digest", dgst.String())
		w.WriteHeader(http.StatusCreated)

	case http.MethodGet, http.MethodHead:
		r.mu.RLock()
		repoManifests := r.manifests[repo]
		var data []byte
		if repoManifests != nil {
			data = repoManifests[ref]
		}
		mt := r.mediaTypes[ref]
		r.mu.RUnlock()

		if data == nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if mt == "" {
			mt = "application/vnd.oci.image.manifest.v1+json"
		}
		dgst := digest.FromBytes(data)
		w.Header().Set("Content-Type", mt)
		w.Header().Set("Docker-Content-Digest", dgst.String())
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))

		if method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write(data)

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// BlobCount returns the number of blobs currently stored.
func (r *MemRegistry) BlobCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.blobs)
}

// HasBlob reports whether the registry has a blob with the given digest.
func (r *MemRegistry) HasBlob(dgst string) bool {
	r.mu.RLock()
	_, ok := r.blobs[dgst]
	r.mu.RUnlock()
	return ok
}

// HasManifest reports whether the registry has a manifest for repo/ref.
func (r *MemRegistry) HasManifest(repo, ref string) bool {
	r.mu.RLock()
	m := r.manifests[repo]
	r.mu.RUnlock()
	if m == nil {
		return false
	}
	_, ok := m[ref]
	return ok
}

// ManifestJSON returns the raw manifest bytes for repo/ref.
func (r *MemRegistry) ManifestJSON(repo, ref string) []byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.manifests[repo] == nil {
		return nil
	}
	return r.manifests[repo][ref]
}

// BlobBytes returns the raw bytes for a blob.
func (r *MemRegistry) BlobBytes(dgstStr string) []byte {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.blobs[dgstStr]
}

// MustParseManifest parses the manifest for repo/ref and returns the OCI
// spec struct.  Panics if the manifest is absent or unparseable.
func (r *MemRegistry) MustParseManifest(repo, ref string) map[string]json.RawMessage {
	data := r.ManifestJSON(repo, ref)
	if data == nil {
		panic(fmt.Sprintf("no manifest for %s/%s", repo, ref))
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		panic(err)
	}
	return m
}
