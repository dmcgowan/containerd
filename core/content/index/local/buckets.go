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
// index) and records blob metadata in buckets inside containerd's shared
// metadata BoltDB.  Only what the GC collector and the blob-reconstruction
// path require is stored here; everything else — chunk offsets, lengths,
// on-blob ranges, dm-verity parameters — is re-read on demand from the
// raw chunk-index entry stored in the content store.
//
// The layout where a "/" delineates a bucket is described below.  Follow
// the same conventions as core/metadata/buckets.go when adding fields.
//
// Conventions:
//   - `╘══*...*` refers to maps with arbitrary keys
//   - All multi-byte integers use signed varint encoding
//     (encoding/binary.PutVarint / binary.Varint)
//   - Timestamps use time.Time.MarshalBinary / time.Time.UnmarshalBinary
//   - Sequence keys (*seq*) are 8-byte big-endian uint64 so that BoltDB
//     cursor iteration visits entries in ascending numeric (ingest) order
//   - All <digest> values are UTF-8 strings, e.g. "sha256:<hex>"
//
// Schema (shares the top-level "v1" bucket with core/metadata)
// └──v1                              – shared schema-version bucket
//
//	├──indexed-content              – indexed-content config (not a ns)
//	│  └──version : <varint>        – indexed-content schema version
//	╘══*namespace*                  – shared with core metadata namespaces
//	   ╘══indexed-content           – indexed-content data for this ns
//	      ╘══blobs
//	         ╘══*blob digest*
//	            ├──createdat : <binary time>
//	            ├──updatedat : <binary time>
//	            ├──size      : <varint>
//	            ├──mediatype : <string>
//	            ├──provider  : <string>
//	            ├──index     : <digest>   – content-store digest of the
//	            │                           chunk-index payload entry;
//	            │                           parse it to get chunk offsets,
//	            │                           lengths, per-chunk hashes, etc.
//	            ├──chunks              – GC reachability: per-chunk digests
//	            │  ╘══*seq* : <digest> – per-chunk hash in index order
//	            ├──extras              – non-chunk byte ranges for
//	            │  ╘══*seq*              byte-exact blob reproduction
//	            │     ├──offset : <varint>
//	            │     ├──length : <varint>
//	            │     ├──kind   : <string>  "index"|"frame"|"padding"|"hole"
//	            │     ├──digest : <digest>  absent when inline is set
//	            │     └──inline : <binary>  absent when digest is set
//	            └──labels
//	               ╘══*key* : <string>
package local

import (
	"encoding/binary"
	"fmt"

	"github.com/opencontainers/go-digest"
	bolt "go.etcd.io/bbolt"
)

// Top-level schema version bucket, indexed-content config bucket, and
// minor-version key.  The "v1" bucket is shared with core/metadata; the
// "indexed-content" sub-bucket under it is owned exclusively by this package.
const (
	schemaVersion = "v1"
	dbVersion     = 1
)

var (
	// bucketKeyVersion is the shared top-level "v1" bucket, same as core/metadata.
	bucketKeyVersion = []byte(schemaVersion)
	// bucketKeyIndexedContent scopes all indexed-content data within "v1",
	// both the config entry ("v1/indexed-content/version") and the per-namespace
	// data ("v1/<ns>/indexed-content/blobs/...").
	bucketKeyIndexedContent = []byte("indexed-content")
	bucketKeyDBVersion      = []byte("version")

	// Object-type buckets (all nested under "v1/<ns>/indexed-content/")
	bucketKeyObjectBlobs  = []byte("blobs")
	bucketKeyObjectChunks = []byte("chunks")
	bucketKeyObjectExtras = []byte("extras")
	bucketKeyObjectLabels = []byte("labels")

	// Provider-metadata config buckets/keys (nested under
	// "v1/indexed-content/", i.e. namespace-independent like version).
	//   v1/indexed-content/providerkey       – AES-256 key (random, 32 bytes)
	//   v1/indexed-content/providers/<name>/ – per-provider record
	//       ref  – registry reference string (plaintext; not a secret)
	//       cred – AES-256-GCM sealed credential (nonce||ciphertext)
	bucketKeyProviderKey     = []byte("providerkey")
	bucketKeyObjectProviders = []byte("providers")
	bucketKeyProviderRef     = []byte("ref")
	bucketKeyProviderCred    = []byte("cred")

	// Field keys inside the per-blob bucket
	bucketKeyCreatedAt        = []byte("createdat")
	bucketKeyUpdatedAt        = []byte("updatedat")
	bucketKeySize             = []byte("size")
	bucketKeyMediaType        = []byte("mediatype")
	bucketKeyProvider         = []byte("provider")
	bucketKeyIndex            = []byte("index")
	bucketKeyIndexOffset      = []byte("indexoffset")
	bucketKeyUncompressedSize = []byte("uncompressedsize")

	// Field keys inside each extras/*seq* subbucket
	bucketKeyOffset = []byte("offset")
	bucketKeyLength = []byte("length")
	bucketKeyKind   = []byte("kind")
	bucketKeyDigest = []byte("digest")
	bucketKeyInline = []byte("inline")
)

// ── Blob bucket helpers ───────────────────────────────────────────────────────

// getBlobsBucket returns the blobs bucket for ns, or nil if absent.
// Path: v1/<ns>/indexed-content/blobs
func getBlobsBucket(tx *bolt.Tx, ns string) *bolt.Bucket {
	return getBucket(tx,
		bucketKeyVersion, []byte(ns),
		bucketKeyIndexedContent, bucketKeyObjectBlobs)
}

// createBlobsBucket creates (or opens) the blobs bucket for ns.
func createBlobsBucket(tx *bolt.Tx, ns string) (*bolt.Bucket, error) {
	return createBucketIfNotExists(tx,
		bucketKeyVersion, []byte(ns),
		bucketKeyIndexedContent, bucketKeyObjectBlobs)
}

// getBlobBucket returns the per-blob bucket for dgst in ns, or nil if absent.
func getBlobBucket(tx *bolt.Tx, ns string, dgst digest.Digest) *bolt.Bucket {
	return getBucket(tx,
		bucketKeyVersion, []byte(ns),
		bucketKeyIndexedContent, bucketKeyObjectBlobs, []byte(dgst))
}

// createBlobBucket creates (or opens) the per-blob bucket for dgst in ns.
func createBlobBucket(tx *bolt.Tx, ns string, dgst digest.Digest) (*bolt.Bucket, error) {
	return createBucketIfNotExists(tx,
		bucketKeyVersion, []byte(ns),
		bucketKeyIndexedContent, bucketKeyObjectBlobs, []byte(dgst))
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
