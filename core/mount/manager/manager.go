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
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/containerd/errdefs"
	"github.com/containerd/log"

	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/metadata"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/pkg/gc"
	"github.com/containerd/containerd/v2/pkg/namespaces"
)

type BoltManager interface {
	mount.MountManager
	metadata.Collector
	Sync(context.Context) error
}

func NewManager(db *bolt.DB, targetDir string, handlers map[string]mount.MountHandler) mount.MountManager {
	return &mountManager{
		db:       db,
		targets:  targetDir,
		handlers: handlers,
	}
}

type mountManager struct {
	db       *bolt.DB
	targets  string
	handlers map[string]mount.MountHandler

	rwlock sync.RWMutex
}

func (mm *mountManager) Activate(ctx context.Context, name string, mounts []mount.Mount, opts ...mount.ActivateOpt) (info mount.ActivationInfo, retErr error) {
	namespace, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return mount.ActivationInfo{}, err
	}

	lid, leased := leases.FromContext(ctx)
	if !leased {
		// TODO: Set a default expiration? Otherwise will be immediately available for GC if nothing references
	}

	var config mount.ActivateOptions
	for _, opt := range opts {
		opt(&config)
	}

	// highest index of a mount
	// first system mount is the first index which should be mounted by the system
	var firstSystemMount = -1
	var mountConv []mountConverter
	var handlers []mount.MountHandler
	for i := range mounts {
		// Check is the source needs formatting, any formatting requires
		// mounting with the mount manager.
		if strings.HasPrefix(mounts[i].Type, "format/") {
			if i == 0 {
				return mount.ActivationInfo{}, fmt.Errorf("first mount cannot be formatted, no mount prior mount state: %w", errdefs.ErrInvalidArgument)
			}

			// At least everything before this must be mounted
			// by the mount manager
			firstSystemMount = i
			if handlers == nil {
				handlers = make([]mount.MountHandler, len(mounts))
			}

			// Strip "format/" from beginning before looking for handler
			mounts[i].Type = mounts[i].Type[7:]

			if mountConv == nil {
				mountConv = make([]mountConverter, len(mounts))
			}

			mv, err := formatMount(mounts[i])
			if err != nil {
				return mount.ActivationInfo{}, err
			}

			mountConv[i] = mv
		} else if mm.handlers != nil {
			handler, ok := mm.handlers[mounts[i].Type]
			if ok {
				if handlers == nil {
					handlers = make([]mount.MountHandler, len(mounts))
				}
				handlers[i] = handler
				firstSystemMount = i + 1
			}
		}
	}
	// If no mounts are handled here, return not implemented and caller
	// may just perform system mounts as normal.
	if firstSystemMount == -1 {
		return mount.ActivationInfo{}, errdefs.ErrNotImplemented
	}

	// TODO: Get read lock to block GC context from starting
	mm.rwlock.RLock()
	defer mm.rwlock.RUnlock()

	var mid uint64

	if err := mm.db.Update(func(tx *bolt.Tx) error {
		v1bkt, err := tx.CreateBucketIfNotExists([]byte("v1"))
		if err != nil {
			return err
		}

		nsbkt, err := v1bkt.CreateBucketIfNotExists([]byte(namespace))
		if err != nil {
			return err
		}
		mbkt, err := nsbkt.CreateBucketIfNotExists(bucketKeyMounts)
		if err != nil {
			return err
		}
		bkt, err := mbkt.CreateBucket([]byte(name))
		if err != nil {
			// If already exists, return already exists
			return err
		}

		mid, err = v1bkt.NextSequence()
		if err != nil {
			return err
		}

		idb, err := encodeID(mid)
		if err != nil {
			return err
		}
		if err = bkt.Put(bucketKeyID, idb); err != nil {
			return err
		}

		// Setup mounts now with generated targets
		// TODO: Write created at time
		// TODO: Write labels
		// TODO: Store mount information including mountpoint

		// TODO: Write lease
		if leased {
			if err = bkt.Put(bucketKeyLease, []byte(lid)); err != nil {
				return err
			}

			lsbkt, err := nsbkt.CreateBucketIfNotExists(bucketKeyLeases)
			if err != nil {
				return err
			}
			lbkt, err := lsbkt.CreateBucketIfNotExists([]byte(lid))
			if err != nil {
				return err
			}
			if err := lbkt.Put([]byte(name), nil); err != nil {
				return err
			}
		}

		return nil
	}); err != nil {
		return mount.ActivationInfo{}, err
	}

	// TODO: If error, rollback and remove by name
	defer func() {
		// TODO: Any error should attempt to unmount all mounted
		if retErr != nil {
			if err := mm.db.Update(func(tx *bolt.Tx) error {
				v1bkt := tx.Bucket([]byte("v1"))
				if v1bkt == nil {
					return fmt.Errorf("missing bucket: %w", errdefs.ErrUnknown)
				}

				nsbkt := v1bkt.Bucket([]byte(namespace))
				if nsbkt == nil {
					return fmt.Errorf("missing namespace %q bucket: %w", namespace, errdefs.ErrUnknown)
				}

				mbkt := nsbkt.Bucket(bucketKeyMounts)
				if mbkt == nil {
					return fmt.Errorf("missing mounts bucket: %w", errdefs.ErrUnknown)
				}

				if leased {
					lsbkt := nsbkt.Bucket(bucketKeyLeases)
					if lsbkt != nil {
						lbkt := lsbkt.Bucket([]byte(lid))
						if lbkt != nil {
							lbkt.Delete([]byte(name))
						}
						if k, _ := lbkt.Cursor().First(); k == nil {
							lsbkt.DeleteBucket([]byte(lid))
						}
					}

				}

				return mbkt.DeleteBucket([]byte(name))
			}); err != nil {
				log.G(ctx).WithError(err).WithField("name", name).Errorf("failed to rollback")
			}
		}
	}()

	targetBase := filepath.Join(mm.targets, fmt.Sprintf("%d", mid))
	if err := os.MkdirAll(targetBase, 0700); err != nil {
		return mount.ActivationInfo{}, err
	}

	var mounted []mount.ActiveMount
	defer func() {
		if retErr != nil {
			for i, m := range mounted {
				var err error
				if h := handlers[i]; h != nil {
					err = h.Unmount(ctx, m.MountPoint)
				} else {
					err = mount.Unmount(m.MountPoint, 0)
				}
				if err != nil {
					log.G(ctx).WithError(err).WithField("MountPoint", m.MountPoint).Error("failed to cleanup mount after failed activation")
				}
			}
		}
	}()

	// Ensure directory order for cleanup when rare case of large number of mounts,
	// this allows cleanup logic to just scan directories on cleanup.
	formatMP := "%d"
	formatType := "%d-type"
	if firstSystemMount > 100 {
		formatMP = "%03d"
		formatType = "%03d-type"
	} else if firstSystemMount > 10 {
		formatMP = "%02d"
		formatType = "%02d-type"
	}

	for i, m := range mounts[:firstSystemMount] {
		if mountConv != nil && mountConv[i] != nil {
			newM, err := mountConv[i](m, mounted)
			if err != nil {
				return mount.ActivationInfo{}, err
			}
			m = newM
		}

		// Use cleanup order for directory names
		ci := firstSystemMount - i
		t := filepath.Join(targetBase, fmt.Sprintf(formatType, ci))
		if err := os.WriteFile(t, []byte(m.Type), 0600); err != nil {
			return mount.ActivationInfo{}, err
		}
		mp := filepath.Join(targetBase, fmt.Sprintf(formatMP, ci))

		var active mount.ActiveMount
		if h := handlers[i]; h != nil {
			active, err = h.Mount(ctx, m, mp, mounted)
			if err != nil {
				return mount.ActivationInfo{}, err
			}
		} else {
			if err := os.Mkdir(mp, 0700); err != nil {
				return mount.ActivationInfo{}, err
			}
			if err := m.Mount(mp); err != nil {
				return mount.ActivationInfo{}, fmt.Errorf("mount failed %v: %w", m, err)
			}
			t := time.Now()
			active = mount.ActiveMount{
				Mount:      m,
				MountPoint: mp,
				MountedAt:  &t,
			}
		}
		mounted = append(mounted, active)
	}

	// If first system mount is converted, fill in the format
	if mountConv != nil {
		if t := mountConv[firstSystemMount]; t != nil {
			newM, err := t(mounts[firstSystemMount], mounted)
			if err != nil {
				return mount.ActivationInfo{}, err
			}
			mounts[firstSystemMount] = newM
		}
	}

	info.Name = name
	info.Active = mounted
	info.System = mounts[firstSystemMount:]
	info.Labels = config.Labels

	// Open another write transaction and update state, or another way to update state?
	if err := mm.db.Update(func(tx *bolt.Tx) error {
		v1bkt := tx.Bucket([]byte("v1"))
		if v1bkt == nil {
			return fmt.Errorf("missing v1 bucket: %w", errdefs.ErrUnknown)
		}

		nsbkt := v1bkt.Bucket([]byte(namespace))
		if nsbkt == nil {
			return fmt.Errorf("missing namespace %q bucket: %w", namespace, errdefs.ErrUnknown)
		}

		mbkt := nsbkt.Bucket(bucketKeyMounts)
		if mbkt == nil {
			return fmt.Errorf("missing mounts bucket: %w", errdefs.ErrUnknown)
		}
		bkt := mbkt.Bucket([]byte(name))
		if bkt == nil {
			return fmt.Errorf("missing mount %q bucket: %w", name, errdefs.ErrUnknown)
		}

		abkt, err := bkt.CreateBucket(bucketKeyActive)
		if err != nil {
			return err
		}

		for i, active := range mounted {
			// Error is i > uint8 max
			cur, err := abkt.CreateBucket([]byte{byte(i)})
			if err != nil {
				return err
			}
			if err = putActiveMount(cur, active); err != nil {
				return err
			}

		}

		// TODO: Save all system mounts

		return nil
	}); err != nil {
		return mount.ActivationInfo{}, err
	}

	return
}

