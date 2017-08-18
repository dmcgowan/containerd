package metadata

import (
	"context"
	"sync"
	"time"

	"github.com/boltdb/bolt"
	"github.com/containerd/containerd/gc"
	"github.com/containerd/containerd/metadata"
	"github.com/containerd/containerd/plugin"
	"github.com/pkg/errors"
)

func init() {
	plugin.Register(&plugin.Registration{
		Type: plugin.GCPlugin,
		ID:   "metadata",
		Requires: []plugin.PluginType{
			plugin.MetadataPlugin,
		},
		Init: func(ic *plugin.InitContext) (interface{}, error) {
			md, err := ic.Get(plugin.MetadataPlugin)
			if err != nil {
				return nil, err
			}

			return NewMetadataCollector(md.(*bolt.DB))
		},
	})
}

//// GarbageCollector interface is used to synchronize global resource access
//// with the garbage collector.
//type GarbageCollector interface {
//	// Rlock acquires a global read lock. This lock will block
//	// while the garbage collector is cleaning data.
//	RLock(context.Context)
//
//	// Runlock releases a read lock
//	RUnlock()
//
//	// MLock acquires a global lock for making mutations.
//	// Mlock is not an exclusive lock but cannot be taken
//	// while the garbage collector is running.
//	MLock(context.Context)
//
//	// Munlock releases a mutation lock
//	MUnlock()
//
//	// Trigger immediately runs garbage collection. If garbage
//	// collection is in progress it will block until it is finished.
//	Trigger(context.Context) error
//}

type metadataCollector struct {
	rlock sync.RWMutex
	mlock sync.RWMutex

	meta *bolt.DB

	// Keep node to data source map in memory for looking up references

	// TODO: Keep track of stats such as pause time, number of collected objects, errors
	lastCollection time.Time
	mutationsSince int
}

func NewMetadataCollector(db *bolt.DB) (gc.GarbageCollector, error) {
	return &metadataCollector{
		meta: db,
	}, nil
}

func (mc *metadataCollector) RLock(context.Context) {
	mc.rlock.RLock()
}

func (mc *metadataCollector) RUnlock() {
	mc.rlock.RUnlock()
}

func (mc *metadataCollector) MLock(context.Context) {
	mc.mlock.RLock()
}

func (mc *metadataCollector) MUnlock() {
	mc.mutationsSince++
	mc.mlock.RUnlock()
}

func (mc *metadataCollector) Trigger(ctx context.Context) error {
	// Track lock time
	mc.mlock.Lock()
	defer mc.mlock.Unlock()

	// TODO: Track errors

	var marked map[gc.Node]struct{}

	if err := mc.meta.View(func(tx *bolt.Tx) error {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		roots := make(chan gc.Node)
		errChan := make(chan error)
		go func() {
			defer close(errChan)
			defer close(roots)

			// Call roots
			if err := metadata.ScanRoots(ctx, tx, roots); err != nil {
				cancel()
				errChan <- err
			}
		}()

		refs := func(ctx context.Context, n gc.Node, fn func(gc.Node)) error {
			return metadata.References(ctx, tx, n, fn)
		}

		reachable, err := gc.ConcurrentMark(ctx, roots, refs)
		if rerr := <-errChan; rerr != nil {
			return rerr
		}
		if err != nil {
			return err
		}
		marked = reachable
		return nil
	}); err != nil {
		return err
	}

	// Track lock time
	mc.rlock.Lock()
	defer mc.rlock.Unlock()

	if err := mc.meta.Update(func(tx *bolt.Tx) error {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		nodeC := make(chan gc.Node)
		var scanErr error

		go func() {
			defer close(nodeC)
			scanErr = metadata.ScanAll(ctx, tx, nodeC)
		}()

		rm := func(n gc.Node) error {
			return metadata.Remove(ctx, tx, n)
		}

		if err := gc.Sweep(marked, nodeC, rm); err != nil {
			return errors.Wrap(err, "failed to sweep")
		}

		if scanErr != nil {
			return errors.Wrap(scanErr, "failed to scan all")
		}

		return nil
	}); err != nil {
		return err
	}

	mc.mutationsSince = 0
	mc.lastCollection = time.Now()

	return nil
}
