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

// pull_lazy_other.go provides a no-op stub for non-Linux platforms.
// Lazy layer ingest requires Linux-specific facilities (EROFS kernel module,
// loop devices).  On other platforms UnpackConfiguration.OnDemand is silently
// ignored and all layers are fetched eagerly.

package local

import (
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes"
)

// lazyLayerHandler returns nil on non-Linux platforms.
// The caller in pull.go treats nil as "use eager fetch instead".
func lazyLayerHandler(
	_ content.Ingester,
	_ remotes.Fetcher,
	_ lazyIndexStore,
	_ LazyCacheWarmer,
	_ string,
	_ credentialProvider,
	_ *ProgressTracker,
) images.Handler {
	return nil
}
