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

// Package index defines the interfaces for the indexed content store, a
// containerd service that manages OCI blobs carrying an internal chunk index
// over their bytes.
//
// The indexed content store is a peer of the existing content store: it
// reuses the content store as its byte-storage primitive but adds a sidecar
// metadata database that records, for each indexed-content blob, the minimal
// reachability metadata the GC needs plus the extras required for byte-exact
// blob reproduction.
//
// Specifically the sidecar tracks:
//
//   - The content-store digest of the chunk-index entry (IndexDigest): opening
//     this entry and parsing it yields all chunk offsets, lengths, on-blob
//     ranges, weights, and per-chunk hashes. The chunk-index entry is stored
//     in the content store under sha256(chunk-index payload bytes), which
//     equals the org.erofs.index.digest annotation on the descriptor.
//
//   - A flat ordered list of per-chunk content-store digests (one per chunk,
//     in chunk-index order). These are the only thing the GC needs from the
//     chunk index — they let the GC mark every chunk content-store entry as
//     reachable without re-parsing the chunk-index entry on every GC pass.
//
//   - An ordered list of extras: non-chunk byte ranges (skippable-frame
//     headers, chunk-index payload bytes, zero-padding) that, together with
//     the chunks, cover every byte of the original blob. Extras are stored
//     either inline (zstd-compressed, when small) or as content-store entries
//     (zstd-compressed, when large). Together, chunks + extras let the store
//     reproduce the original blob byte-for-byte from the sidecar alone.
//
// dm-verity parameters (hash offset, root digest, block size) live on the
// layer descriptor's org.erofs.dmverity.* annotations and are never duplicated
// in the sidecar. Callers that need dm-verity information pass the descriptor.
//
// This package defines the abstract interface only. A local implementation
// is provided in the local/ subpackage and registered as the
// "io.containerd.content.index.v1" plugin.
//
// See designs/indexed-content.md and designs/indexed-content-service.md for
// the full design rationale, and designs/erofs-image-spec/spec.md for the
// EROFS image format specification this package consumes.
package index

