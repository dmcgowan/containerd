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
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/containerd/errdefs"
	"github.com/containerd/log"

	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/metadata"
	"github.com/containerd/containerd/v2/core/metadata/boltutil"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/internal/kmutex"
	"github.com/containerd/containerd/v2/pkg/gc"
	"github.com/containerd/containerd/v2/pkg/namespaces"
)

type BoltManager interface {
	mount.Manager
	metadata.Collector
	Sync(context.Context) error
}

type managerOptions struct {
	handlers map[string]mount.Handler
	roots    []*os.Root
}

type Opt func(*managerOptions) error

func WithMountHandler(name string, h mount.Handler) Opt {
	return func(o *managerOptions) error {
		if o.handlers == nil {
			o.handlers = make(map[string]mount.Handler)
		}
		o.handlers[name] = h
		return nil
	}
}

func WithAllowedRoot(root string) Opt {
	return func(o *managerOptions) error {
		r, err := os.OpenRoot(root)
		if err != nil {
			return err
		}
		o.roots = append(o.roots, r)
		return nil
	}
}

func NewManager(db *bolt.DB, targetDir string, opts ...Opt) (mount.Manager, error) {
	options := managerOptions{}
	for _, o := range opts {
		if err := o(&options); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(targetDir, 0700); err != nil {
		return nil, err
	}
	tr, err := os.OpenRoot(targetDir)
	if err != nil {
		return nil, fmt.Errorf("failed to open target root %q: %w", targetDir, err)
	}
	// Mount points are owned by backing mounts rather than by the
	// activation which created them, since a backing mount may back
	// several activations.
	if err := tr.Mkdir(backingDir, 0700); err != nil && !os.IsExist(err) {
		tr.Close()
		return nil, fmt.Errorf("failed to create backing dir under %q: %w", targetDir, err)
	}
	rootMap := map[string]*os.Root{
		tr.Name(): tr,
	}
	for _, r := range options.roots {
		rootMap[r.Name()] = r
	}

	return &mountManager{
		db:       db,
		targets:  tr,
		handlers: options.handlers,
		rootMap:  rootMap,
		activate: kmutex.New(),
		mounting: kmutex.New(),
	}, nil
}

type mountManager struct {
	db       *bolt.DB
	targets  *os.Root
	handlers map[string]mount.Handler
	rootMap  map[string]*os.Root

	rwlock   sync.RWMutex
	activate kmutex.KeyedLocker
	// mounting serializes activations which resolve to the same
	// mount, keyed by mount identity, so that only one of them
	// performs the underlying mount.
	mounting kmutex.KeyedLocker
}

func (mm *mountManager) Close() error {
	var errs []error
	for _, r := range mm.rootMap {
		if err := r.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	errs = append(errs, mm.db.Close())
	return errors.Join(errs...)
}

func (mm *mountManager) Activate(ctx context.Context, name string, mounts []mount.Mount, opts ...mount.ActivateOpt) (info mount.ActivationInfo, retErr error) {
	namespace, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return mount.ActivationInfo{}, err
	}

	// Serialize concurrent activations of the same name to prevent a
	// racing Activate from misidentifying an in-progress activation as
	// a stale record and destroying it.
	if err := mm.activate.Lock(ctx, name); err != nil {
		return mount.ActivationInfo{}, err
	}
	defer mm.activate.Unlock(name)

	log.G(ctx).WithField("name", name).WithField("mounts", mounts).Debugf("activating mount")

	lid, leased := leases.FromContext(ctx)

	var config mount.ActivateOptions
	for _, opt := range opts {
		opt(&config)
	}

	// Transformation rewrites mounts in place, don't mutate the
	// caller's slice.
	if len(mounts) > 0 {
		local := make([]mount.Mount, len(mounts))
		copy(local, mounts)
		mounts = local
	}

	shouldTransform := func(p string, t string) bool {
		p = p + "/*"
		for _, mt := range config.AllowMountTypes {
			if mt == p || mt == t {
				return false
			}
		}
		return true
	}

	shouldHandle := func(t string) bool {
		return !slices.Contains(config.AllowMountTypes, t)
	}

	transforms := map[string]mount.Transformer{
		"format": mountFormatter{},
		"mkfs": &mkfs{
			rootMap: mm.rootMap,
		},
		"mkdir": &mkdir{
			rootMap: mm.rootMap,
		},
	}

	start := time.Now()
	// highest index of a mount
	// first system mount is the first index which should be mounted by the system
	var firstSystemMount = -1
	var mountConv [][]mount.Transformer
	var handlers []mount.Handler
	for i := range mounts {
		mountType := mounts[i].Type

		// Check is the source needs transformation, any transform operation requires
		// mounting with the mount manager.
		for transformType, mt, ok := strings.Cut(mountType, "/"); ok; transformType, mt, ok = strings.Cut(mountType, "/") {
			if tr, ok := transforms[transformType]; ok {
				if shouldTransform(transformType, mounts[i].Type) {
					// At least everything before this must be mounted
					// by the mount manager
					firstSystemMount = i
				}

				if handlers == nil {
					handlers = make([]mount.Handler, len(mounts))
				}

				if mountConv == nil {
					mountConv = make([][]mount.Transformer, len(mounts))
				}

				mountConv[i] = append(mountConv[i], typeTransformer{
					Transformer: tr,
					mountType:   mt,
				})

				mountType = mt
			} else {
				log.G(ctx).Warnf("unknown transform %q for mount %v", transformType, mounts[i])
				break
			}
		}

		var handler mount.Handler
		if mm.handlers != nil {
			handler = mm.handlers[mountType]
		}

		if handler != nil || config.Temporary {
			if handlers == nil {
				handlers = make([]mount.Handler, len(mounts))
			}
			handlers[i] = handler
			if shouldHandle(mountType) || config.Temporary {
				firstSystemMount = i + 1
			}
		}
	}
	// If no mounts are handled here, return not implemented and caller
	// may just perform system mounts as normal.
	if firstSystemMount == -1 {
		return mount.ActivationInfo{}, errdefs.ErrNotImplemented
	}
	if firstSystemMount > 255 {
		return mount.ActivationInfo{}, fmt.Errorf("too many mounts (%d): maximum 255: %w", firstSystemMount, errdefs.ErrInvalidArgument)
	}

	// Get read lock to block GC context from starting
	mm.rwlock.RLock()
	defer mm.rwlock.RUnlock()

	var (
		mid uint64
		// Backing mounts whose last reference was dropped while
		// replacing a stale record; unmounted once the transaction
		// commits.
		staleBacking []backingMount
	)

	if err := mm.db.Update(func(tx *bolt.Tx) error {
		if err := migrateFromV1(tx); err != nil {
			return fmt.Errorf("failed to migrate mount database: %w", err)
		}

		v1bkt, err := tx.CreateBucketIfNotExists(bucketKeyVersion)
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
			existing := mbkt.Bucket([]byte(name))
			if existing == nil {
				return err
			}
			// If the mount is fully activated, return already exists
			// so the caller can reuse the existing mount.
			if len(existing.Get(bucketKeyComplete)) > 0 {
				return fmt.Errorf("mount %q: %w", name, errdefs.ErrAlreadyExists)
			}
			// The mount bucket exists but was never fully activated
			// (e.g., process crashed between creating the bucket and
			// completing activation). Release whatever it had already
			// claimed and proceed with a fresh activation.
			if lid := existing.Get(bucketKeyLease); len(lid) > 0 {
				if lsbkt := nsbkt.Bucket(bucketKeyLeases); lsbkt != nil {
					if lbkt := lsbkt.Bucket(lid); lbkt != nil {
						if err := lbkt.Delete([]byte(name)); err != nil {
							return err
						}
					}
				}
			}
			staleBacking, err = releaseBackingMounts(tx, namespace, name, activationBackingIDs(existing))
			if err != nil {
				return err
			}
			if err := mbkt.DeleteBucket([]byte(name)); err != nil {
				return err
			}
			bkt, err = mbkt.CreateBucket([]byte(name))
			if err != nil {
				return err
			}
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

		if err := boltutil.WriteLabels(bkt, config.Labels); err != nil {
			return err
		}

		if err := boltutil.WriteTimestamps(bkt, start, start); err != nil {
			return err
		}

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

	if len(staleBacking) > 0 {
		if err := mm.unmountBackingMounts(ctx, staleBacking); err != nil {
			log.G(ctx).WithError(err).WithField("name", name).Warn("failed to clean up stale activation mounts")
		}
	}

	defer func() {
		// If error, rollback and remove by name. Releasing the
		// claimed backing mounts reference counts them down; only
		// those which nothing else is using are unmounted.
		if retErr != nil {
			var orphaned []backingMount
			if err := mm.db.Update(func(tx *bolt.Tx) error {
				nsbkt := getBucket(tx, bucketKeyVersion, []byte(namespace))
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
							if k, _ := lbkt.Cursor().First(); k == nil {
								lsbkt.DeleteBucket([]byte(lid))
							}
						}
					}
				}

				// Claims are recorded as they are made, so the
				// record covers mounts claimed but not completed.
				bkt := mbkt.Bucket([]byte(name))
				if bkt == nil {
					return nil
				}

				var err error
				orphaned, err = releaseBackingMounts(tx, namespace, name, activationBackingIDs(bkt))
				if err != nil {
					return err
				}

				return mbkt.DeleteBucket([]byte(name))
			}); err != nil {
				log.G(ctx).WithError(err).WithField("name", name).Errorf("failed to rollback")
			}
			if err := mm.unmountBackingMounts(ctx, orphaned); err != nil {
				log.G(ctx).WithError(err).WithField("name", name).Error("failed to cleanup mounts after failed activation")
			}
		}
	}()

	var mounted []mount.ActiveMount

	for i, m := range mounts[:firstSystemMount] {
		if mountConv != nil && mountConv[i] != nil {
			for _, tr := range mountConv[i] {
				newM, err := tr.Transform(ctx, m, mounted)
				if err != nil {
					return mount.ActivationInfo{}, err
				}
				m = newM
			}
			mounts[i] = m
		}

		backing, err := mm.activateBackingMount(ctx, namespace, name, i, m, handlers[i], mounted)
		if err != nil {
			return mount.ActivationInfo{}, err
		}
		mounted = append(mounted, backing.active())
	}

	// If first system mount is converted, fill in the format. There is
	// no system mount to convert when every mount was handled here.
	if mountConv != nil && firstSystemMount < len(mounts) {
		for _, tr := range mountConv[firstSystemMount] {
			newM, err := tr.Transform(ctx, mounts[firstSystemMount], mounted)
			if err != nil {
				return mount.ActivationInfo{}, err
			}
			mounts[firstSystemMount] = newM
		}
	}
	// If no system mounts, add a bind mount if temporary
	// TODO: Add config for whether to add the bind mount?
	if config.Temporary && firstSystemMount > 0 {
		mounts = append(mounts, mount.Mount{
			Type:    "bind",
			Source:  mounted[firstSystemMount-1].MountPoint,
			Options: []string{"rbind"},
		})
	}

	info.Name = name
	info.Active = mounted
	info.System = mounts[firstSystemMount:]
	info.Labels = config.Labels

	// Open another write transaction and update state, or another way to update state?
	if err := mm.db.Update(func(tx *bolt.Tx) error {
		bkt := getBucket(tx, bucketKeyVersion, []byte(namespace), bucketKeyMounts, []byte(name))
		if bkt == nil {
			return fmt.Errorf("missing mount %q bucket: %w", name, errdefs.ErrUnknown)
		}

		// The chain is recorded as it is mounted, all that remains is
		// to mark the activation complete.
		if err := bkt.Put(bucketKeyComplete, []byte{1}); err != nil {
			return err
		}

		if err := boltutil.WriteTimestamps(bkt, start, time.Now()); err != nil {
			return err
		}

		if len(info.System) > 0 {
			if len(info.System) > 255 {
				return fmt.Errorf("too many system mounts (%d): maximum 255", len(info.System))
			}
			sbkt, err := bkt.CreateBucket(bucketKeySystem)
			if err != nil {
				return err
			}
			for i, sm := range info.System {
				cur, err := sbkt.CreateBucket([]byte{byte(i)})
				if err != nil {
					return err
				}
				if err = putSystemMount(cur, sm); err != nil {
					return err
				}
			}
		}

		return nil
	}); err != nil {
		return mount.ActivationInfo{}, err
	}

	return
}

