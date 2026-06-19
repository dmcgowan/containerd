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

package images

import (
	"flag"
	"fmt"
	"os"
	"sync"

	"github.com/containerd/log"
	"github.com/pelletier/go-toml/v2"
)

var imageListFile = flag.String("image-list", "", "The TOML file containing the non-default images to be used in tests.")

// ImageList holds public image references
type ImageList struct {
	Alpine           string
	BusyBox          string
	BusyBoxByDigest  string
	Pause            string
	ResourceConsumer string
	VolumeCopyUp     string
	VolumeOwnership  string
	ArgsEscaped      string
	Nginx            string
	Whiteout         string

	// EROFS images — converted versions of the classic images above.
	// These are dual-format OCI image indexes: the original tar-based
	// manifests come first (for backward compat) followed by EROFS variants
	// annotated with os.features=["erofs"].
	//
	// The *Merge variants collapse all layers into a single merged EROFS
	// image (no overlay chain), which is the preferred form for the EROFS
	// snapshotter's fast-path.
	//
	// Default refs point to docker.io/dmcgowan/ which hosts pre-converted
	// reference images.  Override with -image-list to point at a local
	// registry during development or CI.
	ErofsAlpine      string
	ErofsBusyBox     string
	ErofsPause       string
	ErofsAlpineMerge string
	ErofsBusyBoxMerge string
}

var (
	imageMap  map[int]string
	imageList ImageList
)

var initOnce sync.Once

func initImages(imageListFile string) {
	imageList = ImageList{
		Alpine:           "ghcr.io/containerd/alpine:3.14.0",
		BusyBox:          "ghcr.io/containerd/busybox:1.36",
		BusyBoxByDigest:  "ghcr.io/containerd/busybox@sha256:7b3ccabffc97de872a30dfd234fd972a66d247c8cfc69b0550f276481852627c",
		Pause:            "registry.k8s.io/pause:3.10.2",
		ResourceConsumer: "registry.k8s.io/e2e-test-images/resource-consumer:1.10",
		VolumeCopyUp:     "ghcr.io/containerd/volume-copy-up:2.2",
		VolumeOwnership:  "ghcr.io/containerd/volume-ownership:2.1",
		ArgsEscaped:      "cplatpublic.azurecr.io/args-escaped-test-image-ns:1.0",
		Nginx:            "ghcr.io/containerd/nginx:1.27.0",
		Whiteout:         "ghcr.io/containerd/whiteout-test:1.0",
	}

	if imageListFile != "" {
		log.L.Infof("loading image list from file: %s", imageListFile)

		fileContent, err := os.ReadFile(imageListFile)
		if err != nil {
			panic(fmt.Errorf("error reading '%v' file contents: %v", imageList, err))
		}

		err = toml.Unmarshal(fileContent, &imageList)
		if err != nil {
			panic(fmt.Errorf("error unmarshalling '%v' TOML file: %v", imageList, err))
		}
	}

	// Back-fill EROFS image refs that were not set by the image-list file.
	// These are derived from the canonical tar images by appending ":erofs-demo"
	// / ":erofs-merge" tags at the same registry used for the tar originals.
	if imageList.ErofsAlpine == "" {
		imageList.ErofsAlpine = "docker.io/dmcgowan/alpine:erofs-demo"
	}
	if imageList.ErofsBusyBox == "" {
		imageList.ErofsBusyBox = "docker.io/dmcgowan/busybox:erofs-demo"
	}
	if imageList.ErofsPause == "" {
		imageList.ErofsPause = "docker.io/dmcgowan/pause:erofs-demo"
	}
	if imageList.ErofsAlpineMerge == "" {
		imageList.ErofsAlpineMerge = "docker.io/dmcgowan/alpine:erofs-merge"
	}
	if imageList.ErofsBusyBoxMerge == "" {
		imageList.ErofsBusyBoxMerge = "docker.io/dmcgowan/busybox:erofs-merge"
	}

	log.L.Infof("Using the following image list: %+v", imageList)
	imageMap = initImageMap(imageList)
}

const (
	// None is to be used for unset/default images
	None = iota
	// Alpine image
	Alpine
	// BusyBox image
	BusyBox
	// BusyBox by digest
	BusyBoxByDigest
	// Pause image
	Pause
	// ResourceConsumer image
	ResourceConsumer
	// VolumeCopyUp image
	VolumeCopyUp
	// VolumeOwnership image
	VolumeOwnership
	// ArgsEscaped tests image for ArgsEscaped windows bug
	ArgsEscaped
	// Nginx image
	Nginx
	// Whiteout image
	Whiteout

	// ErofsAlpine is a dual-format index with an EROFS variant of Alpine.
	ErofsAlpine
	// ErofsBusyBox is a dual-format index with an EROFS variant of BusyBox.
	ErofsBusyBox
	// ErofsPause is a dual-format index with an EROFS variant of Pause.
	ErofsPause
	// ErofsAlpineMerge is a single-layer merged EROFS image of Alpine.
	ErofsAlpineMerge
	// ErofsBusyBoxMerge is a single-layer merged EROFS image of BusyBox.
	ErofsBusyBoxMerge
)

func initImageMap(imageList ImageList) map[int]string {
	images := map[int]string{}
	images[Alpine] = imageList.Alpine
	images[BusyBox] = imageList.BusyBox
	images[BusyBoxByDigest] = imageList.BusyBoxByDigest
	images[Pause] = imageList.Pause
	images[ResourceConsumer] = imageList.ResourceConsumer
	images[VolumeCopyUp] = imageList.VolumeCopyUp
	images[VolumeOwnership] = imageList.VolumeOwnership
	images[ArgsEscaped] = imageList.ArgsEscaped
	images[Nginx] = imageList.Nginx
	images[Whiteout] = imageList.Whiteout
	images[ErofsAlpine] = imageList.ErofsAlpine
	images[ErofsBusyBox] = imageList.ErofsBusyBox
	images[ErofsPause] = imageList.ErofsPause
	images[ErofsAlpineMerge] = imageList.ErofsAlpineMerge
	images[ErofsBusyBoxMerge] = imageList.ErofsBusyBoxMerge
	return images
}

// Get returns the fully qualified URI to an image (including version)
func Get(image int) string {
	initOnce.Do(func() {
		initImages(*imageListFile)
	})

	return imageMap[image]
}
