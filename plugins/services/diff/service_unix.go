//go:build !windows && !darwin

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

package diff

var defaultDifferConfig = &config{
	// EROFS differ must precede walking: it handles native EROFS layers
	// (application/vnd.erofs[+zstd]) via direct decompress+copy, and also
	// handles tar layers via ConvertTarErofs. Without this ordering, the
	// walking differ receives EROFS blobs and mis-handles them as tar streams.
	// The EROFS differ returns ErrNotImplemented for truly unsupported types
	// so the walking differ still handles non-EROFS cases on plain overlayfs.
	Order:  []string{"erofs", "walking"},
	SyncFs: false,
}
