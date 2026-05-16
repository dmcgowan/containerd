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

package local

import (
	"context"
	"fmt"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// ContentAdapter implements content.Store by delegating to an indexed content
// store for blobs that carry org.erofs.index.* annotations, and to the regular
// content store for everything else.
//
// This allows the erofs differ and other consumers that accept a content.Store
// to transparently read from the indexed content store: when a +zstd EROFS
// layer with a chunk index is requested, the assembled reader (chunks + extras)
// is returned instead of a raw content-store reader.
//
// The adapter is read-only for indexed blobs (ReaderAt is overridden; Writer
// and other mutating operations delegate to the underlying content store).
type ContentAdapter struct {
	idx content.ReaderAt // interface not directly, but via idxStore below
	cs  content.Store

	idxStore contentindex.Store
}

// NewContentAdapter returns a ContentAdapter that uses idxStore for reads on
// indexed blobs and cs for everything else.
func NewContentAdapter(idxStore contentindex.Store, cs content.Store) *ContentAdapter {
	return &ContentAdapter{
		cs:       cs,
		idxStore: idxStore,
	}
}

// ReaderAt returns a content.ReaderAt for desc.  If desc carries
// org.erofs.index.range annotations and is known to the indexed store, the
// assembled reader (reassembles the blob byte-for-byte from chunks + extras)
// is returned.  Otherwise the regular content store's ReaderAt is used.
func (a *ContentAdapter) ReaderAt(ctx context.Context, desc ocispec.Descriptor) (content.ReaderAt, error) {
	if hasIndexAnnotation(desc) {
		ra, err := a.idxStore.ReaderAt(ctx, desc)
		if err == nil {
			return ra, nil
		}
		// If not found in indexed store, fall through to regular content store.
		if !errdefs.IsNotFound(err) {
			return nil, err
		}
	}
	return a.cs.ReaderAt(ctx, desc)
}

// hasIndexAnnotation reports whether desc carries the chunk-index range
// annotation that marks it as an indexed-content blob.
func hasIndexAnnotation(desc ocispec.Descriptor) bool {
	if desc.Annotations == nil {
		return false
	}
	_, ok := desc.Annotations[contentindex.AnnotationIndexRange]
	return ok
}

// The remaining methods all delegate to the underlying content store.

func (a *ContentAdapter) Info(ctx context.Context, dgst digest.Digest) (content.Info, error) {
	return a.cs.Info(ctx, dgst)
}

func (a *ContentAdapter) Update(ctx context.Context, info content.Info, fieldpaths ...string) (content.Info, error) {
	return a.cs.Update(ctx, info, fieldpaths...)
}

func (a *ContentAdapter) Walk(ctx context.Context, fn content.WalkFunc, filters ...string) error {
	return a.cs.Walk(ctx, fn, filters...)
}

func (a *ContentAdapter) Delete(ctx context.Context, dgst digest.Digest) error {
	return a.cs.Delete(ctx, dgst)
}

func (a *ContentAdapter) Writer(ctx context.Context, opts ...content.WriterOpt) (content.Writer, error) {
	return a.cs.Writer(ctx, opts...)
}

func (a *ContentAdapter) Status(ctx context.Context, ref string) (content.Status, error) {
	return a.cs.Status(ctx, ref)
}

func (a *ContentAdapter) ListStatuses(ctx context.Context, filters ...string) ([]content.Status, error) {
	return a.cs.ListStatuses(ctx, filters...)
}

func (a *ContentAdapter) Abort(ctx context.Context, ref string) error {
	return a.cs.Abort(ctx, ref)
}

// Compile-time check: ContentAdapter implements content.Store.
var _ content.Store = (*ContentAdapter)(nil)

// noopReaderAt is a placeholder; not used directly in the struct but kept for
// documentation of the design.
type noopReaderAt struct{}

func (noopReaderAt) ReadAt([]byte, int64) (int, error) {
	return 0, fmt.Errorf("noopReaderAt: not implemented")
}

func (noopReaderAt) Size() int64        { return 0 }
func (noopReaderAt) Close() error        { return nil }
var _ = time.Second // suppress unused import
