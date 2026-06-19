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

package index

import (
	"context"
	"io"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// OpenReader returns an io.ReadCloser over the original blob bytes for desc.
//
// The store's ReaderAt method is the authoritative byte source — it
// reassembles the blob from per-chunk content-store entries plus the
// chunk-index trailer.  OpenReader wraps that ReaderAt in an
// io.NewSectionReader for callers that want a sequential streaming
// interface (e.g. for piping into io.Copy, a hash.Hash, an OCI pusher,
// or a tar extractor) without having to manage offsets explicitly.
//
// The returned ReadCloser owns the underlying ReaderAt and releases it
// on Close.  Closing the reader is required to free the indexed-store
// handle; failing to do so leaks one in-memory handle per call.
//
// Smart routing note: when the caller passes a content.Store backed by
// a ContentAdapter (see core/content/index/local/adapter.go), a
// `desc` carrying the org.erofs.chunk-index.range annotation is
// automatically served from the indexed store by the adapter's own
// ReaderAt — so OpenReader works uniformly across both plain content
// stores and indexed-aware adapters.
func OpenReader(ctx context.Context, s Store, desc ocispec.Descriptor) (io.ReadCloser, error) {
	ra, err := s.ReaderAt(ctx, desc)
	if err != nil {
		return nil, err
	}
	return &readerAtReadCloser{
		Reader: io.NewSectionReader(ra, 0, ra.Size()),
		closer: ra,
	}, nil
}

// readerAtReadCloser pairs a SectionReader's Read with the underlying
// ReaderAt's Close so the indexed-store handle is released when the
// caller is done streaming.
type readerAtReadCloser struct {
	io.Reader
	closer io.Closer
}

func (r *readerAtReadCloser) Close() error {
	return r.closer.Close()
}
