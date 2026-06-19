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
// The collector implements the metadata.collectionWithReferences interface via
// References.  When the core GC traversal reaches a reachable
// ResourceContentIndex node, References emits a forward reference to each
// content-store entry the blob owns:
//
//   - The chunk-index entry                    (indexed-content/blobs/*blob*/index)
//   - Every per-chunk entry                    (indexed-content/blobs/*blob*/chunks/*)
//   - Every non-inline extra entry             (indexed-content/blobs/*blob*/extras/*/digest)
//
// Those ResourceContent nodes are thereby kept alive as long as the blob is
// reachable.  Emitting the edges on demand (rather than pre-computing them all
// up-front) keeps GC memory bounded for blobs with many chunks.  Inline extras
// have no content-store entry to pin.
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

// Active is a no-op for indexed-content blobs: they are never their own GC
// roots.  Reachability comes exclusively from forward-reference labels of
// the form "containerd.io/gc.ref.content-index.<name>=<digest>" written by
// core/images.SetChildrenMappedLabels on the manifest that owns the layer.
// The core metadata GC scanner resolves those labels to ResourceContentIndex
// nodes, transitively pinning the indexed-content blob (and the per-chunk
// ResourceContent nodes the blob owns via References).
func (c *collection) Active(_ string, _ func(gc.Node)) {}

// References satisfies the metadata.collectionWithReferences interface.
//
// When the GC visits a reachable ResourceContentIndex node (reached because a
// core object carries a containerd.io/gc.ref.content-index label pointing at
// the blob digest), this method emits a forward reference to every
// content-store entry the blob owns:
//
//   - The chunk-index entry (keyed by IndexDigest)
//   - Each per-chunk entry (keyed by per-chunk hash)
//   - Each non-inline extra entry (keyed by its compressed-bytes digest)
//
// Emitting these edges on demand — rather than pre-computing all of them in
// ActiveWithBackRefs and holding them in the gcContext.backRefs map for the
// whole pass — keeps GC memory bounded even for blobs with thousands of
// chunks.  Inline extras are omitted because they have no content-store entry
// to pin.
//
// The lookup uses the index store's own read transaction (c.tx) opened in
// StartCollection, so the metadata GC interface stays free of any bolt types.
func (c *collection) References(_ context.Context, node gc.Node, fn func(gc.Node)) {
	if node.Type != metadata.ResourceContentIndex {
		return
	}
	bb := getBlobsBucket(c.tx, node.Namespace) // v1/<ns>/indexed-content/blobs
	if bb == nil {
		return
	}
	blobBkt := bb.Bucket([]byte(node.Key))
	if blobBkt == nil {
		// Node may be created from a dead edge; nothing to reference.
		return
	}

	emit := func(d digest.Digest) {
		fn(gc.Node{
			Type:      metadata.ResourceContent,
			Namespace: node.Namespace,
			Key:       d.String(),
		})
	}

	// ── chunk-index entry ────────────────────────────────────────────
	if iv := blobBkt.Get(bucketKeyIndex); len(iv) > 0 {
		if d, err := digest.Parse(string(iv)); err == nil {
			emit(d)
		}
	}

	// ── per-chunk entries ────────────────────────────────────────────
	if chunksBkt := getChunksBucket(blobBkt); chunksBkt != nil {
		_ = chunksBkt.ForEach(func(_, cv []byte) error {
			if d, err := digest.Parse(string(cv)); err == nil {
				emit(d)
			}
			return nil
		})
	}

	// ── non-inline extra entries ──────────────────────────────────────
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
					emit(d)
				}
			}
			return nil
		})
	}
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
//
// After the index metadata for a removed blob is deleted, the blob digest is
// a candidate for cache cleanup.  Because the sparse-file cache is addressed
// purely by digest (and may be shared by the same blob ingested into several
// namespaces), the on-disk cache is removed only once the digest is absent
// from every namespace's blobs bucket.  The actual file removal happens after
// the metadata write transaction commits, via the registered blobRemover.
func (c *collection) Finish() error {
	c.releaseTx()
	if len(c.removed) == 0 {
		return nil
	}

	// Digests whose index metadata was deleted this pass and which are no
	// longer present in any namespace — candidates for cache removal.
	orphaned := map[digest.Digest]struct{}{}
	// Provider names recorded on the deleted blobs, keyed by digest, so a
	// fully-orphaned blob's provider reconstruction record can be reaped too.
	providerNames := map[digest.Digest]string{}

	if err := update(c.ctx, c.store.db, func(tx *bolt.Tx) error {
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
			blob := bb.Bucket([]byte(dgst))
			if blob == nil {
				continue
			}
			if name := blob.Get(bucketKeyProvider); len(name) > 0 {
				providerNames[dgst] = string(name)
			}
			if err := bb.DeleteBucket([]byte(dgst)); err != nil {
				return fmt.Errorf("content/index: gc remove %s/%s: %w",
					n.Namespace, n.Key, err)
			}
			orphaned[dgst] = struct{}{}
		}
		// Drop any candidate still referenced in another namespace.
		for dgst := range orphaned {
			if blobExistsAnyNamespace(tx, dgst) {
				delete(orphaned, dgst)
			}
		}
		// Reap provider reconstruction records for fully-orphaned blobs.
		for dgst := range orphaned {
			if name := providerNames[dgst]; name != "" {
				if err := deleteProvider(tx, name); err != nil {
					return fmt.Errorf("content/index: gc remove provider %s: %w", name, err)
				}
			}
		}
		return nil
	}); err != nil {
		return err
	}

	// Reclaim on-disk cache for blobs now absent from every namespace.
	// Done outside the metadata transaction so a slow file delete does not
	// hold the write lock, and so a removal failure cannot roll back the
	// (already durable) metadata deletion.
	if c.store.blobRemover != nil {
		for dgst := range orphaned {
			if err := c.store.blobRemover(dgst); err != nil {
				// Non-fatal: the metadata is gone, so the cache is orphaned
				// and will be retried on a subsequent sweep.  Log via the
				// store's context is not available here; swallow and move on.
				_ = err
			}
		}
	}
	return nil
}

// blobExistsAnyNamespace reports whether an indexed blob with the given digest
// is present in any namespace's blobs bucket.  Used to decide whether a cache
// blob is safe to remove after one namespace's reference is collected.
func blobExistsAnyNamespace(tx *bolt.Tx, dgst digest.Digest) bool {
	v := tx.Bucket(bucketKeyVersion)
	if v == nil {
		return false
	}
	found := false
	_ = v.ForEach(func(nsKey, val []byte) error {
		if found || val != nil || string(nsKey) == string(bucketKeyIndexedContent) {
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
		if bb.Bucket([]byte(dgst)) != nil {
			found = true
		}
		return nil
	})
	return found
}
