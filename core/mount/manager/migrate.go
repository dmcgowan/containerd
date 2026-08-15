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
	"bytes"
	"errors"
	"fmt"

	"github.com/containerd/log"
	bolt "go.etcd.io/bbolt"
)

// errNeedsMigration signals that a read observed data left behind by
// the schema this package replaced. It never escapes this package:
// Info and List, the two read only entry points, escalate to a write
// transaction on this error, which performs the migration and then
// retries the read. Every other entry point already runs inside a
// write transaction and simply calls migrateFromV1 unconditionally,
// since the check it starts with is a single bucket lookup.
var errNeedsMigration = errors.New("mount database needs migration")

// checkNotV1 reports errNeedsMigration if tx still has data recorded
// under the schema this package replaced.
func checkNotV1(tx *bolt.Tx) error {
	if tx.Bucket(bucketKeyVersionV1) != nil {
		return errNeedsMigration
	}
	return nil
}

// migrateFromV1 converts any activations recorded under the bucket
// name of the schema this package replaced into the current one, then
// deletes the old bucket. It is a cheap no-op, a single bucket lookup,
// when there is nothing to convert.
//
// Called at the start of every write transaction, so whichever
// operation happens to run first after an upgrade performs the
// conversion and commits it together with whatever else that
// operation was already doing; from then on this is permanently a
// no-op for the life of the database.
func migrateFromV1(tx *bolt.Tx) error {
	oldbkt := tx.Bucket(bucketKeyVersionV1)
	if oldbkt == nil {
		return nil
	}

	v2bkt, err := tx.CreateBucketIfNotExists(bucketKeyVersion)
	if err != nil {
		return err
	}

	// Snapshot the namespace names before descending into them:
	// migrating a namespace mutates sibling state (the backing mount
	// id sequence lives in v2bkt), which must not happen while a
	// cursor is open over oldbkt.
	var namespaces [][]byte
	if err := oldbkt.ForEachBucket(func(k []byte) error {
		namespaces = append(namespaces, bytes.Clone(k))
		return nil
	}); err != nil {
		return err
	}

	for _, ns := range namespaces {
		newnsbkt, err := v2bkt.CreateBucketIfNotExists(ns)
		if err != nil {
			return err
		}
		if err := migrateNamespaceFromV1(v2bkt, newnsbkt, oldbkt.Bucket(ns)); err != nil {
			return fmt.Errorf("failed to migrate namespace %q: %w", ns, err)
		}
	}

	return tx.DeleteBucket(bucketKeyVersionV1)
}

