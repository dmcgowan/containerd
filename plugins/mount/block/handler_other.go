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

//go:build !linux

package block

import (
	"context"

	"github.com/containerd/containerd/v2/core/content/index/cache"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	coremount "github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/errdefs"
)

// Handler is a no-op on non-Linux platforms.
type Handler struct{}

// NewHandler returns a Handler that always returns ErrNotImplemented.
func NewHandler(_ contentindex.Store, _ cache.Cache) *Handler {
	return &Handler{}
}

func (h *Handler) Mount(_ context.Context, _ coremount.Mount, _ string, _ []coremount.ActiveMount) (coremount.ActiveMount, error) {
	return coremount.ActiveMount{}, errdefs.ErrNotImplemented
}

func (h *Handler) Unmount(_ context.Context, _ string) error {
	return errdefs.ErrNotImplemented
}

var _ coremount.Handler = (*Handler)(nil)
