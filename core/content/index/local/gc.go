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
	"sync"

	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/core/metadata"
	"github.com/containerd/containerd/v2/pkg/gc"
	"github.com/opencontainers/go-digest"
	bolt "go.etcd.io/bbolt"
)

// Collector returns a metadata.Collector that integrates the indexed content
// store into containerd's label-based garbage collector.
//
// The collector is registered against the
// "containerd.io/gc.ref.content.index[.<name>]" label namespace (see
// contentindex.GCRefLabel).  Any core metadata object carrying such a label
// pins the named blob — and, transitively, every content-store entry the blob
// references — for the lifetime of the carrying object.
//
// # How chunk and extra content-store entries are kept alive
//
// The collector implements the metadata.collectionWithBackRefs interface via
// ActiveWithBackRefs. For every active indexed-content blob the method emits
// back-reference edges from the blob node to each content-store entry it owns:
//
//   - The chunk-index entry                    (indexed-content/blobs/*blob*/index)
//   - Every per-chunk entry                    (indexed-content/blobs/*blob*/chunks/*)
//   - Every non-inline extra entry             (indexed-content/blobs/*blob*/extras/*/digest)
//
// When the core GC traversal reaches a reachable ResourceContentIndex node, it
// follows these back-references to the corresponding ResourceContent nodes,
// keeping those content-store entries alive as long as the blob is reachable.
// Inline extras have no content-store entry to pin.
//
// # Lifecycle
//
// When containerd's GC marks a blob unreachable (Remove is called), the blob's
// metadata record is deleted in Finish().  The chunks it referenced become
// unreferenced in the content store and are collected by the next
// content-store GC pass, subject to whether any other indexed-content blob
// still lists the same digest.
func (s *Store) Collector() metadata.Collector {
	return &collector{store: s}
}

type collector struct {
	store *Store
}

func (c *collector) ReferenceLabel() string {
	return contentindex.GCRefLabel
}

// StartCollection opens a stable read view of the indexed-content metadata
// buckets for the duration of the GC pass.
//
// Because the Transactor interface exposes only View/Update callbacks (not a
// raw Begin), the read transaction is held open by a background goroutine that
// blocks inside a db.View call.  The goroutine is unblocked when Cancel or
// Finish signals via the txDone channel.
func (c *collector) StartCollection(ctx context.Context) (metadata.CollectionContext, error) {
	type openResult struct {
		coll *collection
		err  error
	}
	ch := make(chan openResult, 1)

	coll := &collection{
		store:  c.store,
		ctx:    ctx,
		txDone: make(chan struct{}),
	}

	go func() {
		viewErr := c.store.db.View(func(tx *bolt.Tx) error {
			coll.tx = tx
			ch <- openResult{coll: coll}
			<-coll.txDone // block until Cancel/Finish releases us
			return nil
		})
		// db.View failed before the callback could send to ch.
		if viewErr != nil {
			select {
			case ch <- openResult{err: viewErr}:
			default:
			}
		}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("content/index: open gc view tx: %w", r.err)
		}
		return r.coll, nil
	case <-ctx.Done():
		coll.releaseTx()
		return nil, ctx.Err()
	}
}

// collection implements metadata.CollectionContext (and the unexported
// metadata.collectionWithBackRefs interface) for a single GC pass.
//
// The read transaction opened in StartCollection is used by All/ActiveWithBackRefs.
// Removals are collected in memory; actual deletes happen in Finish under a
// fresh write transaction so that Cancel can abandon cheaply.
type collection struct {
	store     *Store
	ctx       context.Context // stored from StartCollection
	tx        *bolt.Tx        // read-only; held for the GC pass duration
	txDone    chan struct{}
	closeOnce sync.Once
	removed   []gc.Node
}

// releaseTx signals the goroutine holding the view transaction to return,
// releasing the read lock.  Safe to call more than once.
func (c *collection) releaseTx() {
	c.closeOnce.Do(func() { close(c.txDone) })
}

// All enumerates every indexed-content blob in every namespace so the core GC
// can determine which are reachable via annotations and which are orphaned.
//
// It walks v1/<ns>/indexed-content/blobs, skipping any non-namespace entry
// at the v1 level (e.g. the "indexed-content" config bucket or the "version"
// plain-key entry written by core/metadata).
func (c *collection) All(fn func(gc.Node)) {
	v := c.tx.Bucket(bucketKeyVersion)
	if v == nil {
		return
	}
	_ = v.ForEach(func(nsKey, val []byte) error {
		// Skip plain k/v entries (e.g. core/metadata's "version" key)
		// and the "indexed-content" config bucket itself.
		if val != nil || string(nsKey) == string(bucketKeyIndexedContent) {
			return nil
		}
		nb := v.Bucket(nsKey)
		if nb == nil {
			return nil
		}
		icBkt := nb.Bucket(bucketKeyIndexedContent)
		if icBkt == nil {
			return nil
		}
		bb := icBkt.Bucket(bucketKeyObjectBlobs)
		if bb == nil {
			return nil
		}
		ns := string(nsKey)
		return bb.ForEach(func(dgstKey, dgstVal []byte) error {
			if dgstVal != nil {
				return nil // skip plain k/v inside blobs
			}
			fn(gc.Node{
				Type:      metadata.ResourceContentIndex,
				Namespace: ns,
				Key:       string(dgstKey),
			})
			return nil
		})
	})
}

