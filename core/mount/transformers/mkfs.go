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

package handlers

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/errdefs"
)

// MkfsHandler returns a handler which formats the given
// source with the provided filesystem if not created.
func MkfsHandler(roots ...string) (mount.Handler, error) {
	var rootMap = map[string]*os.Root{}
	for _, root := range roots {
		if !filepath.IsAbs(root) {
			return nil, fmt.Errorf("mkdir handler root %q must be absolute path: %w", root, errdefs.ErrInvalidArgument)
		}
		r, err := os.OpenRoot(root)
		if err != nil {
			return nil, fmt.Errorf("failed to open root %q: %w", root, err)
		}
		rootMap[filepath.Clean(root)+string(filepath.Separator)] = r
	}

	return &mkdirHandler{
		rootMap: rootMap,
	}, nil
}

type mkfsHandler struct {
	rootMap map[string]*os.Root
}

func (h *mkfsHandler) Mount(ctx context.Context, m mount.Mount, mp string, _ []mount.ActiveMount) (mount.ActiveMount, error) {
	if m.Type != "mkfs" {
		return mount.ActiveMount{}, errdefs.ErrNotImplemented
	}

	var r *os.Root
	var subpath string

	for path, root := range h.rootMap {
		if strings.HasPrefix(m.Source, path) {
			r = root
			subpath = strings.TrimPrefix(m.Source, path)
			break
		}
	}
	if r == nil {
		return mount.ActiveMount{}, fmt.Errorf("no root %q configured for mkdir: %w", m.Source, errdefs.ErrNotImplemented)
	}

	var (
		format = "ext4"
		size   int64
		id     string
	)
	// Parse options
	for _, o := range m.Options {
		key, value, ok := strings.Cut(o, "=")
		if !ok {
			key = o
			value = "true"
		}
		switch key {
		case "size_mb":
			sizemb, err := strconv.Atoi(value)
			if err != nil {
				return mount.ActiveMount{}, fmt.Errorf("bad option %s: %w", key, err)
			}
			size = int64(sizemb) * 1024 * 1024
		case "fs":
			format = value
		case "uuid":
			id = value
		default:
			return mount.ActiveMount{}, fmt.Errorf("unknown mount option %s: %w", key, errdefs.ErrInvalidArgument)
		}
	}
	switch format {
	case "ext4":
		// ok
	default:
		return mount.ActiveMount{}, fmt.Errorf("unsupported filesystem %q: %w", format, errdefs.ErrInvalidArgument)
	}

	if _, err := r.Stat(subpath); err == nil {
		// Check magic number
	} else if os.IsNotExist(err) {
		f, err := r.OpenFile(subpath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0640)
		if err != nil {
			return mount.ActiveMount{}, fmt.Errorf("failed to create file %q: %w", m.Source, err)
		}
		name := f.Name()
		err = f.Truncate(size)
		f.Close()
		if err != nil {
			return mount.ActiveMount{}, fmt.Errorf("failed to truncate file %q: %w", m.Source, err)
		}

		if err := createWritableImage(ctx, name, id); err != nil {
			return mount.ActiveMount{}, fmt.Errorf("failed format %q: %w", m.Source, err)
		}
	} else {
		return mount.ActiveMount{}, fmt.Errorf("failed to stat %q: %w", m.Source, err)
	}

	t := time.Now()
	return mount.ActiveMount{
		Mount:      m,
		MountedAt:  &t,
		MountPoint: mp,
	}, nil
}

func (*mkfsHandler) Unmount(ctx context.Context, path string) error {
	return nil
}

func createWritableImage(ctx context.Context, filename string, uuid string) error {
	args := []string{"-q"}
	if uuid != "" {
		args = append(args, []string{"-U", uuid}...)
	}
	args = append(args, filename)
	// TODO: Pre-resolve this and pass it in
	cmd := exec.CommandContext(ctx, "mkfs.ext4", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mkfs.ext4 failed: %s: %w", out, err)
	}
	return nil
}
