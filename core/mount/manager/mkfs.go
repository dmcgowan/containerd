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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/containerd/errdefs"
	"github.com/containerd/log"

	"github.com/containerd/containerd/v2/core/mount"
)

type mkfs struct {
	rootMap map[string]*os.Root
}

func (t *mkfs) Transform(ctx context.Context, m mount.Mount, a []mount.ActiveMount) (mount.Mount, error) {
	var r *os.Root
	var subpath string

	for path, root := range t.rootMap {
		if strings.HasPrefix(m.Source, path) {
			r = root
			subpath = strings.TrimPrefix(m.Source, path)
			subpath, _ = filepath.Rel("/", subpath)
			break
		}
	}
	if r == nil {
		return m, fmt.Errorf("no root %q configured for mkdir: %w", m.Source, errdefs.ErrNotImplemented)
	}

	log.G(ctx).Debugf("transforming mkfs mount: %+v", m)

	var (
		size int64
		id   string
	)
	var options []string
	for _, o := range m.Options {
		if strings.HasPrefix(o, "mkfs.") {
			key, value, ok := strings.Cut(o[5:], "=")
			if !ok {
				key = o
				value = "true"
			}
			switch key {
			case "size_mb":
				sizemb, err := strconv.Atoi(value)
				if err != nil {
					return mount.Mount{}, fmt.Errorf("bad option %s: %w", key, err)
				}
				size = int64(sizemb) * 1024 * 1024
			case "fs":
				if value != "ext4" {
					return mount.Mount{}, fmt.Errorf("unsupported filesystem %q: %w", value, errdefs.ErrInvalidArgument)
				}
			case "uuid":
				id = value
			default:
				return mount.Mount{}, fmt.Errorf("unknown mount option %s: %w", key, errdefs.ErrInvalidArgument)
			}

		} else {
			options = append(options, o)
		}
	}
	m.Options = options
	if size == 0 {
		return mount.Mount{}, fmt.Errorf("mkfs requires mkfs.size_mb option: %w", errdefs.ErrInvalidArgument)
	}

	if _, err := r.Stat(subpath); err == nil {
		// Check magic number
	} else if os.IsNotExist(err) {
		f, err := r.OpenFile(subpath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0640)
		if err != nil {
			return mount.Mount{}, fmt.Errorf("failed to create file %q: %w", m.Source, err)
		}
		name := f.Name()
		err = f.Truncate(size)
		f.Close()
		if err != nil {
			return mount.Mount{}, fmt.Errorf("failed to truncate file %q: %w", m.Source, err)
		}

		if err := createWritableImage(ctx, name, id); err != nil {
			return mount.Mount{}, fmt.Errorf("failed format %q: %w", m.Source, err)
		}
	} else {
		return mount.Mount{}, fmt.Errorf("failed to stat %q: %w", m.Source, err)
	}

	return m, nil
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
