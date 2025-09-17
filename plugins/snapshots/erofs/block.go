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

	"github.com/containerd/errdefs"
)

func createWritableImage(filename string, size int64) error {
	return fmt.Errorf("writable image not yet supported: %w", errdefs.ErrNotImplemented)
	/*
			f, err := file.CreateFromPath(filename, size)
			if err != nil {
				return err
			}
			defer f.Close()
			//d, err := diskfs.Create(filename, size, diskfs.SectorSizeDefault)
			//d.Size = size
			//fs, err := d.CreateFilesystem(disk.FilesystemSpec{FSType: filesystem.TypeExt4})

			fs, err := ext4.Create(f, size, 0, 0, nil)
			if err != nil {
				return err
			}
			defer fs.Close()

			err = fs.Mkdir("/work")
			if err != nil {
				return err
			}
			err = fs.Mkdir("/fs")
			if err != nil {
				return err
			}

		return nil
	*/
}
