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

package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	criruntime "k8s.io/cri-api/pkg/apis/runtime/v1"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/integration/images"
	"github.com/containerd/containerd/v2/plugins"
)

// TestPodSharesBlockDevice verifies that the containers of a pod share
// a single backing block device when the snapshotter supports it.
//
// This exercises the whole path: the CRI plugin places every writable
// snapshot in the pod in one group, the erofs snapshotter backs the
// group with a single ext4 image, and the mount manager mounts that
// image once no matter how many containers reference it. The image is
// only reclaimed once the last member of the group is gone.
func TestPodSharesBlockDevice(t *testing.T) {
	workDir := t.TempDir()
	cfgPath := filepath.Join(workDir, "config.toml")
	cfg := `
version = 3

[plugins.'io.containerd.cri.v1.images']
  snapshotter = "erofs"

[plugins.'io.containerd.cri.v1.runtime'.containerd]
  default_runtime_name = "runc"

[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runc]
  runtime_type = "io.containerd.runc.v2"
  snapshotter = "erofs"

[plugins.'io.containerd.snapshotter.v1.erofs']
  default_size = "64MB"

# Reclaim the shared image as soon as the last snapshot using it is
# removed, so the test does not have to wait for a scheduled collection.
[plugins.'io.containerd.gc.v1.scheduler']
  deletion_threshold = 1
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfg), 0o600))

	ctrd := newCtrdProc(t, *containerdBin, workDir, nil)
	require.NoError(t, ctrd.isReady())

	rSvc := ctrd.criRuntimeService(t)
	iSvc := ctrd.criImageService(t)

	ctrdClient, err := containerd.New(ctrd.grpcAddress(), containerd.WithDefaultNamespace(k8sNamespace))
	require.NoError(t, err)

	t.Cleanup(func() {
		if t.Failed() {
			t.Log("Dumping containerd config and logs due to test failure")
			dumpFileContent(t, ctrd.configPath())
			dumpFileContent(t, ctrd.logPath())
		}
		assert.NoError(t, ctrdClient.Close())
		cleanupPods(t, rSvc)
		assert.NoError(t, ctrd.kill(syscall.SIGTERM))
		assert.NoError(t, ctrd.wait(5*time.Minute))
	})

	snapshotterRoot := requireGroupingSnapshotter(t, ctrdClient, "erofs")
	groupsDir := filepath.Join(snapshotterRoot, "groups")

	imageName := images.Get(images.BusyBox)
	pullImagesByCRI(t, iSvc, imageName)

	podCtx := newPodTCtx(t, rSvc, "shared-block-pod", "snapshot-group")

	first := podCtx.createContainer("first", imageName,
		criruntime.ContainerState_CONTAINER_RUNNING,
		WithCommand("sleep", "1d"))
	second := podCtx.createContainer("second", imageName,
		criruntime.ContainerState_CONTAINER_RUNNING,
		WithCommand("sleep", "1d"))

	// One image for the whole pod, not one per container.
	groupFiles := groupImages(t, groupsDir)
	require.Len(t, groupFiles, 1, "every container in the pod should share one block image")
	image := groupFiles[0]

	// And it is attached once, however many containers use it.
	assert.Len(t, loopDevicesFor(t, image), 1,
		"the shared image should be attached to a single loop device")

	// Sharing the device must not mean sharing the filesystem: each
	// container gets its own directory inside it.
	_, stderr, err := rSvc.ExecSync(first, []string{"sh", "-c", "echo first > /only-in-first"}, 30*time.Second)
	require.NoError(t, err, "stderr: %s", stderr)

	stdout, _, err := rSvc.ExecSync(first, []string{"cat", "/only-in-first"}, 30*time.Second)
	require.NoError(t, err)
	assert.Equal(t, "first\n", string(stdout))

	_, _, err = rSvc.ExecSync(second, []string{"test", "-e", "/only-in-first"}, 30*time.Second)
	assert.Error(t, err, "containers sharing a block device must not share a rootfs")

	// The image outlives any individual container.
	require.NoError(t, rSvc.StopContainer(first, 0))
	require.NoError(t, rSvc.RemoveContainer(first))
	assertGroupImageRetained(t, image)

	require.NoError(t, rSvc.StopContainer(second, 0))
	require.NoError(t, rSvc.RemoveContainer(second))
	// The pause container still holds the group.
	assertGroupImageRetained(t, image)

	// Removing the pod removes the last member, and with it the image.
	require.NoError(t, rSvc.StopPodSandbox(podCtx.id))
	require.NoError(t, rSvc.RemovePodSandbox(podCtx.id))

	require.Eventually(t, func() bool {
		_, err := os.Stat(image)
		return os.IsNotExist(err)
	}, 2*time.Minute, time.Second, "shared image %s should be reclaimed with the last member", image)

	assert.Empty(t, loopDevicesFor(t, image), "the shared image should no longer be attached")
}

// requireGroupingSnapshotter skips the test unless the named
// snapshotter is registered and reports that it supports grouping,
// and returns its root directory.
func requireGroupingSnapshotter(t *testing.T, client *containerd.Client, snapshotter string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.IntrospectionService().Plugins(ctx,
		fmt.Sprintf("type==%s,id==%s", plugins.SnapshotPlugin, snapshotter))
	require.NoError(t, err)
	if len(resp.Plugins) == 0 {
		t.Skipf("%s snapshotter plugin is not registered", snapshotter)
	}

	p := resp.Plugins[0]
	if initErr := p.InitErr; initErr != nil {
		t.Skipf("%s snapshotter plugin is not ready: %s", snapshotter, initErr.Message)
	}
	if !slices.Contains(p.Capabilities, "groups") {
		t.Skipf("%s snapshotter does not support snapshot grouping: %v", snapshotter, p.Capabilities)
	}

	root := p.Exports[plugins.SnapshotterRootDir]
	require.NotEmpty(t, root, "%s snapshotter does not export its root directory", snapshotter)
	return root
}

// groupImages returns the block images backing the snapshot groups
// which currently exist.
func groupImages(t *testing.T, groupsDir string) []string {
	t.Helper()

	entries, err := os.ReadDir(groupsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		require.NoError(t, err)
	}

	var found []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		images, err := filepath.Glob(filepath.Join(groupsDir, e.Name(), "*.img"))
		require.NoError(t, err)
		found = append(found, images...)
	}
	return found
}

// loopDevicesFor returns the loop devices currently attached to the
// given file.
func loopDevicesFor(t *testing.T, path string) []string {
	t.Helper()

	backingFiles, err := filepath.Glob("/sys/block/loop*/loop/backing_file")
	require.NoError(t, err)

	var devices []string
	for _, bf := range backingFiles {
		b, err := os.ReadFile(bf)
		if err != nil {
			// The device may be detached while we are looking at it.
			if os.IsNotExist(err) {
				continue
			}
			require.NoError(t, err)
		}
		backing := strings.TrimSuffix(strings.TrimRight(string(b), "\n"), " (deleted)")
		if backing == path {
			devices = append(devices, filepath.Base(filepath.Dir(filepath.Dir(bf))))
		}
	}
	return devices
}

// assertGroupImageRetained checks the shared image is still present,
// allowing time for a collection which should not have removed it.
func assertGroupImageRetained(t *testing.T, image string) {
	t.Helper()

	// Give any triggered collection a chance to run, so that a
	// premature removal is caught rather than raced past.
	time.Sleep(2 * time.Second)
	_, err := os.Stat(image)
	assert.NoError(t, err, "shared image %s should be retained while the group has members", image)
}
