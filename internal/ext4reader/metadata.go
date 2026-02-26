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

package ext4reader

import (
	"fmt"
	"io/fs"

	"github.com/containerd/containerd/v2/internal/fstar"
)

// NewMetadataFunc returns a MetadataFunc that extracts FileMetadata from
// an ext4reader filesystem's FileInfo.Sys() value (*InodeInfo).
func NewMetadataFunc() fstar.MetadataFunc {
	return func(info fs.FileInfo) (*fstar.FileMetadata, error) {
		ii, ok := info.Sys().(*InodeInfo)
		if !ok {
			return nil, fmt.Errorf("unexpected Sys() type %T, expected *ext4reader.InodeInfo", info.Sys())
		}
		return &fstar.FileMetadata{
			Ino:        ii.Ino,
			UID:        ii.UID,
			GID:        ii.GID,
			Mtime:      ii.Mtime,
			Nlink:      ii.Nlink,
			DevMajor:   ii.DevMajor,
			DevMinor:   ii.DevMinor,
			LinkTarget: ii.LinkTarget,
			Xattrs:     ii.Xattrs,
		}, nil
	}
}
