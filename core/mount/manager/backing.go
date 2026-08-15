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
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/containerd/errdefs"

	"github.com/containerd/containerd/v2/core/mount"
)

// backingDir is the directory under the target root which holds the
// mount point of every backing mount. A backing mount outlives the
// activation which created it, so it cannot live under a per activation
// directory. The short, non-numeric name also keeps it out of any
// numeric, per activation directories a different schema version may
// have created.
const backingDir = "b"

// mountPointName is the directory used as the mount point within a
// backing mount's directory. The sibling typeFileName records the
// mount type so an orphaned backing mount directory can still be
// unmounted with the correct handler after a restart.
const (
	mountPointName = "fs"
	typeFileName   = "type"
)

// backingMount is a mount performed by the manager. A single backing
// mount may back several activations: activations which describe the
// same mount within a namespace are backed by one mount instead of
// each performing their own, so it stays mounted for as long as any
// activation refers to it.
type backingMount struct {
	id    uint64
	mount mount.Mount
	point string
	at    *time.Time
}

// active returns the backing mount as an ActiveMount for reporting
// through ActivationInfo.
func (b backingMount) active() mount.ActiveMount {
	return mount.ActiveMount{
		Mount:      b.mount,
		MountPoint: b.point,
		MountedAt:  b.at,
	}
}

// mountIdentity returns a digest which uniquely identifies the kernel
// mount described by m. Two mounts with the same identity in the same
// namespace resolve to the same filesystem, so the manager performs
// the mount once and reference counts it.
//
// Fields are length prefixed so that no combination of values can
// produce the same digest as a different combination. Option order is
// significant because it is significant to the kernel.
func mountIdentity(m mount.Mount) []byte {
	h := sha256.New()
	var lenbuf [8]byte
	write := func(s string) {
		binary.BigEndian.PutUint64(lenbuf[:], uint64(len(s)))
		h.Write(lenbuf[:])
		h.Write([]byte(s))
	}
	write(m.Type)
	write(m.Source)
	write(m.Target)
	binary.BigEndian.PutUint64(lenbuf[:], uint64(len(m.Options)))
	h.Write(lenbuf[:])
	for _, o := range m.Options {
		write(o)
	}
	return h.Sum(nil)
}

// shareable reports whether m may be satisfied by an existing,
// identical backing mount.
//
// Only mounts whose source names a concrete object in the filesystem
// are shared. Mounting the same image, block file or directory twice
// yields two views of the same data, so a single mount can serve every
// chain which references it. Filesystems which synthesize their
// contents instead (tmpfs, proc, sysfs, mqueue, devpts, overlay, ...)
// use a symbolic source such as "tmpfs" or "none"; two such mounts
// with identical parameters are still distinct filesystems and must
// not be collapsed into one.
func shareable(m mount.Mount) bool {
	return filepath.IsAbs(m.Source)
}

// backingKey encodes a backing mount id for use as a bolt bucket key.
func backingKey(id uint64) []byte {
	b, _ := encodeID(id)
	return b
}

// backingRoot returns the directory holding a backing mount's mount
// point and type file.
func (mm *mountManager) backingRoot(id uint64) string {
	return filepath.Join(mm.targets.Name(), backingDir, strconv.FormatUint(id, 10))
}

// putBackingMount writes the mount parameters which identify a backing
// mount. These are written once, when the backing mount is created, and
// are never rewritten: they are the identity the dedup index is built
// from, and the transformed values reported back through
// ActivationInfo.
func putBackingMount(bkt *bolt.Bucket, m mount.Mount) error {
	if err := bkt.Put(bucketKeyType, []byte(m.Type)); err != nil {
		return err
	}
	if err := bkt.Put(bucketKeySource, []byte(m.Source)); err != nil {
		return err
	}
	if err := bkt.Put(bucketKeyTarget, []byte(m.Target)); err != nil {
		return err
	}
	if len(m.Options) > 0 {
		if err := bkt.Put(bucketKeyOptions, []byte(strings.Join(m.Options, "\x00"))); err != nil {
			return err
		}
	}
	return nil
}

// putBackingMounted records that a backing mount has been successfully
// mounted. Until this is written the backing mount is considered
// incomplete and is cleaned up rather than reused.
func putBackingMounted(bkt *bolt.Bucket, mountPoint string, at time.Time) error {
	if err := bkt.Put(bucketKeyMountPoint, []byte(mountPoint)); err != nil {
		return err
	}
	encoded, err := at.MarshalBinary()
	if err != nil {
		return err
	}
	return bkt.Put(bucketKeyMountedAt, encoded)
}

