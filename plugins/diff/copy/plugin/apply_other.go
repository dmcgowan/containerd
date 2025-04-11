//go:build !linux

package plugin

import (
	"context"
	"path/filepath"

	"github.com/containerd/errdefs"

	"github.com/containerd/containerd/v2/core/mount"
)

func apply(ctx context.Context, mounts []mount.Mount, target string, copyTo func(string) error) (retErr error) {
	if len(mounts) > 1 || mounts[0].Type != "bind" {
		return errdefs.ErrNotImplemented
	}
	return copyTo(filepath.Join(mounts[0].Source, target))
}
