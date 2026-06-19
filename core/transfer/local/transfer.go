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
	"io"
	"net/url"
	"time"

	"github.com/containerd/typeurl/v2"
	"golang.org/x/sync/semaphore"

	"github.com/containerd/errdefs"

	"github.com/containerd/containerd/v2/core/content"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/transfer"
	"github.com/containerd/containerd/v2/core/unpack"
	"github.com/containerd/containerd/v2/pkg/imageverifier"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// LazyIndexStore is the minimal interface the transfer service needs to
// perform a lazy ingest. Implemented by the local indexed-content store
// (core/content/index/local.Store).
//
// The interface is intentionally small so that alternative implementations
// (e.g. remote stores, caching proxies) can satisfy it without pulling in the
// full contentindex.Store contract.
type LazyIndexStore interface {
	WriteLazy(ctx context.Context, ref string, desc ocispec.Descriptor, p contentindex.ByteProvider) error
}

// lazyIndexStore is the unexported alias used inside the package.
type lazyIndexStore = LazyIndexStore

// providerPersister is an optional interface a lazy index store may implement
// to persist registry-provider reconstruction metadata (the registry ref and,
// when supplied, an encrypted credential) so chunk filling can survive a
// daemon restart.  The local indexed-content store satisfies it.
type providerPersister interface {
	PutProvider(ctx context.Context, name, ref string, cred []byte) error
}

// credentialProvider is an optional interface that an ImageFetcher may satisfy
// to expose the registry credential for a specific host as a pre-serialised
// (JSON) byte blob.  Used by lazyLayerHandler to persist reconstruction
// metadata.  OCIRegistry satisfies this interface.
type credentialProvider interface {
	// RegistryCredentialJSON returns the JSON-serialised credential for host.
	// Returns nil and no error when no credential is configured.
	RegistryCredentialJSON(ctx context.Context, host string) ([]byte, error)
}

// registryHostFromRef extracts just the host[:port] from an image ref such as
// "registry.example.com/repo:tag" or "registry.example.com/repo@sha256:…".
func registryHostFromRef(ref string) string {
	// Add a dummy scheme so url.Parse works.
	u, err := url.Parse("dummy://" + ref)
	if err != nil {
		return ""
	}
	return u.Host
}

// LazyCacheWarmer schedules background population of the on-disk cache for
// a lazy-ingested blob.  When configured, the lazy-pull path invokes Warm
// after a successful WriteLazy so the sparse cache file is opportunistically
// filled (at PriorityBackground) before the container is actually run.
// The block-mount handler's foreground EnsureAll then finds the file already
// populated and the mount completes effectively instantly.
//
// PrepareForFSView, when implemented, additionally pre-warms the EROFS
// superblock + inode-table region synchronously before returning.  This
// makes the backing file directly usable as an io.ReaderAt by go-erofs:
// the client-side fsview/block handler can resolve /etc/passwd /
// /etc/group from the sparse file without triggering a kernel mount.
// Implementations that don't (or can't) pre-warm the metadata region
// should still implement the method as a no-op.
//
// The interface mirrors core/content/index/cache.Warmer but is restated here
// to avoid a transfer→cache import.
type LazyCacheWarmer interface {
	Warm(ctx context.Context, desc ocispec.Descriptor, p contentindex.ByteProvider) error
	PrepareForFSView(ctx context.Context, desc ocispec.Descriptor, p contentindex.ByteProvider) error
}

type localTransferService struct {
	content content.Store
	images  images.Store
	// limiter for upload
	limiterU *semaphore.Weighted
	// limiter for download operation
	limiterD *semaphore.Weighted
	// limiter for unpack operation
	limiterP *semaphore.Weighted
	config   TransferConfig
}

func NewTransferService(cs content.Store, is images.Store, tc TransferConfig) transfer.Transferrer {
	ts := &localTransferService{
		content: cs,
		images:  is,
		config:  tc,
	}
	if tc.MaxConcurrentUploadedLayers > 0 {
		ts.limiterU = semaphore.NewWeighted(int64(tc.MaxConcurrentUploadedLayers))
	}
	if tc.MaxConcurrentDownloads > 0 {
		ts.limiterD = semaphore.NewWeighted(int64(tc.MaxConcurrentDownloads))
	}
	// MaxConcurrentUnpacks > 1 enables parallel layer unpack. Value of 0 or 1
	// means sequential (no semaphore). Parallel unpack requires the snapshotter
	// to support the "rebase" capability.
	if tc.MaxConcurrentUnpacks > 1 {
		ts.limiterP = semaphore.NewWeighted(int64(tc.MaxConcurrentUnpacks))
	}
	return ts
}