// readBackingMount reads a full backing mount record.
func readBackingMount(id uint64, bkt *bolt.Bucket) (backingMount, error) {
	b := backingMount{
		id: id,
		mount: mount.Mount{
			Type:   string(bkt.Get(bucketKeyType)),
			Source: string(bkt.Get(bucketKeySource)),
			Target: string(bkt.Get(bucketKeyTarget)),
		},
		point: string(bkt.Get(bucketKeyMountPoint)),
	}
	if v := bkt.Get(bucketKeyOptions); len(v) > 0 {
		b.mount.Options = strings.Split(string(v), "\x00")
	}
	if v := bkt.Get(bucketKeyMountedAt); len(v) > 0 {
		var at time.Time
		if err := at.UnmarshalBinary(v); err != nil {
			return backingMount{}, err
		}
		b.at = &at
	}
	return b, nil
}

// getBackingMount loads the backing mount with the given key from a
// namespace bucket, returning false when it no longer exists.
func getBackingMount(nsbkt *bolt.Bucket, key []byte) (backingMount, bool, error) {
	bbkt := nsbkt.Bucket(bucketKeyBacking)
	if bbkt == nil {
		return backingMount{}, false, nil
	}
	bkt := bbkt.Bucket(key)
	if bkt == nil {
		return backingMount{}, false, nil
	}
	id, _ := binary.Uvarint(key)
	b, err := readBackingMount(id, bkt)
	if err != nil {
		return backingMount{}, false, err
	}
	return b, true, nil
}

// claimBackingMount resolves m to a backing mount and adds a reference
// from the named activation, recording the claim at the given position
// in the activation's chain.
//
// When an identical, shareable backing mount already exists its id and
// mount point are returned and the caller must not mount anything.
// Otherwise a new backing mount is created and returned with an empty
// mount point, meaning the caller is responsible for performing the
// mount and calling completeBackingMount.
//
// The claim is recorded before the mount is performed so that an
// activation interrupted part way through still has a record of what
// it took references on, and can release them when it is replaced.
//
// Callers must hold the mounting lock for the mount identity so that
// concurrent activations of the same mount cannot both observe it
// missing.
func claimBackingMount(tx *bolt.Tx, namespace, name string, index int, m mount.Mount) (backingMount, error) {
	v1bkt, err := tx.CreateBucketIfNotExists(bucketKeyVersion)
	if err != nil {
		return backingMount{}, err
	}
	nsbkt, err := v1bkt.CreateBucketIfNotExists([]byte(namespace))
	if err != nil {
		return backingMount{}, err
	}
	bbkt, err := nsbkt.CreateBucketIfNotExists(bucketKeyBacking)
	if err != nil {
		return backingMount{}, err
	}

	if index < 0 || index > 255 {
		return backingMount{}, fmt.Errorf("mount index %d out of range: %w", index, errdefs.ErrInvalidArgument)
	}
	mbkt := getSubBucket(nsbkt, bucketKeyMounts, []byte(name))
	if mbkt == nil {
		return backingMount{}, fmt.Errorf("mount %q: %w", name, errdefs.ErrNotFound)
	}
	abkt, err := mbkt.CreateBucketIfNotExists(bucketKeyActive)
	if err != nil {
		return backingMount{}, err
	}
	cbkt, err := abkt.CreateBucketIfNotExists([]byte{byte(index)})
	if err != nil {
		return backingMount{}, err
	}

	share := shareable(m)
	var identity []byte
	if share {
		identity = mountIdentity(m)
		xbkt := nsbkt.Bucket(bucketKeyBackingIndex)
		if xbkt != nil {
			if k := xbkt.Get(identity); len(k) > 0 {
				if bkt := bbkt.Bucket(k); bkt != nil {
					id, _ := binary.Uvarint(k)
					existing, err := readBackingMount(id, bkt)
					if err != nil {
						return backingMount{}, err
					}
					// A record without a mount point never completed;
					// fall through and replace it rather than handing
					// back a path which was never mounted.
					if existing.point != "" {
						rbkt, err := bkt.CreateBucketIfNotExists(bucketKeyRefs)
						if err != nil {
							return backingMount{}, err
						}
						if err := rbkt.Put([]byte(name), nil); err != nil {
							return backingMount{}, err
						}
						if err := cbkt.Put(bucketKeyBackedBy, k); err != nil {
							return backingMount{}, err
						}
						return existing, nil
					}
					if err := bbkt.DeleteBucket(k); err != nil {
						return backingMount{}, err
					}
				}
				if err := xbkt.Delete(identity); err != nil {
					return backingMount{}, err
				}
			}
		}
	}

	id, err := v1bkt.NextSequence()
	if err != nil {
		return backingMount{}, err
	}
	key := backingKey(id)
	bkt, err := bbkt.CreateBucket(key)
	if err != nil {
		return backingMount{}, err
	}
	if err := putBackingMount(bkt, m); err != nil {
		return backingMount{}, err
	}
	rbkt, err := bkt.CreateBucket(bucketKeyRefs)
	if err != nil {
		return backingMount{}, err
	}
	if err := rbkt.Put([]byte(name), nil); err != nil {
		return backingMount{}, err
	}
	if share {
		xbkt, err := nsbkt.CreateBucketIfNotExists(bucketKeyBackingIndex)
		if err != nil {
			return backingMount{}, err
		}
		if err := xbkt.Put(identity, key); err != nil {
			return backingMount{}, err
		}
	}
	if err := cbkt.Put(bucketKeyBackedBy, key); err != nil {
		return backingMount{}, err
	}

	return backingMount{id: id, mount: m}, nil
}