func encodeID(id uint64) ([]byte, error) {
	var (
		buf       [binary.MaxVarintLen64]byte
		idEncoded = buf[:]
	)
	idEncoded = idEncoded[:binary.PutUvarint(idEncoded, id)]

	if len(idEncoded) == 0 {
		return nil, fmt.Errorf("failed encoding id = %v", id)
	}
	return idEncoded, nil
}

func readID(bkt *bolt.Bucket) uint64 {
	id, _ := binary.Uvarint(bkt.Get(bucketKeyID))
	return id
}

func putActiveMount(bkt *bolt.Bucket, active mount.ActiveMount) error {
	if err := bkt.Put(bucketKeyType, []byte(active.Type)); err != nil {
		return err
	}

	// TODO: Same if device?
	if err := bkt.Put(bucketKeyMountPoint, []byte(active.MountPoint)); err != nil {
		return err
	}

	mountedAt, err := active.MountedAt.MarshalBinary()
	if err != nil {
		return err
	}
	if err := bkt.Put(bucketKeyMountedAt, mountedAt); err != nil {
		return err
	}

	// TODO: Add Source
	// TODO: Add Target
	// TODO: Add Options

	return nil
}

func readActiveMount(bkt *bolt.Bucket) (mount.ActiveMount, error) {
	var active mount.ActiveMount
	active.Type = string(bkt.Get(bucketKeyType))
	active.MountPoint = string(bkt.Get(bucketKeyMountPoint))
	if v := bkt.Get(bucketKeyMountedAt); v != nil {
		var mountedAt time.Time
		if err := mountedAt.UnmarshalBinary(v); err != nil {
			// TODO: Should this be skipped or otherwise logged and ignored?
			return mount.ActiveMount{}, err
		}
		active.MountedAt = &mountedAt
	}

	return active, nil
}

