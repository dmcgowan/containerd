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
// (designs/erofs-image-spec/spec.md §2.3).
const (
	// AnnotationChunkIndexRange names the byte range of the chunk index
	// in the blob, in the form "offset[:end]" (half-open interval).
	AnnotationChunkIndexRange = "org.erofs.chunk-index.range"

	// AnnotationChunkIndexDigest is the digest of the chunk-index payload
	// bytes (header + entries, excluding any enclosing frame header).
	// Recommended for index-based integrity verification.
	AnnotationChunkIndexDigest = "org.erofs.chunk-index.digest"

	// AnnotationChunkIndexMediaType identifies the chunk-index format.
	// Defaults to ChunkIndexMediaTypeEROFSV1 when absent.
	AnnotationChunkIndexMediaType = "org.erofs.chunk-index.mediaType"

	// AnnotationChunkIndexTarget is the digest of the layer that a
	// standalone chunk-index layer (mediaType
	// application/vnd.erofs.chunk-index.v1) applies to.
	// When present, consumers MUST verify it matches the descriptor
	// digest of the immediately preceding manifest.layers[] entry.
	// Per erofs-image-spec §3.4.3.
	AnnotationChunkIndexTarget = "org.erofs.chunk-index.target"

	// AnnotationDmVerityHashOffset is the uncompressed byte offset where
	// the dm-verity merkle tree begins.
	AnnotationDmVerityHashOffset = "org.erofs.dmverity.hash_offset"

	// AnnotationDmVerityRootDigest is the merkle tree's root digest.
	AnnotationDmVerityRootDigest = "org.erofs.dmverity.root_digest"

	// AnnotationDmVerityBlockSize is the dm-verity block size in bytes.
	// Defaults to DefaultDmVerityBlockSize when absent.
	AnnotationDmVerityBlockSize = "org.erofs.dmverity.block_size"

	// AnnotationRole tags a layer descriptor for a non-default role in
	// image composition. See erofs-image-spec §2.4.
	// The device role may be applied to any media type; overlay-lower
	// and overlay-data require an EROFS media type.
	AnnotationRole = "org.erofs.role"

	// AnnotationUncompressedDigest is the digest of the layer's
	// uncompressed image data — the physical bytes obtained by fully
	// decompressing the layer blob.
	// For application/vnd.erofs+zstd this is the SHA-256 of the
	// decompressed data stream. For raw application/vnd.erofs this equals
	// the descriptor digest and the annotation may be omitted.
	// This value is identical to the layer's DiffID as defined in
	// erofs-image-spec §5.2. When rootfs.diff_ids is absent from the
	// image configuration, this annotation is the sole source of the
	// per-layer uncompressed digest for ChainID computation.
	// Per erofs-image-spec §2.3 and §5.2.
	AnnotationUncompressedDigest = "org.erofs.uncompressed-digest"
)

// Values for AnnotationRole.
const (
	// RoleDevice marks the layer as a raw byte-source for EROFS
	// multi-device addressing. The runtime decompresses the blob, places
	// it at a predictable path, and passes that path to a consuming EROFS
	// mount via device=. May be applied to any media type.
	RoleDevice = "device"

	// RoleOverlayLower marks the layer as an EROFS image used as an
	// overlayfs lowerdir. Whiteouts and trusted.overlay.opaque xattrs
	// are honored. Requires application/vnd.erofs[+zstd].
	RoleOverlayLower = "overlay-lower"

	// RoleOverlayData marks the layer as an EROFS image carrying file
	// payloads referenced by a higher metadata layer via overlayfs
	// metacopy/redirect (composefs-style). The runtime supplies it as a
	// data-only overlayfs lower. Requires application/vnd.erofs[+zstd].
	RoleOverlayData = "overlay-data"
)

// EROFS layer media types defined by erofs-image-spec §2.1.
const (
	// MediaTypeEROFS is the canonical media type for any valid EROFS
	// filesystem image (raw, uncompressed).
	MediaTypeEROFS = "application/vnd.erofs"

	// MediaTypeEROFSZstd is the canonical media type for a
	// zstd-compressed EROFS filesystem image.
	MediaTypeEROFSZstd = "application/vnd.erofs+zstd"

	// MediaTypeEROFSLayer is the legacy media type for a raw EROFS layer.
	// Deprecated: new producers SHOULD emit MediaTypeEROFS.
	// Consumers MUST treat this as equivalent to MediaTypeEROFS; when not
	// the top layer in manifest.layers[] it implies RoleOverlayLower.
	MediaTypeEROFSLayer = "application/vnd.erofs.layer.v1"

	// MediaTypeEROFSLayerZstd is the legacy media type for a
	// zstd-compressed EROFS layer.
	// Deprecated: new producers SHOULD emit MediaTypeEROFSZstd.
	// Consumers MUST treat this as equivalent to MediaTypeEROFSZstd; when
	// not the top layer in manifest.layers[] it implies RoleOverlayLower.
	MediaTypeEROFSLayerZstd = "application/vnd.erofs.layer.v1+zstd"
)

// Chunk-index media type defined by erofs-image-spec §2.2.
const (
	// ChunkIndexMediaTypeEROFSV1 is the chunk-index format defined in
	// this revision of the spec. The format is not specific to zstd;
	// the CompressionType field in the header records the per-chunk
	// compression used.
	ChunkIndexMediaTypeEROFSV1 = "application/vnd.erofs.chunk-index.v1"
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
// AnnotationDmVerityBlockSize annotation is absent (per erofs-image-spec §2.3).
const DefaultDmVerityBlockSize uint32 = 4096
