//go:build !windows

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

package v2

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/fifo"
	"golang.org/x/sys/unix"
)

func openShimLog(ctx context.Context, bundle *Bundle, _ func(string, time.Duration) (net.Conn, error)) (io.ReadCloser, error) {
	return fifo.OpenFifo(ctx, filepath.Join(bundle.Path, "log"), unix.O_RDWR|unix.O_CREAT|unix.O_NONBLOCK, 0700)
}

func checkCopyShimLogError(ctx context.Context, err error) error {
	select {
	case <-ctx.Done():
		if err == fifo.ErrReadClosed || errors.Is(err, os.ErrClosed) {
			return nil
		}
	default:
	}
	return err
}

// defaultSocketDir returns the directory used for shim unix sockets.
// The path is intentionally kept short and hardcoded rather than derived
// from the configured state directory, because unix socket paths are
// limited to 108 characters.
func defaultSocketDir() string {
	defaultDir := filepath.Join(defaults.DefaultStateDir, "s")
	uid := os.Geteuid()
	if uid == 0 {
		return defaultDir
	}

	// Check if default state dir is already setup for this non-root user
	if st, err := os.Stat(defaults.DefaultStateDir); err == nil {
		if sys := st.Sys(); sys != nil {
			if stat, ok := sys.(*syscall.Stat_t); ok && int(stat.Uid) == uid {
				return defaultDir
			}
		}
	}

	return fmt.Sprintf("/tmp/containerd-s-%d", uid)
}