func createBucketIfNotExists(tx *bolt.Tx, keys ...[]byte) (*bolt.Bucket, error) {
	bkt, err := tx.CreateBucketIfNotExists(keys[0])
	if err != nil {
		return nil, err
	}

	for _, key := range keys[1:] {
		bkt, err = bkt.CreateBucketIfNotExists(key)
		if err != nil {
			return nil, err
		}
	}

	return bkt, nil
}

func getBucket(tx *bolt.Tx, keys ...[]byte) *bolt.Bucket {
	bkt := tx.Bucket(keys[0])
	if bkt == nil {
		return nil
	}

	for _, key := range keys[1:] {
		bkt = bkt.Bucket(key)
		if bkt == nil {
			return nil
		}
	}

	return bkt
}

func (mm *mountManager) Deactivate(ctx context.Context, name string) error {
	namespace, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return err
	}

	var (
		mid       uint64
		allActive []mount.ActiveMount
	)

	// First in a single transaction, mark the mounts as deactivated
	if err := mm.db.Update(func(tx *bolt.Tx) error {
		v1bkt := tx.Bucket([]byte("v1"))
		if v1bkt == nil {
			return fmt.Errorf("missing v1 bucket: %w", errdefs.ErrUnknown)
		}

		nsbkt := v1bkt.Bucket([]byte(namespace))
		if nsbkt == nil {
			return fmt.Errorf("missing namespace %q bucket: %w", namespace, errdefs.ErrUnknown)
		}

		mbkt := nsbkt.Bucket(bucketKeyMounts)
		if mbkt == nil {
			return fmt.Errorf("missing mounts bucket: %w", errdefs.ErrUnknown)
		}
		bkt := mbkt.Bucket([]byte(name))
		if bkt == nil {
			return fmt.Errorf("missing mount %q bucket: %w", name, errdefs.ErrUnknown)
		}

		mid = readID(bkt)

		lid := bkt.Get(bucketKeyLease)
		if lid != nil {
			lssbkt := nsbkt.Bucket(bucketKeyLeases)
			if lssbkt != nil {
				lsbkt := lssbkt.Bucket(lid)
				if lsbkt != nil {
					if err = lsbkt.Delete([]byte(name)); err != nil {
						return err
					}
				}
			}
		}

		abkt := bkt.Bucket(bucketKeyActive)
		if abkt != nil {
			abkt.ForEachBucket(func(k []byte) error {
				active, err := readActiveMount(abkt.Bucket(k))
				if err != nil {
					return err
				}
				allActive = append(allActive, active)
				return nil
			})
		}

		if err = mbkt.DeleteBucket([]byte(name)); err != nil {
			return err
		}

		// TODO: Is unmountq really needed or just delete?

		return nil
	}); err != nil {
		return err
	}

	// TODO: Should this also be backgrounded, no much can do on failure to unmount
	var mountErrors error
	for i := len(allActive) - 1; i >= 0; i-- {
		var err error
		if h := mm.handlers[allActive[i].Type]; h != nil {
			err = h.Unmount(ctx, allActive[i].MountPoint)
		} else {
			err = mount.Unmount(allActive[i].MountPoint, 0)
		}
		if err != nil {
			mountErrors = errors.Join(mountErrors, err)
		}
	}
	if mountErrors != nil {
		// Don't try to cleanup, GC will need to do the rest
		return mountErrors
	}

	// Run in background, GC would handle leftovers?
	// Make configurable?
	if err := os.RemoveAll(filepath.Join(mm.targets, fmt.Sprintf("%d", mid))); err != nil {
		// TODO: Only log here, cleanup would have to occur later
	}

	return nil
}