// activateBackingMount resolves a single mount in a chain to a backing
// mount, performing the underlying mount when this is the first
// reference to it.
//
// Identical mounts within a namespace resolve to the same backing
// mount, so a mount which appears in several chains is mounted once and
// stays mounted until the last chain releases it. The mount identity is
// locked for the duration so that concurrent activations of the same
// mount cannot both decide to mount it.
func (mm *mountManager) activateBackingMount(ctx context.Context, namespace, name string, index int, m mount.Mount, handler mount.Handler, mounted []mount.ActiveMount) (backingMount, error) {
	if shareable(m) {
		key := mountingKey(m)
		if err := mm.mounting.Lock(ctx, key); err != nil {
			return backingMount{}, err
		}
		defer mm.mounting.Unlock(key)
	}

	var backing backingMount
	if err := mm.db.Update(func(tx *bolt.Tx) error {
		var err error
		backing, err = claimBackingMount(tx, namespace, name, index, m)
		return err
	}); err != nil {
		return backingMount{}, err
	}

	if backing.point != "" {
		// Already mounted for another chain.
		log.G(ctx).WithFields(log.Fields{
			"name":       name,
			"backing":    backing.id,
			"mountpoint": backing.point,
		}).Debug("reusing backing mount")
		return backing, nil
	}

	mp, err := mm.prepareBackingDir(backing.id, m.Type, handler == nil)
	if err != nil {
		return backingMount{}, err
	}

	var active mount.ActiveMount
	if handler != nil {
		active, err = handler.Mount(ctx, m, mp, mounted)
		if err != nil {
			return backingMount{}, fmt.Errorf("mount handler failed %v: %w", m, err)
		}
	} else {
		if err := m.Mount(mp); err != nil {
			return backingMount{}, fmt.Errorf("mount failed %v: %w", m, err)
		}
		now := time.Now()
		active = mount.ActiveMount{
			Mount:      m,
			MountPoint: mp,
			MountedAt:  &now,
		}
	}
	if active.MountedAt == nil {
		now := time.Now()
		active.MountedAt = &now
	}

	if err := mm.db.Update(func(tx *bolt.Tx) error {
		return completeBackingMount(tx, namespace, backing.id, active.MountPoint, *active.MountedAt)
	}); err != nil {
		return backingMount{}, err
	}

	backing.point = active.MountPoint
	backing.at = active.MountedAt
	return backing, nil
}

