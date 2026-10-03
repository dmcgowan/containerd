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
	"os"
	"path/filepath"
	"runtime"

	"github.com/containerd/log"

	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/timeout"
	"golang.org/x/sync/errgroup"
)

// LoadExistingShims loads existing shims from the path specified by stateDir
// rootDir is for cleaning up the unused paths of removed shims.
//
// cfg.Reap decides, per shim, whether to keep or reap it; cfg.Runtime is
// usually left empty so each shim's runtime is read from its bundle.
func (m *ShimManager) LoadExistingShims(ctx context.Context, stateDir string, rootDir string, cfg LoadConfig) error {
	if cfg.OnClose == nil {
		return errors.New("LoadConfig.OnClose must not be nil")
	}
	nsDirs, err := os.ReadDir(stateDir)
	if err != nil {
		return err
	}
	for _, nsd := range nsDirs {
		if !nsd.IsDir() {
			continue
		}
		ns := nsd.Name()
		// skip hidden directories
		if len(ns) > 0 && ns[0] == '.' {
			continue
		}
		log.G(ctx).WithField("namespace", ns).Debug("loading shims in namespace")
		if err := m.loadShims(namespaces.WithNamespace(ctx, ns), stateDir, cfg); err != nil {
			log.G(ctx).WithField("namespace", ns).WithError(err).Error("loading shims in namespace")
			continue
		}
		if err := m.cleanupWorkDirs(namespaces.WithNamespace(ctx, ns), rootDir); err != nil {
			log.G(ctx).WithField("namespace", ns).WithError(err).Error("cleanup working directory in namespace")
			continue
		}
	}
	return nil
}

func (m *ShimManager) loadShims(ctx context.Context, stateDir string, cfg LoadConfig) error {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return err
	}
	ctx = log.WithLogger(ctx, log.G(ctx).WithField("namespace", ns))

	shimDirs, err := os.ReadDir(filepath.Join(stateDir, ns))
	if err != nil {
		return err
	}
	eg, ctx2 := errgroup.WithContext(ctx)
	eg.SetLimit(runtime.GOMAXPROCS(0))
	var errLoad error
	for _, sd := range shimDirs {
		if !sd.IsDir() {
			continue
		}
		id := sd.Name()
		// skip hidden directories
		if len(id) > 0 && id[0] == '.' {
			continue
		}
		bundle, err := LoadBundle(ctx, stateDir, id)
		if err != nil {
			errLoad = err
			// fine to return error here, it is a programmer error if the context
			// does not have a namespace
			break
		}
		eg.Go(func() error {
			// fast path
			f, err := os.Open(bundle.Path)
			if err != nil {
				bundle.Delete()
				log.G(ctx2).WithError(err).Errorf("fast path read bundle path for %s", bundle.Path)
				return nil
			}

			bf, err := f.Readdirnames(-1)
			f.Close()
			if err != nil {
				bundle.Delete()
				log.G(ctx2).WithError(err).Errorf("fast path read bundle path for %s", bundle.Path)
				return nil
			}
			if len(bf) == 0 {
				bundle.Delete()
				return nil
			}
			if err := m.loadShim(ctx2, bundle, cfg); err != nil {
				log.G(ctx2).WithError(err).Errorf("failed to load shim %s", bundle.Path)
				bundle.Delete()
				return nil
			}
			return nil
		})
	}
	_ = eg.Wait()
	return errLoad
}

// Load connects to an already-running shim from an existing bundle and registers
// it with the manager. It is the generic counterpart to [ShimManager.Start] for
// containerd restarts. cfg.Reap, if set, decides whether the loaded shim is kept
// or reaped.
func (m *ShimManager) Load(ctx context.Context, bundle *Bundle, cfg LoadConfig) error {
	if cfg.OnClose == nil {
		return errors.New("LoadConfig.OnClose must not be nil")
	}
	return m.loadShim(ctx, bundle, cfg)
}

func (m *ShimManager) loadShim(ctx context.Context, bundle *Bundle, cfg LoadConfig) error {
	var (
		runtime = cfg.Runtime
		id      = bundle.ID
	)

	// One budget for the whole load: shims are loaded during plugin
	// initialization, so a shim that never answers would otherwise stall
	// containerd startup. Nested timeouts can only shorten a deadline, so this
	// bounds the load however many calls it makes.
	ctx, cancel := timeout.WithContext(ctx, loadTimeout)
	defer cancel()

	// If we're on 1.6+ and specified custom path to the runtime binary, path will be saved in 'shim-binary-path' file.
	if runtime == "" {
		if data, err := os.ReadFile(filepath.Join(bundle.Path, "shim-binary-path")); err == nil {
			runtime = string(data)
		} else if err != nil && !os.IsNotExist(err) {
			log.G(ctx).WithError(err).Error("failed to read `runtime` path from bundle")
		}
	}

	// Fall back to a caller-supplied resolver for very old bundles that predate
	// the shim-binary-path file (e.g. the task manager reads it from the
	// container record).
	if runtime == "" && cfg.RuntimeResolver != nil {
		r, err := cfg.RuntimeResolver(ctx, id)
		if err != nil {
			return err
		}
		runtime = r
	}

	if runtime == "" {
		return fmt.Errorf("no runtime for shim %q: unable to read %q from bundle and none supplied", id, "shim-binary-path")
	}

	if _, err := m.resolveRuntimePath(runtime); err != nil {
		bundle.Delete()

		return fmt.Errorf("failed to resolve runtime path: %w", err)
	}

	onClose := func() {
		log.G(ctx).WithField("id", id).Info("shim disconnected")
		m.shims.Delete(ctx, id)
		cfg.OnClose(id)
	}

	shim, err := loadShim(ctx, bundle, onClose)
	if err != nil {
		// Let the caller clean up a shim that could not be connected to.
		cfg.OnClose(id)
		return fmt.Errorf("unable to load shim %q: %w", id, err)
	}

	// The caller decides whether this shim should be kept or reaped. A Reap
	// that returns keep==false has already torn the shim down.
	if cfg.Reap != nil {
		keep, err := cfg.Reap(ctx, shim)
		if err != nil {
			// Returning an error makes loadShims remove the bundle; a shim we
			// cannot reap would otherwise be reloaded on every start.
			return err
		}
		if !keep {
			return nil
		}
	}

	m.shims.Add(ctx, shim)
	return nil
}

func (m *ShimManager) cleanupWorkDirs(ctx context.Context, rootDir string) error {
	ns, err := namespaces.NamespaceRequired(ctx)
	if err != nil {
		return err
	}

	f, err := os.Open(filepath.Join(rootDir, ns))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()

	dirs, err := f.Readdirnames(-1)
	if err != nil {
		return err
	}

	for _, dir := range dirs {
		// if the task was not loaded, cleanup and empty working directory
		// this can happen on a reboot where /run for the bundle state is cleaned up
		// but that persistent working dir is left
		if _, err := m.shims.Get(ctx, dir); err != nil {
			path := filepath.Join(rootDir, ns, dir)
			if err := os.RemoveAll(path); err != nil {
				log.G(ctx).WithError(err).Errorf("cleanup working dir %s", path)
			}
		}
	}
	return nil
}