func (mm *mountManager) Info(context.Context, string) (mount.ActivationInfo, error) {
	return mount.ActivationInfo{}, errdefs.ErrNotImplemented
}

func (mm *mountManager) Update(context.Context, mount.ActivationInfo, ...string) (mount.ActivationInfo, error) {
	return mount.ActivationInfo{}, errdefs.ErrNotImplemented
}

func (mm *mountManager) List(context.Context, ...string) ([]mount.ActivationInfo, error) {
	return nil, errdefs.ErrNotImplemented
}

func (mm *mountManager) StartCollection(ctx context.Context) (metadata.CollectionContext, error) {
	// lock now and collection will unlock on cancel or finish
	mm.rwlock.Lock()

	tx, err := mm.db.Begin(true)
	if err != nil {
		return nil, err
	}

	return &collectionContext{
		ctx:     ctx,
		tx:      tx,
		manager: mm,
		removed: map[string]map[string]struct{}{},
	}, nil
}

func (sm *mountManager) ReferenceLabel() string {
	return "mount"
}

type collectionContext struct {
	ctx     context.Context
	tx      *bolt.Tx
	manager *mountManager
	removed map[string]map[string]struct{}
}

func (cc *collectionContext) All(fn func(gc.Node)) {
	v1bkt := cc.tx.Bucket([]byte("v1"))
	if v1bkt == nil {
		return
	}
	nsc := v1bkt.Cursor()
	for nsk, nsv := nsc.First(); nsk != nil; nsk, nsv = nsc.Next() {
		if nsv != nil {
			continue
		}
		mc := v1bkt.Bucket(nsk).Cursor()
		for mk, mv := mc.First(); mk != nil; mk, mv = mc.Next() {
			if mv != nil {
				continue
			}
			fn(gc.Node{
				Type:      metadata.ResourceMount,
				Namespace: string(nsk),
				Key:       string(mk),
			})
		}
	}
}