// migrateNamespaceFromV1 migrates one namespace's activations and
// lease index.
func migrateNamespaceFromV1(v2bkt, newnsbkt, oldnsbkt *bolt.Bucket) error {
	// Names of activations which produced a surviving v2 entry, so the
	// lease index below only carries forward entries for those.
	survived := map[string]struct{}{}

	if oldmbkt := oldnsbkt.Bucket(bucketKeyMounts); oldmbkt != nil {
		newmbkt, err := newnsbkt.CreateBucketIfNotExists(bucketKeyMounts)
		if err != nil {
			return err
		}

		var names [][]byte
		if err := oldmbkt.ForEachBucket(func(k []byte) error {
			names = append(names, bytes.Clone(k))
			return nil
		}); err != nil {
			return err
		}

		for _, name := range names {
			if newmbkt.Bucket(name) != nil {
				// Can only happen after rolling back to a binary
				// which still wrote v1, then rolling forward again:
				// the name was reused for a live v2 activation before
				// this pass ever ran. That sequence is not one this
				// store promises to preserve; keep the v2 entry,
				// which describes mounts this process can actually
				// account for, and drop the v1 one.
				log.L.WithField("name", string(name)).Warn(
					"discarding a mount activation left over from a previous version of the mount database: " +
						"a namesake was already created under the current version")
				continue
			}
			ok, err := migrateActivationFromV1(v2bkt, newnsbkt, newmbkt, oldmbkt.Bucket(name), name)
			if err != nil {
				return fmt.Errorf("failed to migrate activation %q: %w", name, err)
			}
			if ok {
				survived[string(name)] = struct{}{}
			}
		}
	}

	if oldlsbkt := oldnsbkt.Bucket(bucketKeyLeases); oldlsbkt != nil && len(survived) > 0 {
		newlsbkt, err := newnsbkt.CreateBucketIfNotExists(bucketKeyLeases)
		if err != nil {
			return err
		}

		var leaseIDs [][]byte
		if err := oldlsbkt.ForEachBucket(func(k []byte) error {
			leaseIDs = append(leaseIDs, bytes.Clone(k))
			return nil
		}); err != nil {
			return err
		}

		for _, lid := range leaseIDs {
			oldlbkt := oldlsbkt.Bucket(lid)
			var names [][]byte
			if err := oldlbkt.ForEach(func(name, _ []byte) error {
				if _, ok := survived[string(name)]; ok {
					names = append(names, bytes.Clone(name))
				}
				return nil
			}); err != nil {
				return err
			}
			if len(names) == 0 {
				continue
			}
			newlbkt, err := newlsbkt.CreateBucketIfNotExists(lid)
			if err != nil {
				return err
			}
			for _, name := range names {
				if err := newlbkt.Put(name, nil); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// migrateActivationFromV1 converts one activation from the schema this
// package replaced. It reports false when the activation is dropped
// rather than carried forward: one with no active bucket recorded no
// mounts, exactly like an activation interrupted before completion in
// the current schema, and is discarded the same way.
func migrateActivationFromV1(v2bkt, newnsbkt, newmbkt, oldbkt *bolt.Bucket, name []byte) (bool, error) {
	oldactivebkt := oldbkt.Bucket(bucketKeyActive)
	if oldactivebkt == nil {
		return false, nil
	}

	newbkt, err := newmbkt.CreateBucket(name)
	if err != nil {
		return false, err
	}

	// id, lease, createdat and updatedat are plain keys at this level
	// in both schemas; copy whatever is there rather than naming each
	// one, so this does not have to track boltutil's key names.
	if err := oldbkt.ForEach(func(k, v []byte) error {
		if v == nil {
			return nil
		}
		return newbkt.Put(k, v)
	}); err != nil {
		return false, err
	}

	// The presence of the active bucket was the v1 completion marker;
	// v2 makes it explicit instead.
	if err := newbkt.Put(bucketKeyComplete, []byte{1}); err != nil {
		return false, err
	}

	if oldlabels := oldbkt.Bucket(bucketKeyLabels); oldlabels != nil {
		newlabels, err := newbkt.CreateBucket(bucketKeyLabels)
		if err != nil {
			return false, err
		}
		if err := copyBucket(newlabels, oldlabels); err != nil {
			return false, err
		}
	}

	if oldsystem := oldbkt.Bucket(bucketKeySystem); oldsystem != nil {
		newsystem, err := newbkt.CreateBucket(bucketKeySystem)
		if err != nil {
			return false, err
		}
		if err := copyBucket(newsystem, oldsystem); err != nil {
			return false, err
		}
	}

	newactivebkt, err := newbkt.CreateBucket(bucketKeyActive)
	if err != nil {
		return false, err
	}
	if err := migrateActiveFromV1(v2bkt, newnsbkt, oldactivebkt, newactivebkt, name); err != nil {
		return false, err
	}

	return true, nil
}

// migrateActiveFromV1 converts a v1 "active" bucket, which stored each
// position's mount inline, into a v2 one, which only ever points at a
// backing mount. A new backing mount is allocated for every position:
// migrated mounts are never deduplicated against one another or
// against mounts made from this point on, because the parameters v1
// recorded (only type and mount point; source, target and options
// were never implemented there) are not enough to tell whether two of
// them actually describe the same filesystem. shareable, which decides
// that, already reports false for a mount with no source, so this
// happens automatically rather than needing special casing here.
func migrateActiveFromV1(v2bkt, newnsbkt, oldactivebkt, newactivebkt *bolt.Bucket, name []byte) error {
	bbkt, err := newnsbkt.CreateBucketIfNotExists(bucketKeyBacking)
	if err != nil {
		return err
	}

	var positions [][]byte
	if err := oldactivebkt.ForEachBucket(func(k []byte) error {
		positions = append(positions, bytes.Clone(k))
		return nil
	}); err != nil {
		return err
	}

	for _, order := range positions {
		oldcur := oldactivebkt.Bucket(order)

		id, err := v2bkt.NextSequence()
		if err != nil {
			return err
		}
		key := backingKey(id)
		newbbkt, err := bbkt.CreateBucket(key)
		if err != nil {
			return err
		}
		if v := oldcur.Get(bucketKeyType); v != nil {
			if err := newbbkt.Put(bucketKeyType, v); err != nil {
				return err
			}
		}
		if v := oldcur.Get(bucketKeyMountPoint); v != nil {
			if err := newbbkt.Put(bucketKeyMountPoint, v); err != nil {
				return err
			}
		}
		if v := oldcur.Get(bucketKeyMountedAt); v != nil {
			if err := newbbkt.Put(bucketKeyMountedAt, v); err != nil {
				return err
			}
		}
		rbkt, err := newbbkt.CreateBucket(bucketKeyRefs)
		if err != nil {
			return err
		}
		if err := rbkt.Put(name, nil); err != nil {
			return err
		}

		newcur, err := newactivebkt.CreateBucket(order)
		if err != nil {
			return err
		}
		if err := newcur.Put(bucketKeyBackedBy, key); err != nil {
			return err
		}
	}

	return nil
}

// copyBucket recursively copies every key and sub-bucket from src into
// dst, which must be empty.
func copyBucket(dst, src *bolt.Bucket) error {
	c := src.Cursor()
	for k, v := c.First(); k != nil; k, v = c.Next() {
		if v != nil {
			if err := dst.Put(k, v); err != nil {
				return err
			}
			continue
		}
		child, err := dst.CreateBucket(k)
		if err != nil {
			return err
		}
		if err := copyBucket(child, src.Bucket(k)); err != nil {
			return err
		}
	}
	return nil
}
