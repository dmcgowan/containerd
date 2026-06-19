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

// Package provider contains the process-local registry of ByteProvider
// instances and the Factory type used to (re-)create them after a daemon
// restart.
//
// The Registry is a singleton-per-process that maps provider names to their
// live ByteProvider implementation. The indexed content store records the
// provider name in the metadata record (Info.Provider) at lazy-ingest time;
// on a subsequent daemon restart the mount manager or cache layer calls
// Registry.Get(name) to retrieve the provider needed to fill missing chunks.
package provider

import (
	"fmt"
	"sync"

	contentindex "github.com/containerd/containerd/v2/core/content/index"
)

// Factory creates a ByteProvider on demand. Implementations should be cheap
// to call: they may be invoked after a daemon restart to restore a previously
// registered provider by name.
type Factory func() (contentindex.ByteProvider, error)

// Registry is a process-local registry of ByteProvider instances, keyed by
// the name the provider returns from its Name() method.
//
// A Registry is safe for concurrent use.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]contentindex.ByteProvider
	factories map[string]Factory
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		providers: make(map[string]contentindex.ByteProvider),
		factories: make(map[string]Factory),
	}
}

// Register adds p to the registry under its Name(). If a provider with the
// same name is already registered, it is replaced.
func (r *Registry) Register(p contentindex.ByteProvider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[p.Name()] = p
}

// Unregister removes the provider registered under name.  It is a no-op if
// no provider with that name exists.  Intended primarily for test cleanup.
func (r *Registry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.providers, name)
}

// RegisterFactory stores a factory under name. When Get(name) is called and
// no live provider is registered under name, the factory is called to create
// one, which is then cached.
func (r *Registry) RegisterFactory(name string, f Factory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[name] = f
}

// Get returns the ByteProvider registered under name. If no live provider
// exists but a factory was registered under name, the factory is called once
// and the resulting provider is cached.
//
// Returns an error if neither a provider nor a factory is registered for name.
func (r *Registry) Get(name string) (contentindex.ByteProvider, error) {
	r.mu.RLock()
	p, ok := r.providers[name]
	r.mu.RUnlock()
	if ok {
		return p, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Re-check under write lock.
	if p, ok = r.providers[name]; ok {
		return p, nil
	}
	f, ok := r.factories[name]
	if !ok {
		return nil, fmt.Errorf("content/index/provider: no provider registered for %q", name)
	}
	p, err := f()
	if err != nil {
		return nil, fmt.Errorf("content/index/provider: factory for %q: %w", name, err)
	}
	r.providers[name] = p
	return p, nil
}

// Global is the process-wide default Registry. Components that cannot share a
// Registry value directly (e.g. plugins wired at startup) use this.
var Global = NewRegistry()