// Active returns no blobs as GC roots.  Indexed-content blobs are not
// self-rooting; they survive only if a core object's
// containerd.io/gc.ref.content.index label pins them.  See ActiveWithBackRefs
// for the back-reference registration that makes chunks reachable when the
// blob itself is reachable via that label.
func (c *collection) Active(_ string, _ func(gc.Node)) {}

// ActiveWithBackRefs satisfies the metadata.collectionWithBackRefs interface.
//
// Indexed-content blobs are NOT self-rooting: fn is never called here, so
// blobs are only kept alive when a core containerd object carries a
// containerd.io/gc.ref.content.index label that points at the blob digest.
// The fn parameter is intentionally ignored.
//
// What this method DOES do is register back-reference edges from every blob
// node to each of its constituent content-store entries (chunk-index entry,
// per-chunk entries, non-inline extra entries) via the bref callback.  When
// the GC Tricolor algorithm later visits a blob node (because some reachable
// core object referenced it via the label mechanism), it consults
// c.backRefs[blobNode] and finds the chunk nodes, which are then themselves
// marked reachable.
//
// For each active blob it calls bref(blobNode, contentNode) for every
// content-store entry the blob owns:
//
//   - The chunk-index entry (keyed by IndexDigest)
//   - Each per-chunk entry (keyed by per-chunk hash)
//   - Each non-inline extra entry (keyed by its compressed-bytes digest)
//
// The core GC uses these back-reference edges to keep the associated
// ResourceContent nodes alive whenever the ResourceContentIndex blob is
// reachable.  Inline extras are omitted because they have no content-store
// entry to pin.
func (c *collection) ActiveWithBackRefs(ns string, _ func(gc.Node), bref func(gc.Node, gc.Node)) {
	bb := getBlobsBucket(c.tx, ns) // v1/<ns>/indexed-content/blobs
	if bb == nil {
		return
	}
	_ = bb.ForEach(func(k, v []byte) error {
		if v != nil {
			return nil
		}
		blobBkt := bb.Bucket(k)
		if blobBkt == nil {
			return nil
		}
		blobNode := gc.Node{
			Type:      metadata.ResourceContentIndex,
			Namespace: ns,
			Key:       string(k),
		}
		// Do NOT call fn(blobNode): indexed-content blobs are not self-rooting.

		// ── chunk-index entry ────────────────────────────────────────
		if iv := blobBkt.Get(bucketKeyIndex); len(iv) > 0 {
			if d, err := digest.Parse(string(iv)); err == nil {
				bref(blobNode, gc.Node{
					Type:      metadata.ResourceContent,
					Namespace: ns,
					Key:       d.String(),
				})
			}
		}

		// ── per-chunk entries ────────────────────────────────────────
		if chunksBkt := getChunksBucket(blobBkt); chunksBkt != nil {
			_ = chunksBkt.ForEach(func(_, cv []byte) error {
				if d, err := digest.Parse(string(cv)); err == nil {
					bref(blobNode, gc.Node{
						Type:      metadata.ResourceContent,
						Namespace: ns,
						Key:       d.String(),
					})
				}
				return nil
			})
		}

		// ── non-inline extra entries ─────────────────────────────────
		if extrasBkt := getExtrasBucket(blobBkt); extrasBkt != nil {
			_ = extrasBkt.ForEach(func(ek, ev []byte) error {
				if ev != nil {
					return nil // skip plain k/v
				}
				exBkt := extrasBkt.Bucket(ek)
				if exBkt == nil {
					return nil
				}
				if dv := exBkt.Get(bucketKeyDigest); len(dv) > 0 {
					if d, err := digest.Parse(string(dv)); err == nil {
						bref(blobNode, gc.Node{
							Type:      metadata.ResourceContent,
							Namespace: ns,
							Key:       d.String(),
						})
					}
				}
				return nil
			})
		}
		return nil
	})
}

// Leased is unused in v1: indexed-content blobs are pinned via labels on core
// objects, and core leases pin blobs transitively through those labels.
func (c *collection) Leased(ns, lease string, fn func(gc.Node)) {}

func (c *collection) Remove(n gc.Node) {
	c.removed = append(c.removed, n)
}

// Cancel releases the read transaction without applying any removals.
func (c *collection) Cancel() error {
	c.releaseTx()
	return nil
}

// Finish releases the read transaction and then deletes any blobs queued by
// Remove in a separate write transaction.  The read tx is released first so
// the write transaction does not contend with it.
func (c *collection) Finish() error {
	c.releaseTx()
	if len(c.removed) == 0 {
		return nil
	}
	return update(c.ctx, c.store.db, func(tx *bolt.Tx) error {
		for _, n := range c.removed {
			if n.Type != metadata.ResourceContentIndex {
				continue
			}
			dgst, err := digest.Parse(n.Key)
			if err != nil {
				continue
			}
			bb := getBlobsBucket(tx, n.Namespace)
			if bb == nil {
				continue
			}
			if bb.Bucket([]byte(dgst)) == nil {
				continue
			}
			if err := bb.DeleteBucket([]byte(dgst)); err != nil {
				return fmt.Errorf("content/index: gc remove %s/%s: %w",
					n.Namespace, n.Key, err)
			}
		}
		return nil
	})
}
