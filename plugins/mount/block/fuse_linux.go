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

package block

// FUSE-based EROFS mounting for unprivileged (non-root) environments.
//
// When loop devices are unavailable (no CAP_SYS_ADMIN) the block handler
// mounts the fully-filled cache file using erofs-fuse, which reads the EROFS
// image via /dev/fuse without any privileged kernel interface.  fusermount (or
// fusermount3) is used for unmount so the user does not need CAP_SYS_ADMIN
// for teardown either.
//
// erofs-fuse binary search order:
//   1. $CONTAINERD_EROFS_FUSE_BINARY env var
//   2. erofs-fuse on $PATH
//
// fusermount binary search order (for unmount):
//   1. fusermount3 on $PATH
//   2. fusermount  on $PATH

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/containerd/log"
	"golang.org/x/sys/unix"
)

const (
	// erofsFuseBinaryEnv overrides the erofs-fuse binary path.
	erofsFuseBinaryEnv = "CONTAINERD_EROFS_FUSE_BINARY"

	// fuseReadyTimeout is how long to wait for the erofs-fuse process to
	// signal readiness by making the mountpoint's filesystem type change to
	// FUSE.
	fuseReadyTimeout = 5 * time.Second
)

// fuseSuperMagic is statfs(2) f_type for any FUSE filesystem.
const fuseSuperMagic = 0x65735546

// erofsFuseBinary returns the path to the erofs-fuse binary.
func erofsFuseBinary() (string, error) {
	if v := os.Getenv(erofsFuseBinaryEnv); v != "" {
		return v, nil
	}
	// Try both common binary names: erofs-fuse (Arch/Fedora) and erofsfuse (Debian/Ubuntu).
	for _, name := range []string{"erofs-fuse", "erofsfuse"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("erofs-fuse binary not found (tried erofs-fuse, erofsfuse; install erofs-utils with FUSE support or set %s)", erofsFuseBinaryEnv)
}

// mountErofsFuse mounts backingFile at mp using erofs-fuse.  It launches the
// erofs-fuse process and waits until the mountpoint's filesystem type becomes
// FUSE (indicating the mount is ready) before returning.  The caller owns the
// returned *exec.Cmd and must call unmountFuse when done.
func mountErofsFuse(backingFile, mp string) (*exec.Cmd, error) {
	bin, err := erofsFuseBinary()
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(mp, 0755); err != nil {
		return nil, fmt.Errorf("block/fuse: create mountpoint %s: %w", mp, err)
	}

	// erofs-fuse <image> <mountpoint> with allow_other so user-namespace
	// processes can access the FUSE mount (requires user_allow_other in
	// /etc/fuse.conf).
	cmd := exec.Command(bin, "-o", "allow_other", backingFile, mp)
	cmd.Stdout = log.L.WriterLevel(log.DebugLevel)
	cmd.Stderr = log.L.WriterLevel(log.WarnLevel)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("block/fuse: start erofs-fuse: %w", err)
	}
	// erofs-fuse (erofsfuse) daemonizes: the parent process exits promptly
	// while the daemon child holds the FUSE mount.  We therefore cannot use
	// process-exit as an error signal — just poll for the mount to appear.
	_ = cmd.Wait()

	deadline := time.NewTimer(fuseReadyTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var st unix.Statfs_t
		if unix.Statfs(mp, &st) == nil && st.Type == fuseSuperMagic {
			// Mount is live.  Return a nil Cmd — the daemon child has already
			// detached; unmounting is done via fusermount/fusermount3.
			return nil, nil
		}
		select {
		case <-deadline.C:
			return nil, fmt.Errorf("block/fuse: erofs-fuse did not mount %s within %s", mp, fuseReadyTimeout)
		case <-tick.C:
		}
	}
}

// unmountFuse unmounts a FUSE mountpoint using fusermount / fusermount3 and
// then waits for the erofs-fuse process to exit.
func unmountFuse(mp string, fuseCmd *exec.Cmd) error {
	// Prefer fusermount3, fall back to fusermount.
	var umountErr error
	for _, bin := range []string{"fusermount3", "fusermount"} {
		p, err := exec.LookPath(bin)
		if err != nil {
			continue
		}
		out, err := exec.Command(p, "-u", mp).CombinedOutput()
		if err == nil {
			umountErr = nil
			break
		}
		umountErr = fmt.Errorf("block/fuse: %s -u %s: %w (%s)", bin, mp, err, out)
	}
	if umountErr != nil {
		// Last-resort: kernel unmount (may fail without privileges but worth trying).
		if kerr := unix.Unmount(mp, unix.MNT_DETACH); kerr == nil {
			umountErr = nil
		}
	}

	if fuseCmd != nil && fuseCmd.Process != nil {
		_ = fuseCmd.Process.Kill()
		_ = fuseCmd.Wait()
	}
	return umountErr
}
