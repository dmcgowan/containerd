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

// pull_lazy_linux.go implements lazy-layer routing for Linux.
//
// Currently the only supported lazy-layer format is EROFS with an embedded
// chunk-index (org.erofs.chunk-index.range annotation).  The routing is
// compiled on Linux only because:
//
//   - The EROFS snapshotter and its kernel filesystem are Linux-only.
//   - Loop devices (used to mount the filled cache file) are Linux-only.
//
// On non-Linux platforms lazyLayerHandler (pull_lazy_other.go) returns nil,
// and the caller falls back to normal eager fetching.

package local

import (
	"context"
	"fmt"

	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/containerd/containerd/v2/core/content"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/core/content/index/provider"
	"github.com/containerd/containerd/v2/core/content/index/registry"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/pkg/namespaces"
)

// lazyLayerHandler returns an images.Handler that routes eligible layers
// through lazy ingest, falling back to eager fetch for non-eligible content.
//
// Eligible layers: EROFS media type + org.erofs.chunk-index.range annotation.
// For those layers only the chunk-index section is downloaded at pull time;
// chunk bytes are fetched on demand when the container first reads them.
//
// Returns nil on non-Linux builds (see pull_lazy_other.go).
func lazyLayerHandler(
	ingester content.Ingester,
	fetcher remotes.Fetcher,
	indexStore lazyIndexStore,
	warmer LazyCacheWarmer,
	imageRef string,
	cp credentialProvider,
	pt *ProgressTracker,
) images.Handler {
	return images.HandlerFunc(func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		if !lazyEligible(desc) {
			// Not a lazy-eligible layer — fall through to normal fetch.
			return fetchHandler(ingester, fetcher, pt)(ctx, desc)
		}

		// Build a provider for this pull and register it globally.
		// The name includes the digest so concurrent pulls of different blobs
		// don't collide in the global registry.
		providerName := fmt.Sprintf("registry:%s", desc.Digest)
		p := registry.New(fetcher, providerName, registry.Config{})
		provider.Global.Register(p)

		// Lazy-ingest: download only the chunk-index section.
		ref := fmt.Sprintf("lazy-%s", desc.Digest)
		if err := indexStore.WriteLazy(ctx, ref, desc, p); err != nil {
			if errdefs.IsAlreadyExists(err) {
				if pt != nil {
					pt.MarkExists(desc)
				}
				return nil, nil
			}
			return nil, fmt.Errorf("lazy ingest %s: %w", desc.Digest, err)
		}

		// Persist the provider's reconstruction metadata (ref + credential)
		// into the index store's bolt DB so chunk filling can survive a daemon
		// restart.  The credential is extracted from the resolver, JSON-
		// serialised, and sealed (AES-256-GCM) by the store before any bytes
		// are written to disk.  The sealed blob is temporary: it is purged
		// once the blob is fully filled (all chunks in the cache).
		if pp, ok := indexStore.(providerPersister); ok && imageRef != "" {
			var credJSON []byte
			if cp != nil {
				host := registryHostFromRef(imageRef)
				if cj, err := cp.RegistryCredentialJSON(ctx, host); err != nil {
					log.G(ctx).WithError(err).WithField("provider", providerName).
						Warn("failed to extract registry credential for provider persistence")
				} else {
					credJSON = cj
				}
			}
			if err := pp.PutProvider(ctx, providerName, imageRef, credJSON); err != nil {
				log.G(ctx).WithError(err).WithField("provider", providerName).
					Warn("failed to persist registry provider metadata")
			}
		}

		log.G(ctx).WithFields(log.Fields{
			"digest":   desc.Digest,
			"provider": providerName,
		}).Debug("lazy layer ingested (chunk-index only)")

		// We deliberately DO NOT call warmer.Warm() at pull time.
		//
		// Pull-time Warm() used to spawn a non-cancellable
		// PriorityBackground WarmAll goroutine that ran at
		// concurrency=4 against the cache (`bgCtx` was a detached
		// context).  For a typical EROFS layer (hundreds of chunks
		// at ~500 ms each) it filled the entire image in ~150 s.
		// If the user ran `ctr image pull --lazy IMG` and then
		// `ctr run IMG` more than ~2 minutes later — or even
		// less for smaller images — `handle.AllPresent()` would
		// already be true at mount time.  The block-mount handler
		// then took the EAGER branch
		// (`plugins/mount/block/handler_linux.go::Mount` checks
		// `!handle.AllPresent()` to enter the lazy path),
		// installing NO fanotify supervisor and using a plain
		// `unix.Mount` — i.e. lazy loading was silently disabled
		// by a too-eager background warmer.
		//
		// The supervisor-time warmer (started inside
		// `plugins/mount/block/supervisor_linux.go::newDaemonSupervisor`)
		// covers the same warm-the-cold-tail job, but only runs
		// while fanotify supervision is actually live.  That
		// guarantees the warmer never outruns the workload's
		// first `ctr run` and the lazy path is always taken
		// when the kernel supports FAN_CLASS_PRE_CONTENT.
		//
		// We still PrepareForFSView at pull time: that warms just
		// the SB + inode-table region (a few MiB) so the
		// no-mount fsview/block spec-build path
		// (`plugins/mount/fsview/block/`) works on the first
		// `ctr run`.  This is far too little data to push the
		// cache to AllPresent.
		if warmer != nil {
			prepareCtx := warmerDetachedCtx(ctx)
			go func() {
				if err := warmer.PrepareForFSView(prepareCtx, desc, p); err != nil {
					log.G(prepareCtx).WithError(err).WithField("digest", desc.Digest).
						Warn("cache prepare-for-fsview on pull failed; spec-build will fall back to kernel mount")
				}
			}()
		}
		return nil, nil
	})
}

// warmerDetachedCtx returns a context derived from context.Background
// that preserves the namespace from `parent`.  Used for fire-and-forget
// background work spawned from short-lived RPC contexts (e.g. the pull
// path's PrepareForFSView).
func warmerDetachedCtx(parent context.Context) context.Context {
	if ns, ok := namespaces.Namespace(parent); ok {
		return namespaces.WithNamespace(context.Background(), ns)
	}
	return context.Background()
}

// lazyEligible returns true if desc is a layer that supports lazy ingest.
// Currently: EROFS media type + org.erofs.chunk-index.range annotation.
func lazyEligible(desc ocispec.Descriptor) bool {
	if desc.Annotations == nil {
		return false
	}
	if desc.Annotations[contentindex.AnnotationChunkIndexRange] == "" {
		return false
	}
	mt := desc.MediaType
	return mt == contentindex.MediaTypeEROFS ||
		mt == contentindex.MediaTypeEROFSZstd ||
		mt == contentindex.MediaTypeEROFSLayer ||
		mt == contentindex.MediaTypeEROFSLayerZstd
}
