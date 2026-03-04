//go:build !linux

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

package erofs

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	cfs "github.com/containerd/continuity/fs"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/internal/erofsutils"
	"github.com/containerd/containerd/v2/pkg/archive"
	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/containerd/containerd/v2/pkg/epoch"
	"github.com/containerd/containerd/v2/pkg/labels"
)

// writeDiff generates the diff tar stream by walking upperRoot and emitting all
// entries as additions. The overlay upper already contains only the files changed
// by the container, so a full lower/upper comparison is not required.
func writeDiff(ctx context.Context, w io.Writer, lower []mount.Mount, upperRoot string) error {
	// The overlay upper already contains only the files changed by the
	// container, so a single-walk of upperRoot is sufficient. DiffDirChanges
	// with DiffSourceOverlayFS is not used here because overlayFSWhiteoutConvert
	// is not implemented for non-Linux hosts. Deletions inside the VM are not
	// representable as overlayfs whiteouts on macOS, so they are intentionally
	// omitted from the diff.
	cw := archive.NewChangeWriter(w, upperRoot)
	if err := cfs.Changes(ctx, "", upperRoot, cw.HandleChange); err != nil {
		return fmt.Errorf("failed to create diff tar stream: %w", err)
	}
	return cw.Close()
}

// resolveUpperRoot returns the path to use as upperRoot for writeDiff.
//
// On non-Linux (block mode), the writable snapshot uses an ext4 block image
// (rwlayer.img) to hold the overlay upper directory inside the VM. The
// image is inaccessible via native mounts on the host, so we extract the
// /upper directory using debugfs(8) from e2fsprogs.
//
// On Linux (non-block mode), the overlay upper is a plain host directory at
// {layer}/fs, so we return that directly.
func resolveUpperRoot(layer string) (root string, cleanup func(), err error) {
	rwlayer := filepath.Join(layer, "rwlayer.img")
	if _, statErr := os.Stat(rwlayer); statErr != nil {
		// Non-block mode (or rwlayer not yet created): use plain host dir.
		return filepath.Join(layer, "fs"), nil, nil
	}

	// Block mode: extract overlay upper from ext4 image using debugfs.
	debugfsBin, lookErr := findDebugfs()
	if lookErr != nil {
		return "", nil, fmt.Errorf("debugfs not found (required to read ext4 upper layer on non-Linux): %w: %w", lookErr, errdefs.ErrNotImplemented)
	}

	tmpDir, mkErr := os.MkdirTemp("", "erofs-upper-*")
	if mkErr != nil {
		return "", nil, fmt.Errorf("failed to create temp dir for upper: %w", mkErr)
	}
	cleanFn := func() { os.RemoveAll(tmpDir) }

	// debugfs rdump extracts /upper recursively into tmpDir, creating
	// tmpDir/upper/ on the host.
	cmd := exec.Command(debugfsBin, "-R", "rdump /upper "+tmpDir, rwlayer)
	if out, runErr := cmd.CombinedOutput(); runErr != nil {
		// debugfs exits non-zero if it can't chown (normal on macOS without
		// root), but the files are still extracted.  Only treat it as a real
		// error when the destination directory was not created at all.
		upperDir := filepath.Join(tmpDir, "upper")
		if _, checkErr := os.Stat(upperDir); checkErr != nil {
			cleanFn()
			return "", nil, fmt.Errorf("debugfs rdump failed: %s: %w", strings.TrimSpace(string(out)), runErr)
		}
	}

	return filepath.Join(tmpDir, "upper"), cleanFn, nil
}

// findDebugfs returns the path to the debugfs(8) binary from e2fsprogs.
// It searches the process PATH first, then falls back to the Homebrew
// e2fsprogs prefix commonly used on macOS.
func findDebugfs() (string, error) {
	if p, err := exec.LookPath("debugfs"); err == nil {
		return p, nil
	}
	// Homebrew on Apple Silicon
	for _, candidate := range []string{
		"/opt/homebrew/opt/e2fsprogs/sbin/debugfs",
		"/usr/local/opt/e2fsprogs/sbin/debugfs",
	} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("debugfs not found in PATH or common locations")
}

