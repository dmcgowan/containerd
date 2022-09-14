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

package transfer

import (
	"context"
	"io"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/pkg/unpack"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

type Transferer interface {
	Transfer(context.Context, interface{}, interface{}, ...Opt) error
}

type ImageResolver interface {
	Resolve(ctx context.Context) (name string, desc ocispec.Descriptor, err error)
}

type ImageFetcher interface {
	ImageResolver

	Fetcher(ctx context.Context, ref string) (Fetcher, error)
}

type ImagePusher interface {
	Pusher(context.Context, ocispec.Descriptor) (Pusher, error)
}

type Fetcher interface {
	Fetch(context.Context, ocispec.Descriptor) (io.ReadCloser, error)
}

type Pusher interface {
	Push(context.Context, ocispec.Descriptor) (content.Writer, error)
}

// ImageFilterer is used to filter out child objects of an image
type ImageFilterer interface {
	ImageFilter(images.HandlerFunc, content.Store) images.HandlerFunc
}

// ImageStorer is a type which is capable of storing an image to
// for a provided descriptor
type ImageStorer interface {
	Store(context.Context, ocispec.Descriptor, images.Store) (images.Image, error)
}

// ImageGetter is type which returns an image from an image store
type ImageGetter interface {
	Get(context.Context, images.Store) (images.Image, error)
}

// ImageImportStreamer returns an import streamer based on OCI or
// Docker image tar archives. The stream should be a raw tar stream
// and without compression.
type ImageImportStreamer interface {
	ImportStream(context.Context) (io.Reader, error)
}

type ImageExportStreamer interface {
	ExportStream(context.Context) (io.WriteCloser, error)
}

type ImageUnpacker interface {
	// TODO: Or unpack options?
	UnpackPlatforms() []unpack.Platform
}

type ProgressFunc func(Progress)

type Config struct {
	Progress ProgressFunc
}

type Opt func(*Config)

func WithProgress(f ProgressFunc) Opt {
	return func(opts *Config) {
		opts.Progress = f
	}
}

type Progress struct {
	Event    string
	Name     string
	Parents  []string
	Progress int64
	Total    int64
	// Descriptor?
}

/*
// Distribution options
// Stream handler
// Progress rate
// Unpack options
// Remote options
// Cases:
//  Registry -> Content/ImageStore (pull)
//  Registry -> Registry
//  Content/ImageStore -> Registry (push)
//  Content/ImageStore -> Content/ImageStore (tag)
// Common fetch/push interface for registry, content/imagestore, OCI index
// Always starts with string for source and destination, on client side, does not need to resolve
//  Higher level implementation just takes strings and options
//  Lower level implementation takes pusher/fetcher?

*/
