// +build linux

package btrfs

import (
	"context"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/snapshot"
	"github.com/containerd/containerd/snapshot/testsuite"
	"github.com/containerd/containerd/testutil"
	"github.com/pkg/errors"
)

const (
	mib = 1024 * 1024
)

func boltSnapshotter(t *testing.T) func(context.Context, string) (snapshot.Snapshotter, func(), error) {
	return func(ctx context.Context, root string) (snapshot.Snapshotter, func(), error) {
		device, err := setupBtrfsLoopbackDevice(t, root)
		if err != nil {
			return nil, nil, err
		}
		snapshotter, err := NewSnapshotter(device.deviceName, root)
		if err != nil {
			device.remove(t)
			return nil, nil, err
		}

		return snapshotter, func() {
			device.remove(t)
		}, nil
	}
}

func TestBtrfs(t *testing.T) {
	testutil.RequiresRoot(t)
	testsuite.SnapshotterSuite(t, "Btrfs", boltSnapshotter(t))
}

func TestFSBtrfs(t *testing.T) {
	testutil.RequiresRoot(t)
	testsuite.FSSuite(t, boltSnapshotter(t), true)
}

func TestBtrfsMounts(t *testing.T) {
	testutil.RequiresRoot(t)
	ctx := context.Background()

	// create temporary directory for mount point
	mountPoint, err := ioutil.TempDir("", "containerd-btrfs-test")
	if err != nil {
		t.Fatal("could not create mount point for btrfs test", err)
	}
	defer os.RemoveAll(mountPoint)
	t.Log("temporary mount point created", mountPoint)

	root, err := ioutil.TempDir(mountPoint, "TestBtrfsPrepare-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)

	b, c, err := boltSnapshotter(t)(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer c()

	target := filepath.Join(root, "test")
	mounts, err := b.Prepare(ctx, target, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Log(mounts)

	for _, mount := range mounts {
		if mount.Type != "btrfs" {
			t.Fatalf("wrong mount type: %v != btrfs", mount.Type)
		}

		// assumes the first, maybe incorrect in the future
		if !strings.HasPrefix(mount.Options[0], "subvolid=") {
			t.Fatalf("no subvolid option in %v", mount.Options)
		}
	}

	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatal(err)
	}
	if err := containerd.MountAll(mounts, target); err != nil {
		t.Fatal(err)
	}
	defer testutil.Unmount(t, target)

	// write in some data
	if err := ioutil.WriteFile(filepath.Join(target, "foo"), []byte("content"), 0777); err != nil {
		t.Fatal(err)
	}

	// TODO(stevvooe): We don't really make this with the driver, but that
	// might prove annoying in practice.
	if err := os.MkdirAll(filepath.Join(root, "snapshots"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := b.Commit(ctx, filepath.Join(root, "snapshots/committed"), filepath.Join(root, "test")); err != nil {
		t.Fatal(err)
	}

	target = filepath.Join(root, "test2")
	mounts, err = b.Prepare(ctx, target, filepath.Join(root, "snapshots/committed"))
	if err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatal(err)
	}

	if err := containerd.MountAll(mounts, target); err != nil {
		t.Fatal(err)
	}
	defer testutil.Unmount(t, target)

	// TODO(stevvooe): Verify contents of "foo"
	if err := ioutil.WriteFile(filepath.Join(target, "bar"), []byte("content"), 0777); err != nil {
		t.Fatal(err)
	}

	if err := b.Commit(ctx, filepath.Join(root, "snapshots/committed2"), target); err != nil {
		t.Fatal(err)
	}
}

type testDevice struct {
	mountPoint string
	fileName   string
	deviceName string
}

// setupBtrfsLoopbackDevice creates a file, mounts it as a loopback device, and
// formats it as btrfs.  The device should be cleaned up by calling
// removeBtrfsLoopbackDevice.
func setupBtrfsLoopbackDevice(t *testing.T, mountPoint string) (*testDevice, error) {

	// create temporary file for the disk image
	file, err := ioutil.TempFile("", "containerd-btrfs-test")
	if err != nil {
		return nil, errors.Wrap(err, "failed to create temp file")
	}
	t.Log("Temporary file created", file.Name())

	// initialize file with 100 MiB
	err = file.Truncate(100 << 20)
	file.Close()
	if err != nil {
		return nil, errors.Wrap(err, "failed to truncate file to 100MB")
	}

	// create device
	losetup := exec.Command("losetup", "--find", "--show", file.Name())
	p, err := losetup.Output()
	if err != nil {
		return nil, errors.Wrap(err, "failed to setup loopback")
	}

	deviceName := strings.TrimSpace(string(p))
	t.Log("Created loop device", deviceName)

	remove := func() {
		if err := exec.Command("losetup", "--detach", deviceName).Run(); err != nil {
			t.Logf("Loopback detach failed for %s: %v", deviceName, err)
		}
	}

	// format
	t.Log("Creating btrfs filesystem")
	mkfs := exec.Command("mkfs.btrfs", deviceName)
	err = mkfs.Run()
	if err != nil {
		remove()
		return nil, errors.Wrap(err, "could not run mkfs.btrfs")
	}

	// mount
	t.Logf("Mounting %s at %s", deviceName, mountPoint)
	mount := exec.Command("mount", deviceName, mountPoint)
	err = mount.Run()
	if err != nil {
		remove()
		return nil, errors.Wrap(err, "could not mount")
	}

	return &testDevice{
		mountPoint: mountPoint,
		fileName:   file.Name(),
		deviceName: deviceName,
	}, nil
}

// remove cleans up the test device, unmounting the loopback and disk image
// file.
func (device *testDevice) remove(t *testing.T) {
	// unmount
	testutil.Unmount(t, device.mountPoint)

	// detach device
	t.Log("Removing loop device")
	losetup := exec.Command("losetup", "--detach", device.deviceName)
	err := losetup.Run()
	if err != nil {
		t.Error("Could not remove loop device", device.deviceName, err)
	}

	// remove file
	t.Log("Removing temporary file")
	err = os.Remove(device.fileName)
	if err != nil {
		t.Error(err)
	}

	// remove mount point
	t.Log("Removing temporary mount point")
	err = os.RemoveAll(device.mountPoint)
	if err != nil {
		t.Error(err)
	}
}
