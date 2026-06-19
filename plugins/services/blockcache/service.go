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

// Package blockcache implements the BlockCache ttrpc service.
//
// The service is consumed by shims that handle "block" mount types.  When a
// shim mounts a sparse block file (fill=sparse option) it opens a Fill stream
// to request byte-range fills.  The service uses the daemon's cache.Cache and
// indexed content store to fill the sparse backing file on demand and reports
// filled ranges back over the stream.
//
// The ttrpc server is already active (the events forwarder shares it); this
// plugin registers an additional service on the same server.
package blockcache

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/containerd/containerd/api/services/blockcache/v1"
	"github.com/containerd/containerd/v2/core/content"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/core/content/index/cache"
	"github.com/containerd/containerd/v2/core/content/index/provider"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"
	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: plugins.TTRPCPlugin,
		ID:   "blockcache",
		Requires: []plugin.Type{
			plugins.ContentPlugin,
			plugins.ContentIndexPlugin,
			plugins.CachePlugin,
		},
		InitFn: func(ic *plugin.InitContext) (any, error) {
			cs, err := ic.GetSingle(plugins.ContentPlugin)
			if err != nil {
				return nil, fmt.Errorf("blockcache service: get content store: %w", err)
			}
			contentStore, ok := cs.(content.Store)
			if !ok {
				return nil, fmt.Errorf("blockcache service: content plugin is not a content.Store")
			}

			idx, err := ic.GetSingle(plugins.ContentIndexPlugin)
			if err != nil {
				return nil, fmt.Errorf("blockcache service: get content index plugin: %w", err)
			}
			indexStore, ok := idx.(contentindex.Store)
			if !ok {
				return nil, fmt.Errorf("blockcache service: index plugin is not a contentindex.Store")
			}

			cv, err := ic.GetSingle(plugins.CachePlugin)
			if err != nil {
				return nil, fmt.Errorf("blockcache service: get cache: %w", err)
			}
			c, ok := cv.(cache.Cache)
			if !ok {
				return nil, fmt.Errorf("blockcache service: cache plugin is not a cache.Cache")
			}

			return &service{
				indexStore:   indexStore,
				contentStore: contentStore,
				cache:        c,
			}, nil
		},
	})
}

type service struct {
	indexStore   contentindex.Store
	contentStore content.Store
	cache        cache.Cache
}

func (s *service) RegisterTTRPC(srv *ttrpc.Server) error {
	blockcache.RegisterTTRPCBlockCacheService(srv, s)
	return nil
}

// Fill handles the bidirectional Fill stream from a shim.
func (s *service) Fill(ctx context.Context, stream blockcache.TTRPCBlockCache_FillServer) error {
	// First message must be Hello.
	first, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("blockcache: waiting for Hello: %w", err)
	}
	hello := first.Hello
	if hello == nil {
		return fmt.Errorf("blockcache: first message must be Hello (got fill=%v filled=%v error=%v)", first.Fill, first.Filled, first.Error)
	}
	blockid := hello.GetBlockid()
	log.G(ctx).WithField("blockid", blockid).Debug("blockcache: Fill stream opened")

	// Resolve the block in the indexed content store.
	dgst, err := godigest.Parse(blockid)
	if err != nil {
		return fmt.Errorf("blockcache: parse blockid %q: %w", blockid, err)
	}

	info, err := s.indexStore.Info(ctx, dgst)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return fmt.Errorf("blockcache: block %s not found in index store: %w", blockid, errdefs.ErrNotFound)
		}
		return fmt.Errorf("blockcache: lookup block %s: %w", blockid, err)
	}

	// Get the byte provider for this block from the global registry.
	p, err := provider.Global.Get(info.Provider)
	if err != nil {
		return fmt.Errorf("blockcache: get provider %q for block %s: %w", info.Provider, blockid, err)
	}

	desc := ocispec.Descriptor{
		MediaType: info.MediaType,
		Digest:    info.Digest,
		Size:      info.Size,
	}

	// Attach the cache.  This increments the refcount; released on stream close.
	handle, err := s.cache.Attach(ctx, desc, p)
	if err != nil {
		return fmt.Errorf("blockcache: cache attach %s: %w", blockid, err)
	}
	defer func() {
		if rerr := handle.Release(); rerr != nil {
			log.G(ctx).WithError(rerr).WithField("blockid", blockid).Warn("blockcache: cache handle release")
		}
	}()

	// Send initial Filled covering any already-resident ranges.
	if filled := s.residentRanges(ctx, handle); len(filled) > 0 {
		if err := stream.Send(&blockcache.FillMessage{
			Filled: &blockcache.Filled{Ranges: filled},
		}); err != nil {
			return err
		}
	}

	// Service fill requests.
	for {
		msg, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
				log.G(ctx).WithField("blockid", blockid).Debug("blockcache: Fill stream closed")
				return nil
			}
			return fmt.Errorf("blockcache: recv: %w", err)
		}

		req := msg.Fill
		if req == nil {
			log.G(ctx).WithField("blockid", blockid).Warnf("blockcache: unexpected message type on Fill stream (hello=%v filled=%v error=%v)", msg.Hello, msg.Filled, msg.Error)
			continue
		}

		fillErr := handle.EnsureRange(ctx, req.GetOffset(), req.GetLength())
		if fillErr != nil {
			log.G(ctx).WithError(fillErr).WithField("blockid", blockid).
				WithField("offset", req.GetOffset()).WithField("length", req.GetLength()).
				Error("blockcache: EnsureRange failed")
			// Report the error to the shim so it can surface an I/O error
			// to the container's blocked read rather than retrying forever.
			if serr := stream.Send(&blockcache.FillMessage{
				Error: &blockcache.FillError{Message: fillErr.Error()},
			}); serr != nil {
				return serr
			}
			continue
		}

		// Compute which ranges are now resident and send them.
		filled := s.rangesCoveredBy(handle, req.GetOffset(), req.GetLength())
		if err := stream.Send(&blockcache.FillMessage{
			Filled: &blockcache.Filled{Ranges: filled},
		}); err != nil {
			return err
		}
	}
}

// residentRanges returns ByteRange entries for all chunks already resident
// in the cache.  Called once at stream open so the shim can seed its bitmap
// without sending redundant Fill requests for already-filled pages.
func (s *service) residentRanges(_ context.Context, h cache.Handle) []*blockcache.ByteRange {
	ranges := h.ResidentRanges()
	if len(ranges) == 0 {
		return nil
	}
	out := make([]*blockcache.ByteRange, len(ranges))
	for i, r := range ranges {
		out[i] = &blockcache.ByteRange{
			Offset: r.Start,
			Length: r.End - r.Start,
		}
	}
	return out
}

// rangesCoveredBy returns the ByteRanges that are resident in the cache
// as a result of an EnsureRange call covering [off, off+length).
// In v1, we conservatively report just the requested range rounded outward
// to the containing chunk boundaries.  The shim ORs these into its bitmap.
func (s *service) rangesCoveredBy(h cache.Handle, off, length int64) []*blockcache.ByteRange {
	// The simplest correct answer: report a range that covers [off, off+length)
	// and is guaranteed to be resident (EnsureRange just filled it).
	return []*blockcache.ByteRange{{Offset: off, Length: length}}
}
