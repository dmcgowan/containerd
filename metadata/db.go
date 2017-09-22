package metadata

import (
	"context"
	"sync"
	"time"

	"github.com/boltdb/bolt"
	"github.com/containerd/containerd/gc"
	"github.com/pkg/errors"
)

type DB struct {
	db *bolt.DB

	rlock sync.RWMutex
	mlock sync.RWMutex

	// TODO: Track sub content and snapshot stores to collect without full lock

	// TODO: Keep track of stats such as pause time, number of collected objects, errors
	lastCollection time.Time
	mutationsSince int
}

func NewDB(db *bolt.DB) *DB {
	return &DB{
		db: db,
	}
}

func (m *DB) View(fn func(*bolt.Tx) error) error {
	m.rlock.RLock()
	defer m.rlock.RUnlock()
	return m.db.View(fn)
}

func (m *DB) Update(fn func(*bolt.Tx) error) error {
	m.mlock.RLock()
	defer m.mlock.RUnlock()
	return m.db.Update(fn)
}

func (m *DB) GarbageCollect(ctx context.Context) error {
	// Track lock time
	m.mlock.Lock()
	defer m.mlock.Unlock()

	// TODO: Track errors

	var marked map[gc.Node]struct{}

	if err := m.db.View(func(tx *bolt.Tx) error {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		roots := make(chan gc.Node)
		errChan := make(chan error)
		go func() {
			defer close(errChan)
			defer close(roots)

			// Call roots
			if err := ScanRoots(ctx, tx, roots); err != nil {
				cancel()
				errChan <- err
			}
		}()

		refs := func(ctx context.Context, n gc.Node, fn func(gc.Node)) error {
			return References(ctx, tx, n, fn)
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

	if err := m.db.Update(func(tx *bolt.Tx) error {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		nodeC := make(chan gc.Node)
		var scanErr error

		go func() {
			defer close(nodeC)
			scanErr = ScanAll(ctx, tx, nodeC)
		}()

		rm := func(n gc.Node) error {
			return Remove(ctx, tx, n)
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
