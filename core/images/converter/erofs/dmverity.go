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
	"fmt"

	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/internal/erofsutils"
)

// stampDmVerityAnnotations writes the standard org.erofs.dmverity.* annotations
// onto annos from the given verity result. It is a no-op when verity is nil.
//
// The set of annotations written is the single source of truth for what the
// dm-verity-aware mount stack reads from a layer descriptor:
//
//   - org.erofs.dmverity.hash-offset  (decimal byte offset of the verity SB)
//   - org.erofs.dmverity.root-digest  (hex root hash)
//   - org.erofs.dmverity.block-size   (decimal, omitted when default 4096)
//
// Centralizing this avoids the previously-duplicated stamping code that drifted
// across erofs.go / chunked.go / merge.go / optimize.go.
func stampDmVerityAnnotations(annos map[string]string, verity *erofsutils.DmVerityResult) {
	if verity == nil || annos == nil {
		return
	}
	annos[contentindex.AnnotationDmVerityHashOffset] = fmt.Sprintf("%d", verity.HashOffset)
	annos[contentindex.AnnotationDmVerityRootDigest] = verity.RootDigest
	if verity.BlockSize != contentindex.DefaultDmVerityBlockSize {
		annos[contentindex.AnnotationDmVerityBlockSize] = fmt.Sprintf("%d", verity.BlockSize)
	}
}