// completeBackingMount records the mount point of a backing mount which
// the caller has just mounted, making it eligible for reuse.
func completeBackingMount(tx *bolt.Tx, namespace string, id uint64, mountPoint string, at time.Time) error {
	bkt := getBucket(tx, bucketKeyVersion, []byte(namespace), bucketKeyBacking, backingKey(id))
	if bkt == nil {
		return fmt.Errorf("backing mount %d: %w", id, errdefs.ErrNotFound)
	}
	return putBackingMounted(bkt, mountPoint, at)
}

// releaseBackingMounts drops the named activation's references from the
// given backing mounts and returns those which lost their last
// reference. Released backing mounts are removed from the database and
// returned to the caller for unmounting, ordered so that a mount built
// on another is unmounted before the mount it was built on.
func releaseBackingMounts(tx *bolt.Tx, namespace, name string, ids []uint64) ([]backingMount, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	nsbkt := getBucket(tx, bucketKeyVersion, []byte(namespace))
	if nsbkt == nil {
		return nil, nil
	}
	bbkt := nsbkt.Bucket(bucketKeyBacking)
	if bbkt == nil {
		return nil, nil
	}
	xbkt := nsbkt.Bucket(bucketKeyBackingIndex)

	var released []backingMount
	for _, id := range ids {
		if id == 0 {
			continue
		}
		key := backingKey(id)
		bkt := bbkt.Bucket(key)
		if bkt == nil {
			continue
		}
		if rbkt := bkt.Bucket(bucketKeyRefs); rbkt != nil {
			if err := rbkt.Delete([]byte(name)); err != nil {
				return nil, err
			}
			if k, _ := rbkt.Cursor().First(); k != nil {
				// Still referenced by another activation.
				continue
			}
		}
		b, err := readBackingMount(id, bkt)
		if err != nil {
			return nil, err
		}
		// Only ever added to the index when shareable; skip the
		// lookup for one that was not, rather than compute a digest
		// which was never a key in it.
		if xbkt != nil && shareable(b.mount) {
			if err := xbkt.Delete(mountIdentity(b.mount)); err != nil {
				return nil, err
			}
		}
		if err := bbkt.DeleteBucket(key); err != nil {
			return nil, err
		}
		released = append(released, b)
	}

	sortBackingUnmountOrder(released)

	return released, nil
}

// sortBackingUnmountOrder orders backing mounts so they can be safely
// unmounted.
//
// A mount which references another mount's mount point is always
// created after the mount it depends on and therefore has a higher
// backing id, so unmounting in descending id order never unmounts a
// filesystem which is still underneath another.
func sortBackingUnmountOrder(backing []backingMount) {
	sort.Slice(backing, func(a, b int) bool {
		return backing[a].id > backing[b].id
	})
}

// activationBackingIDs returns the backing mount ids backing an
// activation, in mount order.
func activationBackingIDs(bkt *bolt.Bucket) []uint64 {
	abkt := bkt.Bucket(bucketKeyActive)
	if abkt == nil {
		return nil
	}
	var ids []uint64
	abkt.ForEachBucket(func(k []byte) error {
		if v := abkt.Bucket(k).Get(bucketKeyBackedBy); len(v) > 0 {
			id, _ := binary.Uvarint(v)
			ids = append(ids, id)
		}
		return nil
	})
	return ids
}

// mountingKey returns the lock key used to serialize activations which
// resolve to the same mount.
func mountingKey(m mount.Mount) string {
	return hex.EncodeToString(mountIdentity(m))
}
