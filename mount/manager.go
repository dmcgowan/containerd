package mount

import (
	"context"
	"time"
)

type Mounted struct {
	Mount
	// MountState?
	MountPoint string
	MountedAt  time.Time
	Labels     map[string]string
}

type mountOptions struct {
	labels map[string]string
}

type MountOpt func(*mountOptions) error

// MountManager manages mounts
type MountManager interface {
	Mount(context.Context, []Mount, string, ...MountOpt) ([]Mounted, error)
	Unmount(context.Context, string) error
	List(filter ...string) ([]Mounted, error)
}

// WithLabels sets labels on a created mount.
func WithLabels(labels map[string]string) MountOpt {
	return func(opts *mountOptions) error {
		opts.labels = labels
	}
}

// MountHandler handles preparing a mount, performing a mount, and unmounting.
type MountHandler interface {
	// Prepare prepares the given mount in the current mount context.
	// Returns a new mount and a target.
	// The target should only be set when mount has a predefined
	// value which cannot be overriden by the caller.
	// In the case where the caller must have use a target but
	// the target is provided, it is up to the caller to link the
	// perform the link/bind mount or error out. The caller must
	// use the target when given.
	Prepare(context.Context, Mount, []Mounted) (Mount, string, error)

	// Mount
	Mount(context.Context, Mount, string) error

	// Unmount
	Unmount(context.Context, Mount, string) error
}
