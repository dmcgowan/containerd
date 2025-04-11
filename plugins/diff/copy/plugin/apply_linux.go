package plugin

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/containerd/containerd/v2/core/mount"
)

func apply(ctx context.Context, mounts []mount.Mount, target string, copyTo func(string) error) (retErr error) {
	switch {
	case len(mounts) == 1 && mounts[0].Type == "overlay":
		const upperdirPrefix = "upperdir="
		var upper string
		for _, o := range mounts[0].Options {
			if strings.HasPrefix(o, upperdirPrefix) {
				upper = strings.TrimPrefix(o, upperdirPrefix)
			}
		}
		if upper == "" {
			break
		}
		return copyTo(filepath.Join(upper, target))
	case len(mounts) == 1 && mounts[0].Type == "bind":
		return copyTo(filepath.Join(mounts[0].Source, target))
	}
	return mount.WithTempMount(ctx, mounts, func(root string) error {
		return copyTo(filepath.Join(root, target))
	})
}