func (cc *collectionContext) Active(ns string, fn func(gc.Node)) {
	nsbkt := getBucket(cc.tx, []byte("v1"), []byte(ns))
	if nsbkt != nil {
		// TODO: Check labels
		mc := nsbkt.Cursor()
		for mk, mv := mc.First(); mk != nil; mk, mv = mc.Next() {
			if mv != nil {
				continue
			}
			// TODO: Check for root/expire labels
			/*
				fn(gc.Node{
					Type:      metadata.ResourceMount,
					Namespace: ns,
					Key:       string(mk),
				})
			*/
		}
	}
}

func (cc *collectionContext) Leased(ns, lease string, fn func(gc.Node)) {
	bkt := getBucket(cc.tx, []byte("v1"), []byte(ns), []byte("leases"), []byte(lease))
	if bkt != nil {
		c := bkt.Cursor()
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			fn(gc.Node{
				Type:      metadata.ResourceMount,
				Namespace: ns,
				Key:       string(k),
			})
		}
	}
}

func (cc *collectionContext) Remove(n gc.Node) {
	if n.Type != metadata.ResourceMount {
		return
	}
	nmap, ok := cc.removed[n.Namespace]
	if !ok {
		if _, ok = nmap[n.Key]; !ok {
			nmap[n.Key] = struct{}{}
		}
	} else {
		cc.removed[n.Namespace] = map[string]struct{}{
			n.Key: struct{}{},
		}
	}
}

func (cc *collectionContext) Cancel() (err error) {
	err = cc.tx.Rollback()
	cc.manager.rwlock.Unlock()
	return
}