// Compare creates a diff between the given mounts and uploads the result
// to the content store.
func (s erofsDiff) Compare(ctx context.Context, lower, upper []mount.Mount, opts ...diff.Opt) (d ocispec.Descriptor, err error) {
	layer, err := erofsutils.MountsToLayer(upper)
	if err != nil {
		return emptyDesc, fmt.Errorf("unsupported layer for erofsDiff Compare method: %w", err)
	}

	var config diff.Config
	for _, opt := range opts {
		if err := opt(&config); err != nil {
			return emptyDesc, err
		}
	}
	if tm := epoch.FromContext(ctx); tm != nil && config.SourceDateEpoch == nil {
		config.SourceDateEpoch = tm
	}

	if config.MediaType == "" {
		config.MediaType = ocispec.MediaTypeImageLayerGzip
	}

	var compressionType compression.Compression
	switch config.MediaType {
	case ocispec.MediaTypeImageLayer:
		compressionType = compression.Uncompressed
	case ocispec.MediaTypeImageLayerGzip:
		compressionType = compression.Gzip
	case ocispec.MediaTypeImageLayerZstd:
		compressionType = compression.Zstd
	default:
		return emptyDesc, fmt.Errorf("unsupported diff media type: %v: %w", config.MediaType, errdefs.ErrNotImplemented)
	}

	var newReference bool
	if config.Reference == "" {
		newReference = true
		config.Reference = uniqueRef()
	}

	cw, err := s.store.Writer(ctx,
		content.WithRef(config.Reference),
		content.WithDescriptor(ocispec.Descriptor{
			MediaType: config.MediaType, // most contentstore implementations just ignore this
		}))
	if err != nil {
		return emptyDesc, fmt.Errorf("failed to open writer: %w", err)
	}

	// errOpen is set when an error occurs while the content writer has not been
	// committed or closed yet to force a cleanup
	var errOpen error
	defer func() {
		if errOpen != nil {
			cw.Close()
			if newReference {
				if abortErr := s.store.Abort(ctx, config.Reference); abortErr != nil {
					log.G(ctx).WithError(abortErr).WithField("ref", config.Reference).Warnf("failed to delete diff upload")
				}
			}
		}
	}()
	if !newReference {
		if errOpen = cw.Truncate(0); errOpen != nil {
			return emptyDesc, errOpen
		}
	}

	upperRoot, cleanupUpper, errOpen := resolveUpperRoot(layer)
	if errOpen != nil {
		return emptyDesc, fmt.Errorf("failed to resolve upper root: %w", errOpen)
	}
	if cleanupUpper != nil {
		defer cleanupUpper()
	}

	if compressionType != compression.Uncompressed {
		dgstr := digest.SHA256.Digester()
		var compressed io.WriteCloser
		if config.Compressor != nil {
			compressed, errOpen = config.Compressor(cw, config.MediaType)
			if errOpen != nil {
				return emptyDesc, fmt.Errorf("failed to get compressed stream: %w", errOpen)
			}
		} else {
			compressed, errOpen = compression.CompressStream(cw, compressionType)
			if errOpen != nil {
				return emptyDesc, fmt.Errorf("failed to get compressed stream: %w", errOpen)
			}
		}
		errOpen = writeDiff(ctx, io.MultiWriter(compressed, dgstr.Hash()), lower, upperRoot)
		compressed.Close()
		if errOpen != nil {
			return emptyDesc, fmt.Errorf("failed to write compressed diff: %w", errOpen)
		}

		if config.Labels == nil {
			config.Labels = map[string]string{}
		}
		config.Labels[labels.LabelUncompressed] = dgstr.Digest().String()
	} else {
		err := writeDiff(ctx, cw, lower, upperRoot)
		if err != nil {
			return emptyDesc, fmt.Errorf("failed to create diff tar stream: %w", err)
		}
	}

	var commitopts []content.Opt
	if config.Labels != nil {
		commitopts = append(commitopts, content.WithLabels(config.Labels))
	}

	dgst := cw.Digest()
	if errOpen = cw.Commit(ctx, 0, dgst, commitopts...); errOpen != nil {
		if !errdefs.IsAlreadyExists(errOpen) {
			return emptyDesc, fmt.Errorf("failed to commit: %w", errOpen)
		}
		errOpen = nil
	}

	info, err := s.store.Info(ctx, dgst)
	if err != nil {
		return emptyDesc, fmt.Errorf("failed to get info from content store: %w", err)
	}
	if info.Labels == nil {
		info.Labels = make(map[string]string)
	}
	// Set "containerd.io/uncompressed" label if digest already existed without label
	if _, ok := info.Labels[labels.LabelUncompressed]; !ok {
		info.Labels[labels.LabelUncompressed] = config.Labels[labels.LabelUncompressed]
		if _, err := s.store.Update(ctx, info, "labels."+labels.LabelUncompressed); err != nil {
			return emptyDesc, fmt.Errorf("error setting uncompressed label: %w", err)
		}
	}

	return ocispec.Descriptor{
		MediaType: config.MediaType,
		Size:      info.Size,
		Digest:    info.Digest,
	}, nil
}
