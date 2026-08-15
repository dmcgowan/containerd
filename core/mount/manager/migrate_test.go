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

package manager

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/containerd/containerd/v2/core/metadata"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/gc"
	"github.com/containerd/containerd/v2/pkg/namespaces"

	bolt "go.etcd.io/bbolt"
)

// legacyActive is one position in a legacy activation's mount chain, in
// the shape the schema this package replaced recorded it: type and
// mount point only, since source, target and options were never
// implemented there.
type legacyActive struct {
	typ string
	mp  string
	at  time.Time
}

// legacyActivation describes one activation to seed under the "v1"
// bucket name. A nil active slice produces an activation with no
// active bucket at all, matching one interrupted before completion.
type legacyActivation struct {
	name   string
	lease  string
	labels map[string]string
	active []legacyActive
}

// seedLegacyV1 writes activations directly under the bucket name of
// the schema this package replaced, exactly as a binary which predates
// the current schema would have. It exists only to build fixtures for
// migration tests: production code never writes this bucket.
func seedLegacyV1(t *testing.T, db *bolt.DB, namespace string, activations ...legacyActivation) {
	t.Helper()
	require.NoError(t, db.Update(func(tx *bolt.Tx) error {
		oldbkt, err := tx.CreateBucketIfNotExists([]byte("v1"))
		if err != nil {
			return err
		}
		nsbkt, err := oldbkt.CreateBucketIfNotExists([]byte(namespace))
		if err != nil {
			return err
		}
		mbkt, err := nsbkt.CreateBucketIfNotExists(bucketKeyMounts)
		if err != nil {
			return err
		}

		var lsbkt *bolt.Bucket
		for i, a := range activations {
			bkt, err := mbkt.CreateBucket([]byte(a.name))
			if err != nil {
				return err
			}
			idb, err := encodeID(uint64(i + 1))
			if err != nil {
				return err
			}
			if err := bkt.Put(bucketKeyID, idb); err != nil {
				return err
			}

			if a.lease != "" {
				if err := bkt.Put(bucketKeyLease, []byte(a.lease)); err != nil {
					return err
				}
				if lsbkt == nil {
					lsbkt, err = nsbkt.CreateBucketIfNotExists(bucketKeyLeases)
					if err != nil {
						return err
					}
				}
				lbkt, err := lsbkt.CreateBucketIfNotExists([]byte(a.lease))
				if err != nil {
					return err
				}
				if err := lbkt.Put([]byte(a.name), nil); err != nil {
					return err
				}
			}

			if len(a.labels) > 0 {
				lblbkt, err := bkt.CreateBucket(bucketKeyLabels)
				if err != nil {
					return err
				}
				for k, v := range a.labels {
					if err := lblbkt.Put([]byte(k), []byte(v)); err != nil {
						return err
					}
				}
			}

			if a.active != nil {
				abkt, err := bkt.CreateBucket(bucketKeyActive)
				if err != nil {
					return err
				}
				for j, act := range a.active {
					cur, err := abkt.CreateBucket([]byte{byte(j)})
					if err != nil {
						return err
					}
					if err := cur.Put(bucketKeyType, []byte(act.typ)); err != nil {
						return err
					}
					if err := cur.Put(bucketKeyMountPoint, []byte(act.mp)); err != nil {
						return err
					}
					atb, err := act.at.MarshalBinary()
					if err != nil {
						return err
					}
					if err := cur.Put(bucketKeyMountedAt, atb); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}))
}

// assertV1Present asserts whether the bucket name of the schema this
// package replaced is still present in db.
func assertV1Present(t *testing.T, db *bolt.DB, present bool) {
	t.Helper()
	require.NoError(t, db.View(func(tx *bolt.Tx) error {
		bkt := tx.Bucket([]byte("v1"))
		if present {
			assert.NotNil(t, bkt, "expected the legacy bucket to still be present")
		} else {
			assert.Nil(t, bkt, "expected the legacy bucket to be gone")
		}
		return nil
	}))
}

// TestMigrateOnActivate verifies that a legacy activation is converted
// to the current schema and made visible through Info as soon as any
// other Activate call runs, and that the legacy bucket is deleted once
// that happens.
func TestMigrateOnActivate(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")
	mountC := new(atomic.Int32)
	m, _ := mkTestManager(t, WithMountHandler("noop", &noopHandler{mounts: mountC}))
	mm := m.(*mountManager)

	at := time.Now().Truncate(time.Second)
	seedLegacyV1(t, mm.db, "test", legacyActivation{
		name: "old",
		labels: map[string]string{
			"containerd.io/gc.bref.container": "c1",
		},
		active: []legacyActive{
			{typ: "noop", mp: "/legacy/lower", at: at},
			{typ: "noop", mp: "/legacy/upper", at: at},
		},
	})
	assertV1Present(t, mm.db, true)

	// An unrelated Activate call is what triggers migration; it does
	// not have to concern the legacy activation at all.
	_, err := m.Activate(ctx, "unrelated", []mount.Mount{{Type: "noop", Source: "/dev/null"}})
	require.NoError(t, err)

	assertV1Present(t, mm.db, false)

	info, err := m.Info(ctx, "old")
	require.NoError(t, err)
	require.Len(t, info.Active, 2)
	assert.Equal(t, "noop", info.Active[0].Type)
	assert.Equal(t, "/legacy/lower", info.Active[0].MountPoint)
	assert.Equal(t, "noop", info.Active[1].Type)
	assert.Equal(t, "/legacy/upper", info.Active[1].MountPoint)
	assert.Equal(t, "c1", info.Labels["containerd.io/gc.bref.container"])

	require.NoError(t, m.Deactivate(ctx, "unrelated"))
	assert.Equal(t, int32(0), mountC.Load(), "the migrated activation was never really mounted through the handler")
}

// TestMigrateDropsIncompleteActivation verifies that a legacy
// activation with no active bucket, meaning it was interrupted before
// it completed, is dropped during migration rather than carried
// forward, exactly like an incomplete activation is dropped in the
// current schema, and that its name is free to reuse afterward.
func TestMigrateDropsIncompleteActivation(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")
	mountC := new(atomic.Int32)
	m, _ := mkTestManager(t, WithMountHandler("noop", &noopHandler{mounts: mountC}))
	mm := m.(*mountManager)

	seedLegacyV1(t, mm.db, "test",
		legacyActivation{
			name:   "keep",
			active: []legacyActive{{typ: "noop", mp: "/legacy/keep", at: time.Now()}},
		},
		legacyActivation{
			name:   "gone",
			active: nil,
		},
	)

	_, err := m.Activate(ctx, "unrelated", []mount.Mount{{Type: "noop", Source: "/dev/null"}})
	require.NoError(t, err)
	require.NoError(t, m.Deactivate(ctx, "unrelated"))

	_, err = m.Info(ctx, "keep")
	assert.NoError(t, err)

	_, err = m.Info(ctx, "gone")
	assert.True(t, errdefs.IsNotFound(err), "expected ErrNotFound, got %v", err)

	// The name must be free to reuse.
	_, err = m.Activate(ctx, "gone", []mount.Mount{{Type: "noop", Source: "/dev/zero"}})
	require.NoError(t, err)
	require.NoError(t, m.Deactivate(ctx, "gone"))
}

// TestMigratePreservesLeaseMembership verifies that a migrated
// activation's lease membership survives, and that an activation
// dropped for being incomplete does not leave a dangling entry in the
// lease it belonged to.
func TestMigratePreservesLeaseMembership(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")
	m, _ := mkTestManager(t, WithMountHandler("noop", &noopHandler{mounts: new(atomic.Int32)}))
	mm := m.(*mountManager)

	seedLegacyV1(t, mm.db, "test",
		legacyActivation{
			name:   "keep",
			lease:  "L",
			active: []legacyActive{{typ: "noop", mp: "/legacy/keep", at: time.Now()}},
		},
		legacyActivation{
			name:  "drop",
			lease: "L",
			// No active bucket: dropped by migration.
		},
	)

	_, err := m.Activate(ctx, "unrelated", []mount.Mount{{Type: "noop", Source: "/dev/null"}})
	require.NoError(t, err)
	require.NoError(t, m.Deactivate(ctx, "unrelated"))

	require.NoError(t, mm.db.View(func(tx *bolt.Tx) error {
		lbkt := getBucket(tx, bucketKeyVersion, []byte("test"), bucketKeyLeases, []byte("L"))
		require.NotNil(t, lbkt, "lease bucket should have been created for the surviving activation")
		assert.NotNil(t, lbkt.Get([]byte("keep")), "surviving activation should still be a lease member")
		assert.Nil(t, lbkt.Get([]byte("drop")), "dropped activation must not leave a dangling lease entry")
		return nil
	}))
}

// TestMigrateSkipsCollisionWithExistingActivation verifies that a
// legacy activation whose name collides with one already created under
// the current schema is discarded without disturbing the existing one.
// This can only arise from rolling back to a binary which still wrote
// the old schema and then rolling forward again, a transition this
// store does not promise to preserve; it must not error or corrupt the
// live activation.
func TestMigrateSkipsCollisionWithExistingActivation(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")
	mountC := new(atomic.Int32)
	m, _ := mkTestManager(t, WithMountHandler("noop", &noopHandler{mounts: mountC}))
	mm := m.(*mountManager)

	// A real, live activation created directly under the current
	// schema, as would happen after a rollback-then-roll-forward
	// sequence left the binary running on v2 again.
	ainfo, err := m.Activate(ctx, "a", []mount.Mount{{Type: "noop", Source: "/dev/null"}})
	require.NoError(t, err)
	liveMP := ainfo.Active[0].MountPoint

	// A namesake left behind under the legacy schema, as if a rolled
	// back binary had since recreated it.
	seedLegacyV1(t, mm.db, "test", legacyActivation{
		name:   "a",
		active: []legacyActive{{typ: "noop", mp: "/legacy/a", at: time.Now()}},
	})

	_, err = m.Activate(ctx, "unrelated", []mount.Mount{{Type: "noop", Source: "/dev/zero"}})
	require.NoError(t, err)

	assertV1Present(t, mm.db, false)

	info, err := m.Info(ctx, "a")
	require.NoError(t, err)
	require.Len(t, info.Active, 1)
	assert.Equal(t, liveMP, info.Active[0].MountPoint,
		"the live v2 activation must survive untouched, not be overwritten by the legacy namesake")

	require.NoError(t, m.Deactivate(ctx, "a"))
	require.NoError(t, m.Deactivate(ctx, "unrelated"))
	assert.Equal(t, int32(0), mountC.Load())
}

// TestMigrateOnDeactivate verifies that Deactivate triggers migration
// and can act on the very activation migration just produced, in the
// same transaction.
func TestMigrateOnDeactivate(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")
	mountC := new(atomic.Int32)
	m, _ := mkTestManager(t, WithMountHandler("noop", &noopHandler{mounts: mountC}))
	mm := m.(*mountManager)

	seedLegacyV1(t, mm.db, "test", legacyActivation{
		name:   "old",
		active: []legacyActive{{typ: "noop", mp: "/legacy/old", at: time.Now()}},
	})

	require.NoError(t, m.Deactivate(ctx, "old"))
	assertV1Present(t, mm.db, false)
	assert.Equal(t, int32(-1), mountC.Load(),
		"unmount was invoked once for the migrated mount, which this fixture never really mounted")

	_, err := m.Info(ctx, "old")
	assert.True(t, errdefs.IsNotFound(err), "expected ErrNotFound, got %v", err)
}

// TestMigrateOnInfo verifies that Info triggers migration itself when
// it is the first operation performed, rather than reporting the
// activation as missing because it is only sitting in the legacy
// bucket.
func TestMigrateOnInfo(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")
	m, _ := mkTestManager(t, WithMountHandler("noop", &noopHandler{mounts: new(atomic.Int32)}))
	mm := m.(*mountManager)

	seedLegacyV1(t, mm.db, "test", legacyActivation{
		name:   "old",
		active: []legacyActive{{typ: "noop", mp: "/legacy/old", at: time.Now()}},
	})

	info, err := m.Info(ctx, "old")
	require.NoError(t, err)
	require.Len(t, info.Active, 1)
	assert.Equal(t, "/legacy/old", info.Active[0].MountPoint)

	assertV1Present(t, mm.db, false)
}

// TestMigrateOnList verifies that List triggers migration itself when
// it is the first operation performed.
func TestMigrateOnList(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")
	m, _ := mkTestManager(t, WithMountHandler("noop", &noopHandler{mounts: new(atomic.Int32)}))
	mm := m.(*mountManager)

	seedLegacyV1(t, mm.db, "test", legacyActivation{
		name:   "old",
		active: []legacyActive{{typ: "noop", mp: "/legacy/old", at: time.Now()}},
	})

	infos, err := m.List(ctx)
	require.NoError(t, err)
	require.Len(t, infos, 1)
	assert.Equal(t, "old", infos[0].Name)

	assertV1Present(t, mm.db, false)
}

// TestMigrateOnGC verifies that starting a collection triggers
// migration before the collector's enumeration methods run, so a
// legacy activation is visible to garbage collection immediately
// rather than only after some other operation happens to migrate it
// first.
func TestMigrateOnGC(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")
	m, _ := mkTestManager(t, WithMountHandler("noop", &noopHandler{mounts: new(atomic.Int32)}))
	mm := m.(*mountManager)

	seedLegacyV1(t, mm.db, "test", legacyActivation{
		name:   "old",
		active: []legacyActive{{typ: "noop", mp: "/legacy/old", at: time.Now()}},
	})

	cc, err := m.(interface {
		StartCollection(context.Context) (metadata.CollectionContext, error)
	}).StartCollection(ctx)
	require.NoError(t, err)

	var all []string
	cc.All(func(n gc.Node) { all = append(all, n.Key) })
	assert.Contains(t, all, "old", "the migrated activation must be visible to the collector")

	require.NoError(t, cc.Finish())
	assertV1Present(t, mm.db, false)

	_, err = m.Info(ctx, "old")
	assert.NoError(t, err, "not removing it should leave it in place, same as any other survivor")
}

// TestMigratedMountReleaseDoesNotDisturbIndex verifies that releasing a
// migrated backing mount, which was never added to the dedup index
// because it has no recorded source, does not disturb the index entry
// of an unrelated, real shareable mount.
func TestMigratedMountReleaseDoesNotDisturbIndex(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")
	sharedC := new(atomic.Int32)
	legacyC := new(atomic.Int32)
	m, _ := mkTestManager(t,
		WithMountHandler("vol", &noopHandler{mounts: sharedC}),
		WithMountHandler("noop", &noopHandler{mounts: legacyC}),
	)
	mm := m.(*mountManager)

	// A real, shareable mount which is added to the dedup index.
	shared := mount.Mount{Type: "vol", Source: "/dev/null"}
	sharedInfo, err := m.Activate(ctx, "shared1", []mount.Mount{shared})
	require.NoError(t, err)
	sharedMP := sharedInfo.Active[0].MountPoint
	assert.Equal(t, int32(1), sharedC.Load())

	// A legacy activation whose migrated backing mount has no source
	// and so is never indexed.
	seedLegacyV1(t, mm.db, "test", legacyActivation{
		name:   "legacy",
		active: []legacyActive{{typ: "noop", mp: "/legacy/mp", at: time.Now()}},
	})

	// Deactivating it migrates first, then immediately releases the
	// migrated backing mount in the same transaction.
	require.NoError(t, m.Deactivate(ctx, "legacy"))
	assert.Equal(t, int32(-1), legacyC.Load())

	// The unrelated shareable mount, and its index entry, must be
	// unaffected.
	info, err := m.Info(ctx, "shared1")
	require.NoError(t, err)
	assert.Equal(t, sharedMP, info.Active[0].MountPoint)

	second, err := m.Activate(ctx, "shared2", []mount.Mount{shared})
	require.NoError(t, err)
	assert.Equal(t, sharedMP, second.Active[0].MountPoint, "the index must still resolve the shared mount")
	assert.Equal(t, int32(1), sharedC.Load(), "the shared mount must not have been mounted a second time")

	require.NoError(t, m.Deactivate(ctx, "shared1"))
	require.NoError(t, m.Deactivate(ctx, "shared2"))
	assert.Equal(t, int32(0), sharedC.Load())
}