import (
	"context"
	"time"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Store is the top-level interface for the indexed content store.
//
// Lifecycle:
//   - Ingester is used to initiate a write of a complete indexed-content
//     blob. The producer streams the blob bytes to the returned
//     content.Writer; on Commit the store extracts the chunk-index entry
//     and all chunks into the content store, identifies any extra byte
//     ranges needed for exact reproduction, and records the sidecar record.
//   - Once an ingest is complete, Provider is used to read the blob
//     (sequentially, by reassembling chunks + extras) or to obtain Mounts()
//     specs.
//   - Manager is used to inspect, list, label, and delete entries.
//
// Cross-store references from core containerd objects (Image, Manifest,
// Snapshot, Container, Active Mount) are annotation-only: a label of the
// form
//
//	containerd.io/gc.ref.content.index[.<name>] = <digest>
//
// pins the named blob (and transitively all of its chunk and index
// content store entries) for the lifetime of the carrying object. The store
// registers a metadata.Collector for these labels so containerd's existing
// label-based GC walks them automatically.
type Store interface {
	Manager
	Provider
	Ingester
}

// Manager provides methods for inspecting, listing, and removing
// indexed-content entries. Mirrors the shape of content.Manager.
type Manager interface {
	InfoProvider

	// Update modifies mutable information related to an entry.
	// Mutable fields:
	//   labels.*
	Update(ctx context.Context, info Info, fieldpaths ...string) (Info, error)

	// Walk iterates entries, optionally filtered by label expressions.
	Walk(ctx context.Context, fn WalkFunc, filters ...string) error

	// Delete removes an entry. Chunks and extras pinned solely by the
	// entry are unreferenced and may be collected by the next
	// content-store GC pass.
	Delete(ctx context.Context, dgst digest.Digest) error
}

// InfoProvider returns metadata for a stored entry.
type InfoProvider interface {
	// Info returns metadata about an indexed-content blob. The returned
	// Info contains only what is stored in the sidecar (digest, size,
	// media type, IndexDigest, provider, labels, timestamps). Chunk
	// offsets and lengths are not included; open and parse the chunk-index
	// content-store entry (keyed by IndexDigest) to get them.
	//
	// Returns errdefs.ErrNotFound if the digest is not known to the store.
	Info(ctx context.Context, dgst digest.Digest) (Info, error)
}

// Provider provides access to indexed-content blob bytes and mounts.
type Provider interface {
	// ReaderAt returns a reader over the original blob bytes assembled
	// from the chunk-index entry plus per-chunk and extra content-store
	// entries. The reader reproduces the blob byte-for-byte, including
	// the embedded chunk index and any skippable-frame framing.
	//
	// The descriptor must carry the org.erofs.index.* annotations so
	// the reader can locate the chunk-index section within the blob.
	// Only desc.Digest and desc.Annotations are required.
	ReaderAt(ctx context.Context, desc ocispec.Descriptor) (content.ReaderAt, error)

	// Mounts returns mount specs that, when activated, expose the blob as
	// a kernel-mountable filesystem (loop, cachefiles, etc.). The
	// returned specs are intended to be composed by a snapshotter or
	// passed directly to the mount manager.
	//
	// Returns errdefs.ErrNotImplemented for stores that do not provide
	// kernel-side delivery.
	Mounts(ctx context.Context, dgst digest.Digest) ([]mount.Mount, error)
}

// Ingester writes a new indexed-content blob.
type Ingester interface {
	// Writer initiates an ingest. The producer streams the complete blob
	// bytes (chunk index and any skippable-frame framing included) to the
	// returned Writer. On Commit the store:
	//   1. Verifies the digest of the streamed bytes against the expected
	//      descriptor digest.
	//   2. Locates the chunk index using the descriptor's
	//      org.erofs.index.* annotations.
	//   3. Ingests the chunk-index payload as a content-store entry under
	//      sha256(payload), which equals org.erofs.index.digest.
	//   4. Extracts each chunk into the content store under its per-chunk
	//      hash (the chunk's content-store digest).
	//   5. Identifies extra byte ranges (skippable-frame headers, padding,
	//      the chunk-index payload itself) that are needed for byte-exact
	//      reproduction; stores them compressed, inline when small.
	//   6. Records the sidecar entry: IndexDigest, ordered chunk-digest
	//      list, and extras list.
	//
	// Writer requires that the descriptor:
	//   - Have a non-empty Digest (the expected blob digest).
	//   - Have annotations naming the chunk-index range (org.erofs.index.range).
	//   - Use a chunk-index format with per-chunk checksums (HashAlgo != 0).
	Writer(ctx context.Context, opts ...content.WriterOpt) (content.Writer, error)
}

// ByteProvider sources the bytes of an indexed-content blob from a
// non-local location (registry, cloud volume, peer-to-peer source).
// Providers are registered as "io.containerd.content.index.provider.v1"
// plugins.
//
// Providers do not cache: their job is to expose a ReaderAt over a blob
// the store can use to drive ingest or to lazily fetch missing chunks.
// Caching, hash verification, and chunk extraction are handled by the
// indexed content store itself.
type ByteProvider interface {
	// Name returns a stable identifier used in operator-visible records
	// (Info.Provider) and in plugin registration logs.
	Name() string

	// Open returns a ReaderAt over the bytes of the named blob, plus the
	// blob's total size. Returns errdefs.ErrNotFound if the provider does
	// not know how to source the blob.
	Open(ctx context.Context, desc ocispec.Descriptor) (content.ReaderAt, error)
}

// Info holds the sidecar metadata for an indexed-content blob.
//
// Only what is stored in the sidecar is surfaced here. Chunk offsets,
// lengths, on-blob ranges, weights, and dm-verity parameters are NOT
// included. To access them:
//
//   - Chunk offsets / lengths / on-blob ranges / weights: open and parse
//     the chunk-index content-store entry keyed by IndexDigest.
//   - dm-verity parameters: read the org.erofs.dmverity.* annotations on
//     the layer descriptor (the caller always has the descriptor).
type Info struct {
	// Digest is the blob's descriptor digest (logical identity).
	Digest digest.Digest

	// Size is the blob's total size in bytes (the entire on-wire blob,
	// including any embedded chunk index and skippable-frame framing).
	Size int64

	// MediaType is the OCI media type from the blob's descriptor.
	MediaType string

	// IndexDigest is the content-store digest of the chunk-index entry.
	// The entry contains the raw chunk-index payload bytes (24-byte header
	// followed by N chunk entries). Open it from the content store and
	// parse it with erofs_index.go's helpers to get chunk details.
	//
	// This value equals the org.erofs.index.digest descriptor annotation
	// when SHA-256 is the hash algorithm.
	IndexDigest digest.Digest

	// Provider names the byte source used at ingest time
	// (e.g. "local-content", "registry"). Empty for purely local ingests.
	Provider string

	// Labels are mutable operator labels. Labels with the
	// "containerd.io/gc.ref.*" namespace are honored by the GC traversal.
	Labels map[string]string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// ChunkRef identifies a parsed chunk from the chunk-index entry. It is
// returned by callers that open and parse the chunk-index content-store
// entry; it is not stored in the sidecar.
type ChunkRef struct {
	// Digest is the chunk's content-store digest, equal to the chunk's
	// per-chunk checksum from the chunk index. Two blobs that share a
	// chunk produce the same Digest, which is what gives the indexed
	// content store its cross-image dedup property.
	Digest digest.Digest

	// Offset is the chunk's logical (uncompressed) offset within the
	// blob's image data section.
	Offset int64

	// Length is the chunk's logical (uncompressed) length.
	Length int64

	// OnBlobStart is the chunk's byte offset within the original blob.
	// For raw layers this equals Offset; for +zstd layers it is the
	// offset of the chunk's compressed zstd frame.
	OnBlobStart int64

	// OnBlobEnd is the chunk's exclusive end offset within the original
	// blob.
	OnBlobEnd int64
}

// DmVerityInfo carries the dm-verity merkle tree parameters per
// erofs-image-spec §7. This type is provided for callers that need to
// parse dm-verity annotations from a descriptor; it is NOT stored in the
// sidecar. Use parseDmVerity (in local/erofs_index.go) to populate it
// from a descriptor's org.erofs.dmverity.* annotations.
type DmVerityInfo struct {
	// RootDigest is the merkle tree's root digest.
	RootDigest digest.Digest

	// HashOffset is the uncompressed byte offset where the merkle tree
	// begins (equivalently, the size of the EROFS filesystem image).
	HashOffset int64

	// BlockSize is the dm-verity data and hash block size in bytes.
	// Defaults to 4096 when omitted from the source annotations.
	BlockSize uint32
}

// WalkFunc is the callback for Walk.
type WalkFunc func(Info) error
