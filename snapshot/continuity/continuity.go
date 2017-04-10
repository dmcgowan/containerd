package continuity

import (
	"bytes"
	"context"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/plugin"
	"github.com/containerd/containerd/snapshot"
	"github.com/containerd/containerd/snapshot/storage"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
	"github.com/stevvooe/continuity"
)

func init() {
	plugin.Register("snapshot-continuity", &plugin.Registration{
		Type: plugin.SnapshotPlugin,
		Init: func(ic *plugin.InitContext) (interface{}, error) {
			return NewSnapshotter(filepath.Join(ic.Root, "snapshot", "continuity"))
		},
	})
}

type snapshotter struct {
	root string
	ms   *storage.MetaStore
	cs   *content.Store
}

// NewSnapshotter returns a Snapshotter which checkouts active
// snapshots from a blob store and stores committed snapshots
// using a continuity manifest.
func NewSnapshotter(root string) (snapshot.Snapshotter, error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	ms, err := storage.NewMetaStore(filepath.Join(root, "metadata.db"))
	if err != nil {
		return nil, err
	}

	for _, d := range []string{"snapshots", "blobs", "manifests"} {
		if err := os.Mkdir(filepath.Join(root, d), 0700); err != nil && !os.IsExist(err) {
			return nil, err
		}
	}

	store, err := content.NewStore(filepath.Join(root, "blobs"))
	if err != nil {
		return nil, err
	}

	return &snapshotter{
		root: root,
		ms:   ms,
		cs:   store,
	}, nil
}

// Stat returns the info for an active or committed snapshot by name or
// key.
//
// Should be used for parent resolution, existence checks and to discern
// the kind of snapshot.
func (o *snapshotter) Stat(ctx context.Context, key string) (snapshot.Info, error) {
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		return snapshot.Info{}, err
	}
	defer t.Rollback()
	return storage.GetInfo(ctx, key)
}

func (o *snapshotter) Prepare(ctx context.Context, key, parent string) ([]containerd.Mount, error) {
	return o.createActive(ctx, key, parent, false)
}

func (o *snapshotter) View(ctx context.Context, key, parent string) ([]containerd.Mount, error) {
	return o.createActive(ctx, key, parent, true)
}

// Mounts returns the mounts for the transaction identified by key. Can be
// called on an read-write or readonly transaction.
//
// This can be used to recover mounts after calling View or Prepare.
func (o *snapshotter) Mounts(ctx context.Context, key string) ([]containerd.Mount, error) {
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		return nil, err
	}
	active, err := storage.GetActive(ctx, key)
	t.Rollback()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get active mount")
	}
	return o.mounts(active), nil
}

func (o *snapshotter) Commit(ctx context.Context, name, key string) error {
	ctx, t, err := o.ms.TransactionContext(ctx, true)
	if err != nil {
		return err
	}

	defer func() {
		if err != nil {
			if rerr := t.Rollback(); rerr != nil {
				log.G(ctx).WithError(rerr).Warn("Failure rolling back transaction")
			}
		}
	}()

	id, err := storage.CommitActive(ctx, key, name)
	if err != nil {
		return errors.Wrap(err, "failed to commit snapshot")
	}
	co := continuity.ContextOptions{
		Digester: contentStore{o.cs, ctx},
	}
	snapshotDir := o.getSnapshotDir(id)
	mctx, err := continuity.NewContextWithOptions(snapshotDir, co)
	if err != nil {
		return errors.Wrap(err, "failed to create continuity context")
	}

	m, err := continuity.BuildManifest(mctx)
	if err != nil {
		return errors.Wrap(err, "failed to create manifest")
	}

	// Marshal, store
	b, err := continuity.Marshal(m)
	if err != nil {
		return errors.Wrap(err, "failed to marshal manifest")
	}
	dgst, err := co.Digester.Digest(bytes.NewReader(b))
	if err != nil {
		return errors.Wrap(err, "failed to store manifest")
	}

	// Move snapshot directory
	renamed := filepath.Join(o.root, "snapshots", "commit-"+id)
	if err = os.Rename(snapshotDir, renamed); err != nil {
		return errors.Wrap(err, "failed to rename snapshot directory")
	}
	defer func() {
		if err != nil {
			if rerr := os.Rename(renamed, snapshotDir); rerr != nil {
				log.G(ctx).WithError(rerr).Errorf("Failure restoring snapshot directory %s", snapshotDir)
			}
		}
	}()

	manifest := filepath.Join(o.root, "manifests", id)
	if err := ioutil.WriteFile(manifest, []byte(dgst.String()), 0400); err != nil {
		return errors.Wrap(err, "failed to write manifest link")
	}

	if err = t.Commit(); err != nil {
		if derr := os.Remove(manifest); derr != nil {
			log.G(ctx).WithError(derr).Warn("Failure removing manifest link")
		}
		return err
	}

	if rerr := os.RemoveAll(renamed); rerr != nil {
		log.G(ctx).WithError(rerr).Warnf("Failure removing snapshot directory %s", renamed)
	}

	return nil
}

