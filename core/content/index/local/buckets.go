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

// Package local is the in-tree implementation of the indexed content
// store (core/content/index).
//
// It stores chunk content as entries in containerd's existing content
// store (one entry per chunk, keyed by the per-chunk hash from the chunk
// index) and tracks blob → reachability mappings in a sidecar BoltDB
// database. The sidecar is intentionally minimal: it holds only what the
// GC collector and the blob-reconstruction path require. Everything else
// — chunk offsets, lengths, on-blob ranges, weights, dm-verity parameters
// — is re-read on demand from the raw chunk-index entry stored in the
// content store.
//
// The layout where a "/" delineates a bucket is described below. Follow
// the same conventions as core/metadata/buckets.go when adding fields.
//
// Conventions:
//   - `╘══*...*` refers to maps with arbitrary keys
//   - `version` is a key to a numeric value identifying the minor revisions
//     of schema version; a namespace cannot be named "version"
//   - All multi-byte integers use signed varint encoding
//     (encoding/binary.PutVarint / binary.Varint)
//   - Timestamps use time.Time.MarshalBinary / time.Time.UnmarshalBinary
//   - Sequence keys (*seq*) are 8-byte big-endian uint64 so that BoltDB
//     cursor iteration visits entries in ascending numeric (ingest) order
//   - All <digest> values are UTF-8 strings, e.g. "sha256:<hex>"
//
// Schema
// └──v1                                                  – Schema version bucket
//    ├──version : <varint>                               – DB minor version; see migrations
//    ╘══*namespace*
//       ╘══blobs                                         – Indexed-content blobs
//          ╘══*blob digest*
//             ├──createdat : <binary time>               – Created at
//             ├──updatedat : <binary time>               – Updated at
//             ├──size      : <varint>                    – Total blob size in bytes
//             ├──mediatype : <string>                    – OCI layer media type
//             ├──provider  : <string>                    – Byte-provider name (optional)
//             ├──index     : <digest>                    – Content-store digest of the chunk-index
//             │                                            entry (24-byte header + N entries).
//             │                                            Open this entry and parse it to get
//             │                                            chunk offsets, lengths, weights, etc.
//             │                                            Equals org.erofs.index.digest when
//             │                                            SHA-256 is the index hash algorithm.
//             ├──chunks                                  – GC reachability: per-chunk digests
//             │  ╘══*seq* : <digest>                     – Per-chunk hash (= content-store digest),
//             │                                            in chunk-index order. These are the only
//             │                                            chunk fields stored in the sidecar; all
//             │                                            other chunk metadata comes from the index
//             │                                            entry above.
//             ├──extras                                  – Non-chunk byte ranges needed to reproduce
//             │  ╘══*seq*                                – the original blob byte-for-byte.
//             │     ├──offset : <varint>                 – Byte offset in original blob
//             │     ├──length : <varint>                 – Byte length in original blob
//             │     ├──kind   : <string>                 – "index"   – chunk-index payload bytes
//             │     │                                      "frame"   – zstd skippable-frame header
//             │     │                                      "padding" – zero-alignment padding
//             │     │                                      "hole"    – arbitrary inter-chunk gap
//             │     ├──digest : <digest>                 – sha256(zstd(extra bytes)); absent when
//             │     │                                      the extra is stored inline
//             │     └──inline : <binary>                 – zstd-compressed payload; absent when
//             │                                            digest is set (content-store entry used)
//             └──labels                                  – Mutable operator labels
//                ╘══*key* : <string>                     – Label value
package local

import (
	"encoding/binary"
	"fmt"

	"github.com/opencontainers/go-digest"
	bolt "go.etcd.io/bbolt"
)

// Top-level schema version bucket and minor-version key.
const (
	schemaVersion = "v1"
	dbVersion     = 1
)

var (
	bucketKeyVersion   = []byte(schemaVersion)
	bucketKeyDBVersion = []byte("version")

	// Object-type buckets
	bucketKeyObjectBlobs  = []byte("blobs")
	bucketKeyObjectChunks = []byte("chunks")
	bucketKeyObjectExtras = []byte("extras")
	bucketKeyObjectLabels = []byte("labels")

	// Field keys inside the per-blob bucket
	bucketKeyCreatedAt  = []byte("createdat")
	bucketKeyUpdatedAt  = []byte("updatedat")
	bucketKeySize       = []byte("size")
	bucketKeyMediaType  = []byte("mediatype")
	bucketKeyProvider   = []byte("provider")
	bucketKeyIndex      = []byte("index")

	// Field keys inside each extras/*seq* subbucket
	bucketKeyOffset = []byte("offset")
	bucketKeyLength = []byte("length")
	bucketKeyKind   = []byte("kind")
	bucketKeyDigest = []byte("digest")
	bucketKeyInline = []byte("inline")
)

