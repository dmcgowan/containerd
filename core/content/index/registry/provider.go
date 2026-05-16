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

// Package registry provides a ByteProvider implementation that fetches indexed
// content blobs from an OCI-compliant container registry.
//
// The provider uses a containerd remotes.Fetcher to download the full blob
// from the registry and exposes it as an io.ReaderAt.  This is the "eager"
// fetch path: the entire blob is downloaded before the caller can read it.
// The indexed content store ingests the blob immediately through its Writer,
// splitting it into chunks and extras.
package registry

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/containerd/containerd/v2/core/content"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/core/remotes"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Provider implements contentindex.ByteProvider by fetching blobs from a
// registry via a remotes.Fetcher.
//
// Open downloads the full blob to memory and returns a bytes-backed ReaderAt.
// The caller (the indexed content store) then ingests the blob into its chunk
// store via a Writer call.
type Provider struct {
	fetcher remotes.Fetcher
	name    string
}

// New returns a Provider that uses fetcher to download blobs.
// name is a human-readable identifier used in log output (e.g. "registry:example.com/myimage").
func New(fetcher remotes.Fetcher, name string) *Provider {
	return &Provider{fetcher: fetcher, name: name}
}

// Name implements contentindex.ByteProvider.
func (p *Provider) Name() string { return p.name }

// Open downloads the blob described by desc and returns a ReaderAt backed by
// the downloaded bytes.  The returned ReaderAt is valid for the lifetime of
// the caller's context.
func (p *Provider) Open(ctx context.Context, desc ocispec.Descriptor) (content.ReaderAt, error) {
	rc, err := p.fetcher.Fetch(ctx, desc)
	if err != nil {
		return nil, fmt.Errorf("registry provider: fetch %s: %w", desc.Digest, err)
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("registry provider: read %s: %w", desc.Digest, err)
	}
	if int64(len(data)) != desc.Size {
		return nil, fmt.Errorf("registry provider: expected %d bytes for %s, got %d", desc.Size, desc.Digest, len(data))
	}

	return &bytesReaderAt{r: bytes.NewReader(data)}, nil
}

// bytesReaderAt adapts bytes.Reader to content.ReaderAt.
type bytesReaderAt struct {
	r *bytes.Reader
}

func (b *bytesReaderAt) ReadAt(p []byte, off int64) (int, error) {
	return b.r.ReadAt(p, off)
}

func (b *bytesReaderAt) Size() int64 { return b.r.Size() }

func (b *bytesReaderAt) Close() error { return nil }

// compile-time check
var _ contentindex.ByteProvider = (*Provider)(nil)
var _ content.ReaderAt = (*bytesReaderAt)(nil)
