package testsuite

import (
	"context"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/fs/fstest"
	"github.com/containerd/containerd/snapshot"
	"github.com/pkg/errors"
)

// FSSuite runs a test suite of file system tests on the snapshotter.
func FSSuite(t *testing.T, snapshotterFn func(ctx context.Context, root string) (snapshot.Snapshotter, func(), error), all bool) {
	fstest.FSSuite(t, snapshotApplier{snapshotterFn}, all)
}

type snapshotApplier struct {
	snapshotC func(ctx context.Context, root string) (snapshot.Snapshotter, func(), error)
}

type snapshotKey struct{}
type baseKey struct{}

func (s snapshotApplier) TestContext(ctx context.Context) (context.Context, func(), error) {
	base, err := ioutil.TempDir("", "test-snapshot-")
	if err != nil {
		return ctx, nil, errors.Wrap(err, "failed to create temp dir")
	}
	if err := os.Mkdir(filepath.Join(base, "snapshots"), 0700); err != nil {
		return ctx, nil, errors.Wrap(err, "failed to make snapshot dir")
	}
	sn, close, err := s.snapshotC(ctx, filepath.Join(base, "snapshots"))
	if err != nil {
		return ctx, nil, errors.Wrap(err, "failed to create snapshotter")
	}

	ctx = context.WithValue(ctx, baseKey{}, base)
	ctx = context.WithValue(ctx, snapshotKey{}, sn)

	return ctx, func() {
		os.RemoveAll(base)
		close()
	}, nil
}
func (s snapshotApplier) Apply(ctx context.Context, a fstest.Applier) (string, func(), error) {
	base := ctx.Value(baseKey{}).(string)
	sn := ctx.Value(snapshotKey{}).(snapshot.Snapshotter)

	applyDir, err := ioutil.TempDir(base, "apply-copy-")
	if err != nil {
		return "", nil, errors.Wrap(err, "failed to create temp dir")
	}
	defer os.RemoveAll(applyDir)

	b, err := ioutil.ReadFile(filepath.Join(base, "base"))
	if err != nil && !os.IsNotExist(err) {
		return "", nil, errors.Wrap(err, "failed to read base file")
	}

	mounts, err := sn.Prepare(ctx, applyDir, string(b))
	if err != nil {
		return "", nil, errors.Wrap(err, "failed to prepare base")
	}

	if err := containerd.MountAll(mounts, applyDir); err != nil {
		return "", nil, errors.Wrap(err, "mount failed")
	}

	err = a.Apply(applyDir)
	containerd.Unmount(applyDir, 0)
	if err != nil {
		return "", nil, errors.Wrap(err, "failed to apply changes to base snapshot")
	}

	newBase := string(b)
	if newBase == "" {
		newBase = "base"
	} else {
		newBase += "^"
	}

	if err := sn.Commit(ctx, newBase, applyDir); err != nil {
		return "", nil, errors.Wrap(err, "failed to commit apply directory")
	}

	// Base file stores the base key for the subsequent layers to apply to
	if err := ioutil.WriteFile(filepath.Join(base, "base"), []byte(newBase), 0600); err != nil {
		return "", nil, errors.Wrap(err, "failed to persist base file")
	}

	newDir, err := ioutil.TempDir(base, "base-copy-")
	if err != nil {
		return "", nil, errors.Wrap(err, "failed to create temp dir")
	}

	mounts, err = sn.Prepare(ctx, newDir, newBase)
	if err != nil {
		return "", nil, errors.Wrap(err, "failed to prepare new base")
	}

	if err := containerd.MountAll(mounts, newDir); err != nil {
		return "", nil, errors.Wrap(err, "failed to mount new base")
	}

	return newDir, func() {
		containerd.Unmount(newDir, 0)
		os.RemoveAll(newDir)
	}, nil
}