func (cc *collectionContext) Finish() error {
	// TODO: Get list of all remaining
	remaining, err := cc.applyRemove()
	if err != nil {
		if rerr := cc.tx.Rollback(); rerr != nil {
			err = errors.Join(err, rerr)
		}
	} else {
		err = cc.tx.Commit()
	}
	if err != nil {
		cc.manager.rwlock.Unlock()
		return err
	}

	// TODO: Consider using unmount q
	cleanup, err := cc.getCleanupDirectories(remaining)

	cc.manager.rwlock.Unlock()

	if err != nil {
		return err
	}

	return cleanupAll(cc.ctx, cleanup, cc.manager.handlers)
}

func (cc *collectionContext) applyRemove() (map[uint64]struct{}, error) {
	remaining := map[uint64]struct{}{}
	v1bkt := cc.tx.Bucket([]byte("v1"))
	if v1bkt == nil {
		return remaining, nil
	}
	nsc := v1bkt.Cursor()
	for nsk, nsv := nsc.First(); nsk != nil; nsk, nsv = nsc.Next() {
		if nsv != nil {
			continue
		}
		removed := cc.removed[string(nsk)]
		nsbkt := v1bkt.Bucket(nsk)
		msbkt := nsbkt.Bucket(bucketKeyMounts)
		if msbkt == nil {
			continue
		}
		lsbkt := nsbkt.Bucket(bucketKeyLeases)
		msc := msbkt.Cursor()
		for msk, msv := msc.First(); msk != nil; msk, msv = msc.Next() {
			if msv != nil {
				continue
			}
			mbkt := msbkt.Bucket(msk)
			var remove bool
			if removed != nil {
				_, remove = removed[string(msk)]
			}

			if remove {
				if lsbkt != nil {
					lid := mbkt.Get(bucketKeyLease)
					if len(lid) > 0 {
						lbkt := lsbkt.Bucket(lid)
						if lbkt != nil {
							lbkt.Delete(msk)
							if k, _ := lbkt.Cursor().First(); k == nil {
								lsbkt.DeleteBucket([]byte(lid))
							}
						}
					}
				}
				msbkt.DeleteBucket(msk)
			} else {
				remaining[readID(mbkt)] = struct{}{}
			}
		}
	}

	return remaining, nil
}

func (cc *collectionContext) getCleanupDirectories(remaining map[uint64]struct{}) ([]string, error) {
	fd, err := os.Open(cc.manager.targets)
	if err != nil {
		return nil, err
	}
	defer fd.Close()

	dirs, err := fd.Readdirnames(0)
	if err != nil {
		return nil, err
	}

	cleanup := []string{}
	for _, d := range dirs {
		id, err := strconv.ParseUint(d, 10, 64)
		if err != nil {
			continue
		}
		if _, ok := remaining[id]; ok {
			continue
		}
		cleanup = append(cleanup, filepath.Join(cc.manager.targets, d))
	}

	return cleanup, nil
}

func cleanupAll(ctx context.Context, roots []string, handlers map[string]mount.MountHandler) error {
	var errs []error
	for _, root := range roots {
		if err := unmountAll(ctx, root, handlers); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func unmountAll(ctx context.Context, root string, handlers map[string]mount.MountHandler) error {
	fd, err := os.Open(root)
	if err != nil {
		return err
	}
	defer fd.Close()

	dirs, err := fd.Readdirnames(0)
	if err != nil {
		return err
	}

	var mountErrs error
	// TODO : Reverse order
	for _, d := range dirs {
		if strings.HasSuffix(d, ".type") {
			continue
		}

		p := filepath.Join(root, d)
		var h mount.MountHandler
		if b, rerr := os.ReadFile(p + ".type"); rerr == nil {
			h = handlers[string(b)]
		} else if !os.IsNotExist(rerr) {
			return rerr
		}
		if h != nil {
			err = h.Unmount(ctx, p)
		} else {
			err = mount.Unmount(p, 0)
		}
		if err != nil {
			mountErrs = errors.Join(mountErrs, fmt.Errorf("failure unmounting %s: %w", d, err))
		}
	}
	if mountErrs != nil {
		return mountErrs
	}
	return os.RemoveAll(root)
}