// prepareBackingDir creates the directory a backing mount is mounted
// into, recording the mount type alongside it so that a directory left
// behind by an unclean shutdown can still be unmounted with the
// correct handler.
//
// The mount point itself is only created when the mount is performed
// directly. Handlers decide what belongs at the path they are given,
// which is not always a directory: the loopback handler, for example,
// puts a symlink to the loop device there.
func (mm *mountManager) prepareBackingDir(id uint64, mountType string, createMountPoint bool) (string, error) {
	dir := filepath.Join(backingDir, strconv.FormatUint(id, 10))
	if err := mm.targets.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return "", fmt.Errorf("failed to create backing mount dir: %w", err)
	}
	// TODO: Go 1.25 use mm.targets.WriteFile
	if err := os.WriteFile(filepath.Join(mm.targets.Name(), dir, typeFileName), []byte(mountType), 0600); err != nil {
		return "", err
	}
	if createMountPoint {
		if err := mm.targets.Mkdir(filepath.Join(dir, mountPointName), 0700); err != nil && !os.IsExist(err) {
			return "", fmt.Errorf("failed to create mount point: %w", err)
		}
	}
	return filepath.Join(mm.targets.Name(), dir, mountPointName), nil
}

// alreadyUnmounted reports whether an unmount error means there was
// nothing mounted at the path, which is the desired end state. This
// happens for backing mounts left behind by a crash between creating
// the mount point and completing the mount.
func alreadyUnmounted(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTDIR)
}