// Remove abandons the transaction identified by key. All resources
// associated with the key will be removed.
func (o *snapshotter) Remove(ctx context.Context, key string) (err error) {
	ctx, t, err := o.ms.TransactionContext(ctx, true)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil && t != nil {
			if rerr := t.Rollback(); rerr != nil {
				log.G(ctx).WithError(rerr).Warn("Failure rolling back transaction")
			}
		}
	}()

	id, _, err := storage.Remove(ctx, key)
	if err != nil {
		return errors.Wrap(err, "failed to remove")
	}

	path := o.getSnapshotDir(id)
	renamed := filepath.Join(o.root, "snapshots", "rm-"+id)
	if err := os.Rename(path, renamed); err != nil {
		if !os.IsNotExist(err) {
			return errors.Wrap(err, "failed to rename")
		}
		renamed = ""
	}

	err = t.Commit()
	t = nil
	if err != nil {
		if renamed != "" {
			if err1 := os.Rename(renamed, path); err1 != nil {
				// May cause inconsistent data on disk
				log.G(ctx).WithError(err1).WithField("path", renamed).Errorf("Failed to rename after failed commit")
			}
		}
		return errors.Wrap(err, "failed to commit")
	}
	if renamed != "" {
		if err := os.RemoveAll(renamed); err != nil {
			// Must be cleaned up, any "rm-*" could be removed if no active transactions
			log.G(ctx).WithError(err).WithField("path", renamed).Warnf("Failed to remove root filesystem")
		}
	}

	return nil
}

// Walk the committed snapshots.
func (o *snapshotter) Walk(ctx context.Context, fn func(context.Context, snapshot.Info) error) error {
	ctx, t, err := o.ms.TransactionContext(ctx, false)
	if err != nil {
		return err
	}
	defer t.Rollback()
	return storage.WalkInfo(ctx, fn)
}

func (o *snapshotter) createActive(ctx context.Context, key, parent string, readonly bool) ([]containerd.Mount, error) {
	var (
		err      error
		path, td string
	)

	td, err = ioutil.TempDir(filepath.Join(o.root, "snapshots"), "new-")
	if err != nil {
		return nil, errors.Wrap(err, "failed to create temp dir")
	}
	defer func() {
		if err != nil {
			if td != "" {
				if err1 := os.RemoveAll(td); err1 != nil {
					err = errors.Wrapf(err, "remove failed: %v", err1)
				}
			}
			if path != "" {
				if err1 := os.RemoveAll(path); err1 != nil {
					err = errors.Wrapf(err, "failed to remove path: %v", err1)
				}
			}
		}
	}()

	ctx, t, err := o.ms.TransactionContext(ctx, true)
	if err != nil {
		return nil, err
	}

	active, err := storage.CreateActive(ctx, key, parent, readonly)
	if err != nil {
		if rerr := t.Rollback(); rerr != nil {
			log.G(ctx).WithError(rerr).Warn("Failure rolling back transaction")
		}
		return nil, errors.Wrap(err, "failed to create active")
	}

	if len(active.ParentIDs) > 0 {
		b, err := ioutil.ReadFile(filepath.Join(o.root, "manifests", active.ParentIDs[0]))
		if err != nil {
			return nil, errors.Wrap(err, "failed to read parent manifest link")
		}
		dgst, err := digest.Parse(string(b))
		if err != nil {
			return nil, errors.Wrap(err, "failed to parse parent manifest digest")
		}
		co := continuity.ContextOptions{
			Provider: contentStore{o.cs, ctx},
		}
		r, err := co.Provider.Reader(dgst)
		if err != nil {
			return nil, errors.Wrap(err, "failed to get parent manifest")
		}
		p, err := ioutil.ReadAll(r)
		if err != nil {
			return nil, errors.Wrap(err, "failed to read parent manifest")
		}
		r.Close()
		m, err := continuity.Unmarshal(p)
		if err != nil {
			return nil, errors.Wrap(err, "failed to unmarshal manifest")
		}

		mctx, err := continuity.NewContextWithOptions(td, co)
		if err != nil {
			return nil, errors.Wrap(err, "failed to get continuity context")
		}

		if err := continuity.ApplyManifest(mctx, m); err != nil {
			return nil, errors.Wrap(err, "failed to apply manifest")
		}
	}

	path = o.getSnapshotDir(active.ID)
	if err := os.Rename(td, path); err != nil {
		if rerr := t.Rollback(); rerr != nil {
			log.G(ctx).WithError(rerr).Warn("Failure rolling back transaction")
		}
		return nil, errors.Wrap(err, "failed to rename")
	}
	td = ""

	if err := t.Commit(); err != nil {
		return nil, errors.Wrap(err, "commit failed")
	}

	return o.mounts(active), nil
}

func (o *snapshotter) getSnapshotDir(id string) string {
	return filepath.Join(o.root, "snapshots", id)
}

func (o *snapshotter) mounts(active storage.Active) []containerd.Mount {
	roFlag := "rw"
	if active.Readonly {
		roFlag = "ro"
	}

	return []containerd.Mount{
		{
			Source: o.getSnapshotDir(active.ID),
			Type:   "bind",
			Options: []string{
				roFlag,
				"rbind",
			},
		},
	}
}

type contentStore struct {
	*content.Store

	ctx context.Context
}

func (cs contentStore) Digest(r io.Reader) (digest.Digest, error) {
	w, err := cs.Store.Writer(cs.ctx, "serial...", 0, "")
	if err != nil {
		return "", err
	}
	n, err := io.Copy(w, r)
	if err != nil {
		return "", err
	}
	dgst := w.Digest()
	log.G(cs.ctx).Infof("Committing %s", dgst)
	if err := w.Commit(n, dgst); err != nil {
		return "", err
	}
	return dgst, nil
}

func (cs contentStore) Reader(dgst digest.Digest) (io.ReadCloser, error) {
	return cs.Store.Reader(cs.ctx, dgst)
}
