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

package index

// Annotation keys consumed and produced by the indexed content store. These
// mirror the EROFS Image Format Specification's annotation set
// (designs/erofs-image-spec/spec.md §7).
const (
	// AnnotationIndexRange names the byte range of the chunk index in
	// the blob, in the form "offset[:end]" (half-open interval).
	AnnotationIndexRange = "org.erofs.index.range"

	// AnnotationIndexDigest is the digest of the chunk index bytes.
	// REQUIRED for index-based DiffID per erofs-image-spec §5.2.
	AnnotationIndexDigest = "org.erofs.index.digest"

	// AnnotationIndexMediaType identifies the chunk-index format.
	// Defaults to ChunkIndexMediaTypeEROFSv1 when absent.
	AnnotationIndexMediaType = "org.erofs.index.mediaType"

	// AnnotationDmVerityHashOffset is the uncompressed byte offset where
	// the dm-verity merkle tree begins.
	AnnotationDmVerityHashOffset = "org.erofs.dmverity.hash_offset"

	// AnnotationDmVerityRootDigest is the merkle tree's root digest.
	AnnotationDmVerityRootDigest = "org.erofs.dmverity.root_digest"

	// AnnotationDmVerityBlockSize is the dm-verity block size in bytes.
	// Defaults to DefaultDmVerityBlockSize when absent.
	AnnotationDmVerityBlockSize = "org.erofs.dmverity.block_size"
)

// Layer media types defined by erofs-image-spec §2.1.
const (
	MediaTypeEROFSLayer           = "application/vnd.erofs.layer.v1"
	MediaTypeEROFSLayerZstd       = "application/vnd.erofs.layer.v1+zstd"
	MediaTypeEROFSLayerMerged     = "application/vnd.erofs.layer.merged.v1"
	MediaTypeEROFSLayerMergedZstd = "application/vnd.erofs.layer.merged.v1+zstd"
	MediaTypeEROFSLayerData       = "application/vnd.erofs.layer.data.v1"
	MediaTypeEROFSLayerDataZstd   = "application/vnd.erofs.layer.data.v1+zstd"
)

// Chunk-index media types defined by erofs-image-spec §2.2.
const (
	// ChunkIndexMediaTypeEROFSv1 is the default chunk-index format.
	ChunkIndexMediaTypeEROFSv1 = "application/vnd.erofs.index.v1"
)

// GCRefLabel is the GC reference label namespace used by the indexed
// content store. The full label key has the form
//
//	containerd.io/gc.ref.content.index[.<name>] = <digest>
//
// containerd's GC traversal picks up labels under this namespace via the
// metadata.Collector implementation registered by the indexed content
// store plugin; see core/content/index/local/gc.go.
const GCRefLabel = "content.index"

// DefaultDmVerityBlockSize is the dm-verity block size assumed when the
// AnnotationDmVerityBlockSize annotation is absent (per erofs-image-spec §7).
const DefaultDmVerityBlockSize uint32 = 4096
