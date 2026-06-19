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

package local

import (
	"fmt"
	"time"

	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/core/metadata/boltutil"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/go-digest"
	bolt "go.etcd.io/bbolt"
)

// ── blobMeta ──────────────────────────────────────────────────────────────────

// blobMeta holds the scalar fields stored directly in the per-blob bucket.
// Chunk digests and extras are in their own sub-buckets and are read/written
// separately (see readChunkDigests / writeChunkDigests / readExtras / writeExtras).
type blobMeta struct {
	Size             int64
	UncompressedSize int64         // total uncompressed bytes; equals chunk-index header's UncompressedSize
	MediaType        string
	Provider         string
	IndexDigest      digest.Digest // content-store digest of the chunk-index payload entry
	IndexOffset      int64         // start offset of the chunk-index section in the original blob
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// readBlobMeta reads scalar fields from an open per-blob bucket.
func readBlobMeta(bkt *bolt.Bucket) (blobMeta, error) {
	var m blobMeta
	if err := boltutil.ReadTimestamps(bkt, &m.CreatedAt, &m.UpdatedAt); err != nil {
		return blobMeta{}, fmt.Errorf("content/index: read timestamps: %w", err)
	}
	if v := bkt.Get(bucketKeySize); len(v) > 0 {
		m.Size = decodeInt64(v)
	}
	if v := bkt.Get(bucketKeyMediaType); len(v) > 0 {
		m.MediaType = string(v)
	}
	if v := bkt.Get(bucketKeyProvider); len(v) > 0 {
		m.Provider = string(v)
	}
	if v := bkt.Get(bucketKeyIndex); len(v) > 0 {
		dgst, err := digest.Parse(string(v))
		if err != nil {
			return blobMeta{}, fmt.Errorf("content/index: parse index digest: %w", err)
		}
		m.IndexDigest = dgst
	}
	if v := bkt.Get(bucketKeyIndexOffset); len(v) > 0 {
		m.IndexOffset = decodeInt64(v)
	}
	if v := bkt.Get(bucketKeyUncompressedSize); len(v) > 0 {
		m.UncompressedSize = decodeInt64(v)
	}
	return m, nil
}

// writeBlobMeta writes scalar fields into an open per-blob bucket.
func writeBlobMeta(bkt *bolt.Bucket, m blobMeta) error {
	if err := boltutil.WriteTimestamps(bkt, m.CreatedAt, m.UpdatedAt); err != nil {
		return fmt.Errorf("content/index: write timestamps: %w", err)
	}
	if err := bkt.Put(bucketKeySize, encodeInt64(m.Size)); err != nil {
		return err
	}
	if m.MediaType != "" {
		if err := bkt.Put(bucketKeyMediaType, []byte(m.MediaType)); err != nil {
			return err
		}
	}
	if m.Provider != "" {
		if err := bkt.Put(bucketKeyProvider, []byte(m.Provider)); err != nil {
			return err
		}
	}
	if m.IndexDigest != "" {
		if err := bkt.Put(bucketKeyIndex, []byte(m.IndexDigest)); err != nil {
			return err
		}
	}
	if m.IndexOffset > 0 {
		if err := bkt.Put(bucketKeyIndexOffset, encodeInt64(m.IndexOffset)); err != nil {
			return err
		}
	}
	if m.UncompressedSize > 0 {
		if err := bkt.Put(bucketKeyUncompressedSize, encodeInt64(m.UncompressedSize)); err != nil {
			return err
		}
	}
	return nil
}

// ── Labels ────────────────────────────────────────────────────────────────────

// readLabels reads the labels sub-bucket, returning an empty map if absent.
func readLabels(blobBkt *bolt.Bucket) (map[string]string, error) {
	lbls, err := boltutil.ReadLabels(blobBkt)
	if err != nil {
		return nil, fmt.Errorf("content/index: read labels: %w", err)
	}
	if lbls == nil {
		lbls = map[string]string{}
	}
	return lbls, nil
}

// writeLabels replaces the labels sub-bucket atomically.
func writeLabels(blobBkt *bolt.Bucket, labels map[string]string) error {
	if err := boltutil.WriteLabels(blobBkt, labels); err != nil {
		return fmt.Errorf("content/index: write labels: %w", err)
	}
	return nil
}

// updateLabels applies fieldpaths-style updates to the labels sub-bucket.
// An empty fieldpaths slice replaces the whole map.
// "labels" replaces the whole map.
// "labels.<key>" sets or removes a single label.
func updateLabels(blobBkt *bolt.Bucket, fieldpaths []string, updated map[string]string) error {
	if len(fieldpaths) == 0 {
		return writeLabels(blobBkt, updated)
	}
	current, err := readLabels(blobBkt)
	if err != nil {
		return err
	}
	for _, fp := range fieldpaths {
		if fp == "labels" {
			current = updated
			continue
		}
		const prefix = "labels."
		if len(fp) > len(prefix) && fp[:len(prefix)] == prefix {
			key := fp[len(prefix):]
			if v, ok := updated[key]; ok && v != "" {
				current[key] = v
			} else {
				delete(current, key)
			}
		}
	}
	return writeLabels(blobBkt, current)
}

// ── Chunk digests ─────────────────────────────────────────────────────────────

// writeChunkDigests writes the ordered list of per-chunk hashes into the
// chunks sub-bucket of blobBkt.  Each entry's key is an 8-byte big-endian
// sequence number; the value is the digest string.
func writeChunkDigests(blobBkt *bolt.Bucket, dgsts []digest.Digest) error {
	chunksBkt, err := createChunksBucket(blobBkt)
	if err != nil {
		return err
	}
	for i, dgst := range dgsts {
		k := encodeSeq(uint64(i))
		if err := chunksBkt.Put(k[:], []byte(dgst)); err != nil {
			return fmt.Errorf("content/index: write chunk %d digest: %w", i, err)
		}
	}
	return nil
}

// readChunkDigests reads the ordered list of per-chunk hashes from the chunks
// sub-bucket of blobBkt.  Returns an empty slice if the bucket is absent.
func readChunkDigests(blobBkt *bolt.Bucket) ([]digest.Digest, error) {
	chunksBkt := getChunksBucket(blobBkt)
	if chunksBkt == nil {
		return nil, nil
	}
	var dgsts []digest.Digest
	if err := chunksBkt.ForEach(func(k, v []byte) error {
		dgst, err := digest.Parse(string(v))
		if err != nil {
			return fmt.Errorf("content/index: parse chunk digest %q: %w", v, err)
		}
		dgsts = append(dgsts, dgst)
		return nil
	}); err != nil {
		return nil, err
	}
	return dgsts, nil
}

// ── Extras ────────────────────────────────────────────────────────────────────

// extraKind classifies a non-chunk byte range within the original blob.
type extraKind string

const (
	// extraKindIndex is the chunk-index payload (24-byte header + N entries).
	// Its content-store entry digest equals the metadata record's `index` key.
	extraKindIndex extraKind = "index"

	// extraKindFrame is the 8-byte zstd skippable-frame header that precedes
	// the chunk-index payload in +zstd layers.
	extraKindFrame extraKind = "frame"

	// extraKindPadding is zero-byte alignment padding between the last chunk
	// and the chunk-index skippable frame.
	extraKindPadding extraKind = "padding"

	// extraKindHole is any other non-zero gap between chunks (unusual).
	extraKindHole extraKind = "hole"
)

// extra describes one non-chunk byte range needed to reproduce the blob.
//
// Exactly one of Digest or Inline is non-zero:
//   - If compressed size >= inlineThreshold: Digest holds the content-store
//     digest of a zstd-compressed content-store entry for this range.
//   - If compressed size < inlineThreshold: Inline holds the raw
//     zstd-compressed bytes directly (no content-store entry is created).
//
// Decompressing Inline (or the content-store entry at Digest) yields the
// original blob bytes for the range [Offset, Offset+Length).
type extra struct {
	Offset int64
	Length int64
	Kind   extraKind
	Digest digest.Digest // content-store digest; empty when Inline is set
	Inline []byte        // zstd-compressed payload; nil when Digest is set
}

// inlineThreshold is the maximum compressed-extra size that is stored inline
// in the metadata record rather than as a separate content-store entry.
const inlineThreshold = 4096

// writeExtras writes the ordered extras list into the extras sub-bucket.
// Each entry occupies its own subbucket keyed by 8-byte big-endian sequence.
func writeExtras(blobBkt *bolt.Bucket, extras []extra) error {
	if len(extras) == 0 {
		return nil
	}
	extrasBkt, err := createExtrasBucket(blobBkt)
	if err != nil {
		return err
	}
	for i, ex := range extras {
		k := encodeSeq(uint64(i))
		exBkt, err := extrasBkt.CreateBucketIfNotExists(k[:])
		if err != nil {
			return fmt.Errorf("content/index: create extra %d bucket: %w", i, err)
		}
		if err := exBkt.Put(bucketKeyOffset, encodeInt64(ex.Offset)); err != nil {
			return err
		}
		if err := exBkt.Put(bucketKeyLength, encodeInt64(ex.Length)); err != nil {
			return err
		}
		if err := exBkt.Put(bucketKeyKind, []byte(ex.Kind)); err != nil {
			return err
		}
		if ex.Digest != "" {
			if err := exBkt.Put(bucketKeyDigest, []byte(ex.Digest)); err != nil {
				return err
			}
		}
		if len(ex.Inline) > 0 {
			if err := exBkt.Put(bucketKeyInline, ex.Inline); err != nil {
				return err
			}
		}
	}
	return nil
}

// readExtras reads the ordered extras list from the extras sub-bucket.
// Returns nil if the bucket is absent (blob has no extras).
func readExtras(blobBkt *bolt.Bucket) ([]extra, error) {
	extrasBkt := getExtrasBucket(blobBkt)
	if extrasBkt == nil {
		return nil, nil
	}
	var extras []extra
	if err := extrasBkt.ForEach(func(k, v []byte) error {
		// ForEach visits both keys and sub-buckets; skip plain keys.
		if v != nil {
			return nil
		}
		exBkt := extrasBkt.Bucket(k)
		if exBkt == nil {
			return nil
		}
		ex, err := readExtra(exBkt)
		if err != nil {
			return err
		}
		extras = append(extras, ex)
		return nil
	}); err != nil {
		return nil, err
	}
	return extras, nil
}

func readExtra(exBkt *bolt.Bucket) (extra, error) {
	var ex extra
	if v := exBkt.Get(bucketKeyOffset); len(v) > 0 {
		ex.Offset = decodeInt64(v)
	}
	if v := exBkt.Get(bucketKeyLength); len(v) > 0 {
		ex.Length = decodeInt64(v)
	}
	if v := exBkt.Get(bucketKeyKind); len(v) > 0 {
		ex.Kind = extraKind(v)
	}
	if v := exBkt.Get(bucketKeyDigest); len(v) > 0 {
		dgst, err := digest.Parse(string(v))
		if err != nil {
			return extra{}, fmt.Errorf("content/index: parse extra digest: %w", err)
		}
		ex.Digest = dgst
	}
	if v := exBkt.Get(bucketKeyInline); len(v) > 0 {
		ex.Inline = make([]byte, len(v))
		copy(ex.Inline, v)
	}
	return ex, nil
}

// ── Info conversion ───────────────────────────────────────────────────────────

// metaToInfo converts the stored metadata fields into the public Info type.
func metaToInfo(dgst digest.Digest, m blobMeta, lbls map[string]string) contentindex.Info {
	return contentindex.Info{
		Digest:           dgst,
		Size:             m.Size,
		UncompressedSize: m.UncompressedSize,
		MediaType:        m.MediaType,
		IndexDigest:      m.IndexDigest,
		Provider:         m.Provider,
		Labels:           lbls,
		CreatedAt:        m.CreatedAt,
		UpdatedAt:        m.UpdatedAt,
	}
}

// ── DB version ────────────────────────────────────────────────────────────────

// initDBVersion writes the dbVersion varint at
// v1/indexed-content/version if it is not already set.
// This path is distinct from core/metadata's v1/version key,
// avoiding any collision when sharing the metadata BoltDB.
func initDBVersion(tx *bolt.Tx) error {
	bkt, err := createBucketIfNotExists(tx,
		bucketKeyVersion, bucketKeyIndexedContent)
	if err != nil {
		return err
	}
	if bkt.Get(bucketKeyDBVersion) != nil {
		return nil
	}
	return bkt.Put(bucketKeyDBVersion, encodeInt64(int64(dbVersion)))
}

// ── ErrNotFound helper ────────────────────────────────────────────────────────

func blobNotFound(dgst digest.Digest) error {
	return fmt.Errorf("content/index: blob %s: %w", dgst, errdefs.ErrNotFound)
}