// unmountBackingMounts unmounts released backing mounts and removes
// their directories. They are unmounted in the order returned by
// releaseBackingMounts, which places dependent mounts before the mounts
// they were built on.
func (mm *mountManager) unmountBackingMounts(ctx context.Context, backing []backingMount) error {
	var errs []error
	for _, b := range backing {
		if b.point == "" {
			// Never completed, nothing is mounted.
			if err := os.RemoveAll(mm.backingRoot(b.id)); err != nil && !os.IsNotExist(err) {
				log.G(ctx).WithError(err).WithField("backing", b.id).Warn("failed to remove backing mount dir")
			}
			continue
		}
		var err error
		if h := mm.handlers[b.mount.Type]; h != nil {
			err = h.Unmount(ctx, b.point)
		} else if err = mount.Unmount(b.point, 0); alreadyUnmounted(err) {
			err = nil
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to unmount %q: %w", b.point, err))
			continue
		}
		if err := os.RemoveAll(mm.backingRoot(b.id)); err != nil && !os.IsNotExist(err) {
			log.G(ctx).WithError(err).WithField("backing", b.id).Warn("failed to remove backing mount dir")
		}
	}
	return errors.Join(errs...)
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

func putSystemMount(bkt *bolt.Bucket, m mount.Mount) error {
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

func readSystemMount(bkt *bolt.Bucket) mount.Mount {
	m := mount.Mount{
		Type:   string(bkt.Get(bucketKeyType)),
		Source: string(bkt.Get(bucketKeySource)),
		Target: string(bkt.Get(bucketKeyTarget)),
	}
	if v := bkt.Get(bucketKeyOptions); len(v) > 0 {
		m.Options = strings.Split(string(v), "\x00")
	}
	return m
}

// readActivationInfo builds the activation info for a mount, resolving
// the backing mount for each position in its chain. nsbkt is the
// namespace bucket holding the backing mount records.
func readActivationInfo(nsbkt *bolt.Bucket, name string, bkt *bolt.Bucket) (mount.ActivationInfo, error) {
	info := mount.ActivationInfo{
		Name: name,
	}
	if abkt := bkt.Bucket(bucketKeyActive); abkt != nil {
		if err := abkt.ForEachBucket(func(k []byte) error {
			key := abkt.Bucket(k).Get(bucketKeyBackedBy)
			if len(key) == 0 {
				return nil
			}
			b, ok, err := getBackingMount(nsbkt, key)
			if err != nil {
				return err
			}
			if !ok {
				// An activation interrupted before it completed can
				// reference a backing mount which a later activation
				// of the same mount replaced. Nothing is mounted for
				// it, so report what is rather than failing the
				// whole listing.
				return nil
			}
			info.Active = append(info.Active, b.active())
			return nil
		}); err != nil {
			return mount.ActivationInfo{}, err
		}
	}
	if sbkt := bkt.Bucket(bucketKeySystem); sbkt != nil {
		if err := sbkt.ForEachBucket(func(k []byte) error {
			info.System = append(info.System, readSystemMount(sbkt.Bucket(k)))
			return nil
		}); err != nil {
			return mount.ActivationInfo{}, err
		}
	}
	lbls, err := boltutil.ReadLabels(bkt)
	if err != nil {
		return mount.ActivationInfo{}, err
	}
	info.Labels = lbls

	return info, nil
}

func getBucket(tx *bolt.Tx, keys ...[]byte) *bolt.Bucket {
	bkt := tx.Bucket(keys[0])
	if bkt == nil {
		return nil
	}

	return getSubBucket(bkt, keys[1:]...)
}

func getSubBucket(bkt *bolt.Bucket, keys ...[]byte) *bolt.Bucket {
	for _, key := range keys {
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

	// Get read lock to block GC context from starting
	mm.rwlock.RLock()
	defer mm.rwlock.RUnlock()

	var released []backingMount

	// First in a single transaction, drop the activation and release
	// its references. Only the mounts which nothing else references
	// come back for unmounting.
	if err := mm.db.Update(func(tx *bolt.Tx) error {
		if err := migrateFromV1(tx); err != nil {
			return fmt.Errorf("failed to migrate mount database: %w", err)
		}

		nsbkt := getBucket(tx, bucketKeyVersion, []byte(namespace))
		if nsbkt == nil {
			return fmt.Errorf("missing namespace %q bucket: %w", namespace, errdefs.ErrNotFound)
		}

		mbkt := nsbkt.Bucket(bucketKeyMounts)
		if mbkt == nil {
			return fmt.Errorf("missing mounts bucket: %w", errdefs.ErrNotFound)
		}
		bkt := mbkt.Bucket([]byte(name))
		if bkt == nil {
			return fmt.Errorf("missing mount %q bucket: %w", name, errdefs.ErrNotFound)
		}

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

		released, err = releaseBackingMounts(tx, namespace, name, activationBackingIDs(bkt))
		if err != nil {
			return err
		}

		return mbkt.DeleteBucket([]byte(name))
	}); err != nil {
		return err
	}

	// TODO: Should this also be backgrounded, not much can be done on failure to unmount
	if err := mm.unmountBackingMounts(ctx, released); err != nil {
		// Don't try to cleanup, GC will need to do the rest
		return err
	}

	return nil
}

func (mm *mountManager) Info(ctx context.Context, name string) (mount.ActivationInfo, error) {
	namespace, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return mount.ActivationInfo{}, err
	}

	var info mount.ActivationInfo
	err = mm.db.View(func(tx *bolt.Tx) error {
		if err := checkNotV1(tx); err != nil {
			return err
		}
		var err error
		info, err = infoFromTx(tx, namespace, name)
		return err
	})
	if errors.Is(err, errNeedsMigration) {
		// A read that arrives before anything else has triggered
		// migration must not silently report an activation as missing
		// when it is only sitting in v1. Escalate to a write
		// transaction, which migrates and then reads within the same
		// transaction.
		err = mm.db.Update(func(tx *bolt.Tx) error {
			if err := migrateFromV1(tx); err != nil {
				return fmt.Errorf("failed to migrate mount database: %w", err)
			}
			var err error
			info, err = infoFromTx(tx, namespace, name)
			return err
		})
	}
	if err != nil {
		return mount.ActivationInfo{}, err
	}
	return info, nil
}

// infoFromTx reads a single activation's info from an already open
// transaction, read only or writable.
func infoFromTx(tx *bolt.Tx, namespace, name string) (mount.ActivationInfo, error) {
	nsbkt := getBucket(tx, bucketKeyVersion, []byte(namespace))
	if nsbkt == nil {
		return mount.ActivationInfo{}, fmt.Errorf("mount %q %w", name, errdefs.ErrNotFound)
	}
	bkt := getSubBucket(nsbkt, bucketKeyMounts, []byte(name))
	if bkt == nil {
		return mount.ActivationInfo{}, fmt.Errorf("mount %q %w", name, errdefs.ErrNotFound)
	}
	return readActivationInfo(nsbkt, name, bkt)
}

func (mm *mountManager) Update(context.Context, mount.ActivationInfo, ...string) (mount.ActivationInfo, error) {
	return mount.ActivationInfo{}, errdefs.ErrNotImplemented
}

func (mm *mountManager) List(ctx context.Context, filters ...string) ([]mount.ActivationInfo, error) {
	namespace, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return nil, err
	}

	var infos []mount.ActivationInfo
	err = mm.db.View(func(tx *bolt.Tx) error {
		if err := checkNotV1(tx); err != nil {
			return err
		}
		var err error
		infos, err = listFromTx(tx, namespace)
		return err
	})
	if errors.Is(err, errNeedsMigration) {
		err = mm.db.Update(func(tx *bolt.Tx) error {
			if err := migrateFromV1(tx); err != nil {
				return fmt.Errorf("failed to migrate mount database: %w", err)
			}
			var err error
			infos, err = listFromTx(tx, namespace)
			return err
		})
	}
	if err != nil {
		return nil, err
	}
	return infos, nil
}

// listFromTx reads every activation in a namespace from an already
// open transaction, read only or writable.
func listFromTx(tx *bolt.Tx, namespace string) ([]mount.ActivationInfo, error) {
	nsbkt := getBucket(tx, bucketKeyVersion, []byte(namespace))
	if nsbkt == nil {
		return nil, nil
	}
	mbkt := nsbkt.Bucket(bucketKeyMounts)
	if mbkt == nil {
		return nil, nil
	}

	var infos []mount.ActivationInfo
	if err := mbkt.ForEachBucket(func(k []byte) error {
		info, err := readActivationInfo(nsbkt, string(k), mbkt.Bucket(k))
		if err != nil {
			return err
		}
		infos = append(infos, info)
		return nil
	}); err != nil {
		return nil, err
	}
	return infos, nil
}

func (mm *mountManager) StartCollection(ctx context.Context) (metadata.CollectionContext, error) {
	// lock now and collection will unlock on cancel or finish
	mm.rwlock.Lock()

	tx, err := mm.db.Begin(true)
	if err != nil {
		mm.rwlock.Unlock()
		return nil, err
	}

	// A collection which runs before anything else has migrated would
	// otherwise see none of the mounts a v1 database still describes
	// and treat them all as orphaned.
	if err := migrateFromV1(tx); err != nil {
		tx.Rollback()
		mm.rwlock.Unlock()
		return nil, fmt.Errorf("failed to migrate mount database: %w", err)
	}

	return &collectionContext{
		ctx:     ctx,
		tx:      tx,
		manager: mm,
		removed: map[string]map[string]struct{}{},
	}, nil
}

func (mm *mountManager) ReferenceLabel() string {
	return "mount"
}

type collectionContext struct {
	ctx     context.Context
	tx      *bolt.Tx
	manager *mountManager
	removed map[string]map[string]struct{}

	// Backing mounts whose last reference was released during
	// applyRemove; they need unmounting after the transaction
	// commits.
	released []backingMount
}

func (cc *collectionContext) All(fn func(gc.Node)) {
	v1bkt := cc.tx.Bucket(bucketKeyVersion)
	if v1bkt == nil {
		return
	}
	nsc := v1bkt.Cursor()
	for nsk, nsv := nsc.First(); nsk != nil; nsk, nsv = nsc.Next() {
		if nsv != nil {
			continue
		}
		mntsbkt := v1bkt.Bucket(nsk).Bucket(bucketKeyMounts)
		if mntsbkt == nil {
			continue
		}
		mc := mntsbkt.Cursor()
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

func gcnode(t gc.ResourceType, ns, key string) gc.Node {
	return gc.Node{
		Type:      t,
		Namespace: ns,
		Key:       key,
	}
}

func (cc *collectionContext) ActiveWithBackRefs(ns string, fn func(gc.Node), bref func(gc.Node, gc.Node)) {
	nsbkt := getBucket(cc.tx, bucketKeyVersion, []byte(ns), bucketKeyMounts)
	if nsbkt != nil {
		mc := nsbkt.Cursor()
		for mk, mv := mc.First(); mk != nil; mk, mv = mc.Next() {
			if mv != nil {
				continue
			}
			n := gcnode(metadata.ResourceMount, ns, string(mk))
			lbkt := nsbkt.Bucket(mk).Bucket(bucketKeyLabels)
			if lbkt != nil {
				lc := lbkt.Cursor()
				for _, h := range []struct {
					key     []byte
					handler func([]byte, []byte)
				}{
					{
						key: labelGCContainerBackRef,
						handler: func(k, v []byte) {
							if ks := string(k); ks != string(labelGCContainerBackRef) {
								// Allow reference naming separated by . or /, ignore names
								if ks[len(labelGCContainerBackRef)] != '.' && ks[len(labelGCContainerBackRef)] != '/' {
									return
								}
							}

							bref(gcnode(metadata.ResourceContainer, ns, string(v)), n)
						},
					},
					{
						key: labelGCContentBackRef,
						handler: func(k, v []byte) {
							if ks := string(k); ks != string(labelGCContentBackRef) {
								// Allow reference naming separated by . or /, ignore names
								if ks[len(labelGCContentBackRef)] != '.' && ks[len(labelGCContentBackRef)] != '/' {
									return
								}
							}

							bref(gcnode(metadata.ResourceContent, ns, string(v)), n)
						},
					},
					{
						key: labelGCImageBackRef,
						handler: func(k, v []byte) {
							if ks := string(k); ks != string(labelGCImageBackRef) {
								// Allow reference naming separated by . or /, ignore names
								if ks[len(labelGCImageBackRef)] != '.' && ks[len(labelGCImageBackRef)] != '/' {
									return
								}
							}

							bref(gcnode(metadata.ResourceImage, ns, string(v)), n)
						},
					},
					{
						key: labelGCSnapBackRef,
						handler: func(k, v []byte) {
							snapshotter := k[len(labelGCSnapBackRef):]
							if i := bytes.IndexByte(snapshotter, '/'); i >= 0 {
								snapshotter = snapshotter[:i]
							}
							bref(gcnode(metadata.ResourceSnapshot, ns, fmt.Sprintf("%s/%s", snapshotter, v)), n)
						},
					},
					// TODO: Consider support for root/expire labels
				} {
					for k, v := lc.Seek(h.key); k != nil && bytes.HasPrefix(k, h.key); k, v = lc.Next() {
						h.handler(k, v)
					}
				}
			}
		}
	}
}

func (cc *collectionContext) Active(ns string, fn func(gc.Node)) {
	cc.ActiveWithBackRefs(ns, fn, func(gc.Node, gc.Node) {})
}

func (cc *collectionContext) Leased(ns, lease string, fn func(gc.Node)) {
	bkt := getBucket(cc.tx, bucketKeyVersion, []byte(ns), []byte("leases"), []byte(lease))
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
	log.G(cc.ctx).WithFields(log.Fields{"namespace": n.Namespace, "name": n.Key}).Debugf("remove mount")
	if n.Type != metadata.ResourceMount {
		return
	}
	nmap, ok := cc.removed[n.Namespace]
	if ok {
		if _, ok = nmap[n.Key]; !ok {
			nmap[n.Key] = struct{}{}
		}
	} else {
		cc.removed[n.Namespace] = map[string]struct{}{
			n.Key: {},
		}
	}
}

func (cc *collectionContext) Cancel() (err error) {
	err = cc.tx.Rollback()
	cc.manager.rwlock.Unlock()
	return
}

func (cc *collectionContext) Finish() error {
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

	// Backing mounts released above are unmounted from their database
	// records, exclude them from the orphan scan so they are not
	// unmounted twice.
	for _, b := range cc.released {
		remaining[b.id] = struct{}{}
	}

	// TODO: Consider using unmount q
	orphaned, err := cc.orphanBackingMounts(remaining)

	cc.manager.rwlock.Unlock()

	if err != nil {
		return err
	}

	var errs []error
	if err := cc.manager.unmountBackingMounts(cc.ctx, cc.released); err != nil {
		errs = append(errs, err)
	}
	if err := cc.manager.unmountBackingMounts(cc.ctx, orphaned); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// applyRemove deletes the activations marked for removal and releases
// the references they held on their backing mounts. It returns the set
// of backing mount ids which are still referenced.
func (cc *collectionContext) applyRemove() (map[uint64]struct{}, error) {
	remaining := map[uint64]struct{}{}
	v1bkt := cc.tx.Bucket(bucketKeyVersion)
	if v1bkt == nil {
		return remaining, nil
	}
	nsc := v1bkt.Cursor()
	for nsk, nsv := nsc.First(); nsk != nil; nsk, nsv = nsc.Next() {
		if nsv != nil {
			continue
		}
		namespace := string(nsk)
		removed := cc.removed[namespace]
		nsbkt := v1bkt.Bucket(nsk)
		msbkt := nsbkt.Bucket(bucketKeyMounts)
		if msbkt != nil {
			lsbkt := nsbkt.Bucket(bucketKeyLeases)
			// Collect first: releasing backing mounts writes to
			// sibling buckets, which must not happen while a cursor is
			// open over the mounts bucket.
			var remove [][]byte
			msc := msbkt.Cursor()
			for msk, msv := msc.First(); msk != nil; msk, msv = msc.Next() {
				if msv != nil {
					continue
				}
				if removed != nil {
					if _, ok := removed[string(msk)]; ok {
						remove = append(remove, bytes.Clone(msk))
					}
				}
			}

			for _, msk := range remove {
				mbkt := msbkt.Bucket(msk)
				if mbkt == nil {
					continue
				}
				if lsbkt != nil {
					lid := mbkt.Get(bucketKeyLease)
					if len(lid) > 0 {
						lbkt := lsbkt.Bucket(lid)
						if lbkt != nil {
							lbkt.Delete(msk)
							if k, _ := lbkt.Cursor().First(); k == nil {
								lsbkt.DeleteBucket(lid)
							}
						}
					}
				}
				released, err := releaseBackingMounts(cc.tx, namespace, string(msk), activationBackingIDs(mbkt))
				if err != nil {
					return nil, err
				}
				cc.released = append(cc.released, released...)
				if err := msbkt.DeleteBucket(msk); err != nil {
					return nil, err
				}
			}
		}

		// Everything still in the backing bucket is either referenced
		// by a surviving activation or is an in-flight activation
		// which has not recorded its mounts yet.
		if bbkt := nsbkt.Bucket(bucketKeyBacking); bbkt != nil {
			bc := bbkt.Cursor()
			for bk, bv := bc.First(); bk != nil; bk, bv = bc.Next() {
				if bv != nil {
					continue
				}
				id, _ := binary.Uvarint(bk)
				remaining[id] = struct{}{}
			}
		}
	}

	sortBackingUnmountOrder(cc.released)

	return remaining, nil
}

// orphanBackingMounts returns backing mounts whose directory is still
// present under the target root but which no longer have a database
// record, for example because the process died between mounting and
// recording the mount. They are reconstructed from the type file
// written before mounting so the correct handler is used to unmount
// them.
func (cc *collectionContext) orphanBackingMounts(remaining map[uint64]struct{}) ([]backingMount, error) {
	root := filepath.Join(cc.manager.targets.Name(), backingDir)
	fd, err := os.Open(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer fd.Close()

	dirs, err := fd.Readdirnames(0)
	if err != nil {
		return nil, err
	}

	var orphaned []backingMount
	for _, d := range dirs {
		id, err := strconv.ParseUint(d, 10, 64)
		if err != nil {
			continue
		}
		if _, ok := remaining[id]; ok {
			continue
		}
		b := backingMount{
			id:    id,
			point: filepath.Join(root, d, mountPointName),
		}
		if bs, err := os.ReadFile(filepath.Join(root, d, typeFileName)); err == nil {
			b.mount.Type = string(bs)
		} else if !os.IsNotExist(err) {
			return nil, err
		} else {
			log.G(cc.ctx).WithField("backing", id).Info("missing type file, attempting unmount with no handler")
		}
		orphaned = append(orphaned, b)
	}

	sortBackingUnmountOrder(orphaned)

	return orphaned, nil
}