func (ts *localTransferService) Transfer(ctx context.Context, src any, dest any, opts ...transfer.Opt) error {
	topts := &transfer.Config{}
	for _, opt := range opts {
		opt(topts)
	}

	// Figure out matrix of whether source destination combination is supported
	switch s := src.(type) {
	case transfer.ImageFetcher:
		switch d := dest.(type) {
		case transfer.ImageStorer:
			return ts.pull(ctx, s, d, topts)
		}
	case transfer.ImageGetter:
		switch d := dest.(type) {
		case transfer.ImagePusher:
			return ts.push(ctx, s, d, topts)
		case transfer.ImageExporter:
			return ts.exportStream(ctx, s, d, topts)
		case transfer.ImageStorer:
			return ts.tag(ctx, s, d, topts)
		}
	case transfer.ImageImporter:
		switch d := dest.(type) {
		case transfer.ImageExportStreamer:
			return ts.echo(ctx, s, d, topts)
		case transfer.ImageStorer:
			// TODO: verify imports with ImageVerifiers?
			return ts.importStream(ctx, s, d, topts)
		}
	}
	return fmt.Errorf("unable to transfer from %s to %s: %w", name(src), name(dest), errdefs.ErrNotImplemented)
}

func name(t any) string {
	switch s := t.(type) {
	case fmt.Stringer:
		return s.String()
	case typeurl.Any:
		return s.GetTypeUrl()
	default:
		return fmt.Sprintf("%T", t)
	}
}

// echo is mostly used for testing, it implements an import->export which is
// a no-op which only roundtrips the bytes.
func (ts *localTransferService) echo(ctx context.Context, i transfer.ImageImporter, e transfer.ImageExportStreamer, tops *transfer.Config) error {
	iis, ok := i.(transfer.ImageImportStreamer)
	if !ok {
		return fmt.Errorf("echo requires access to raw stream: %w", errdefs.ErrNotImplemented)
	}
	r, _, err := iis.ImportStream(ctx)
	if err != nil {
		return err
	}
	wc, _, err := e.ExportStream(ctx)
	if err != nil {
		return err
	}

	// TODO: Use fixed buffer? Send write progress?
	_, err = io.Copy(wc, r)
	if werr := wc.Close(); werr != nil && err == nil {
		err = werr
	}
	return err
}

// WithLease attaches a lease on the context
func (ts *localTransferService) withLease(ctx context.Context, opts ...leases.Opt) (context.Context, func(context.Context) error, error) {
	nop := func(context.Context) error { return nil }

	_, ok := leases.FromContext(ctx)
	if ok {
		return ctx, nop, nil
	}

	ls := ts.config.Leases
	if ls == nil {
		return ctx, nop, nil
	}

	if len(opts) == 0 {
		// Use default lease configuration if no options provided
		opts = []leases.Opt{
			leases.WithRandomID(),
			leases.WithExpiration(24 * time.Hour),
		}
	}

	l, err := ls.Create(ctx, opts...)
	if err != nil {
		return ctx, nop, err
	}

	ctx = leases.WithLease(ctx, l.ID)
	return ctx, func(ctx context.Context) error {
		return ls.Delete(ctx, l)
	}, nil
}

type TransferConfig struct {
	// Leases manager is used to create leases during operations if none, exists
	Leases leases.Manager

	// MaxConcurrentDownloads restricts the total number of concurrent downloads
	// across all layers during an image pull operation. This helps control the
	// overall network bandwidth usage.
	MaxConcurrentDownloads int

	// ConcurrentLayerFetchBuffer sets the maximum size in bytes for each chunk
	// when downloading layers in parallel. Larger chunks reduce coordination
	// overhead but use more memory. When ConcurrentLayerFetchBuffer is above
	// 512 bytes, parallel layer fetch is enabled. It can accelerate pulls for
	// big images.
	ConcurrentLayerFetchBuffer int

	// MaxConcurrentUploadedLayers is the max concurrent uploads for push
	MaxConcurrentUploadedLayers int

	// MaxConcurrentUnpacks controls the number of concurrent unpacks
	MaxConcurrentUnpacks int

	// DuplicationSuppressor is used to make sure that there is only one
	// in-flight fetch request or unpack handler for a given descriptor's
	// digest or chain ID.
	DuplicationSuppressor unpack.KeyedLocker

	// BaseHandlers are a set of handlers which get are called on dispatch.
	// These handlers always get called before any operation specific
	// handlers.
	BaseHandlers []images.Handler

	// UnpackPlatforms are used to specify supported combination of platforms and snapshotters
	UnpackPlatforms []unpack.Platform

	// ImageVerifiers verify the image before saving into the image store.
	Verifiers map[string]imageverifier.ImageVerifier

	// RegistryConfigPath is a path to the root directory containing registry-specific configurations
	RegistryConfigPath string

	// IndexStore is the optional indexed content store used for lazy EROFS
	// layer ingest. When set and an UnpackConfiguration has LazyEROFS=true,
	// EROFS layers carrying org.erofs.chunk-index.range are ingested lazily
	// (only the chunk-index section is downloaded) via this store.
	IndexStore lazyIndexStore

	// CacheWarmer, when non-nil, is invoked after each successful lazy
	// ingest so the sparse cache file begins streaming in the background.
	// Without a warmer the chunks would not be fetched until the container
	// is run (the block-mount handler's eager EnsureAll), serialising
	// network IO on the run path.  Optional: when nil, lazy pull still
	// works but the run is delayed by the time it takes to fetch and
	// decompress every chunk.
	CacheWarmer LazyCacheWarmer
}
