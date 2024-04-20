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

// block allows fetching content based on a block tree and referenced
// by the root hash
package block

import (
	"context"

	"github.com/opencontainers/go-digest"
)

type FetchBlock func()
type PutBlock func()

type Info struct {
	Index ocispec.Descriptor
	// Holes
}

type Store interface {
	// Create creates a new block content blob from the descriptor
	// Create options
	//  Source
	//  Make sparse
	//  Allow lazy load
	//  Block on get if unavailable
	Create(context.Context, ocispec.Descriptor) (Info, error)
	Stat(context.Context, digest.Digest) (Info, error)

	// Inherit content store interfaces?
}

type BlockProvider interface {
	// Provides a mount which will create a block device
}

type StreamingFetcher interface {
	// Fetch is used to connect a block fetch function to the content
	FetchStream(context.Context, digest.Digest, FetchBlock) error
}

type StreamingPutter interface {
	// Put is used to put a block
	PutStream(context.Context, digest.Digest, PutBlock) error
}