// ── Blob bucket helpers ───────────────────────────────────────────────────────

// getBlobsBucket returns the blobs bucket for ns, or nil if absent.
func getBlobsBucket(tx *bolt.Tx, ns string) *bolt.Bucket {
	return getBucket(tx, bucketKeyVersion, []byte(ns), bucketKeyObjectBlobs)
}

// createBlobsBucket creates (or opens) the blobs bucket for ns.
func createBlobsBucket(tx *bolt.Tx, ns string) (*bolt.Bucket, error) {
	return createBucketIfNotExists(tx, bucketKeyVersion, []byte(ns), bucketKeyObjectBlobs)
}

// getBlobBucket returns the per-blob bucket for dgst in ns, or nil if absent.
func getBlobBucket(tx *bolt.Tx, ns string, dgst digest.Digest) *bolt.Bucket {
	return getBucket(tx, bucketKeyVersion, []byte(ns), bucketKeyObjectBlobs, []byte(dgst))
}

// createBlobBucket creates (or opens) the per-blob bucket for dgst in ns.
func createBlobBucket(tx *bolt.Tx, ns string, dgst digest.Digest) (*bolt.Bucket, error) {
	return createBucketIfNotExists(tx,
		bucketKeyVersion, []byte(ns), bucketKeyObjectBlobs, []byte(dgst))
}

// ── Sub-bucket helpers (operate on an already-opened blob bucket) ─────────────

// getChunksBucket returns the chunks sub-bucket of blobBkt, or nil.
func getChunksBucket(blobBkt *bolt.Bucket) *bolt.Bucket {
	return blobBkt.Bucket(bucketKeyObjectChunks)
}

// createChunksBucket creates (or opens) the chunks sub-bucket of blobBkt.
func createChunksBucket(blobBkt *bolt.Bucket) (*bolt.Bucket, error) {
	return blobBkt.CreateBucketIfNotExists(bucketKeyObjectChunks)
}

// getExtrasBucket returns the extras sub-bucket of blobBkt, or nil.
func getExtrasBucket(blobBkt *bolt.Bucket) *bolt.Bucket {
	return blobBkt.Bucket(bucketKeyObjectExtras)
}

// createExtrasBucket creates (or opens) the extras sub-bucket of blobBkt.
func createExtrasBucket(blobBkt *bolt.Bucket) (*bolt.Bucket, error) {
	return blobBkt.CreateBucketIfNotExists(bucketKeyObjectExtras)
}

// getLabelsBucket returns the labels sub-bucket of blobBkt, or nil.
func getLabelsBucket(blobBkt *bolt.Bucket) *bolt.Bucket {
	return blobBkt.Bucket(bucketKeyObjectLabels)
}

// createLabelsBucket creates (or opens) the labels sub-bucket of blobBkt.
func createLabelsBucket(blobBkt *bolt.Bucket) (*bolt.Bucket, error) {
	return blobBkt.CreateBucketIfNotExists(bucketKeyObjectLabels)
}

// ── Key encoding ─────────────────────────────────────────────────────────────

// encodeSeq encodes n as an 8-byte big-endian key.  BoltDB's byte-order
// cursor then visits entries in ascending numeric order.
func encodeSeq(n uint64) [8]byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], n)
	return b
}

// encodeInt64 encodes v as a signed varint (same scheme as core/metadata).
func encodeInt64(v int64) []byte {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutVarint(buf[:], v)
	return buf[:n]
}

// decodeInt64 decodes a signed varint produced by encodeInt64.
func decodeInt64(b []byte) int64 {
	v, _ := binary.Varint(b)
	return v
}

// ── Generic bucket traversal helpers ─────────────────────────────────────────

// getBucket traverses keys from the transaction root, returning nil if any
// level is absent.
func getBucket(tx *bolt.Tx, keys ...[]byte) *bolt.Bucket {
	bkt := tx.Bucket(keys[0])
	for _, key := range keys[1:] {
		if bkt == nil {
			return nil
		}
		bkt = bkt.Bucket(key)
	}
	return bkt
}

// createBucketIfNotExists creates each bucket in keys, returning the leaf.
func createBucketIfNotExists(tx *bolt.Tx, keys ...[]byte) (*bolt.Bucket, error) {
	bkt, err := tx.CreateBucketIfNotExists(keys[0])
	if err != nil {
		return nil, err
	}
	for _, key := range keys[1:] {
		bkt, err = bkt.CreateBucketIfNotExists(key)
		if err != nil {
			return nil, fmt.Errorf("content/index: create bucket %q: %w", key, err)
		}
	}
	return bkt, nil
}
