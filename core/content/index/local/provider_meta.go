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

// Registry-provider metadata persistence.
//
// A lazily-ingested blob records the name of the ByteProvider that can fetch
// its chunks (Info.Provider).  To let chunk filling survive a daemon restart,
// the provider's reconstruction metadata is persisted in the same
// indexed-content bolt DB as the rest of the index metadata:
//
//	v1/indexed-content/providerkey            – AES-256 key (32 random bytes)
//	v1/indexed-content/providers/<name>/ref   – registry reference (plaintext)
//	v1/indexed-content/providers/<name>/cred  – sealed credential blob
//
// The reference (registry host + repository) is not secret and is stored in
// the clear.  Any credential associated with the provider is sealed with
// AES-256-GCM before being written, so credentials are never stored in
// plaintext on disk.  The symmetric key lives in the metadata DB
// (providerkey); this protects against casual inspection of the on-disk
// state (the credential cannot be grepped out of the cache or DB files)
// while keeping the key co-located with the metadata it protects.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"io"

	"github.com/containerd/errdefs"
	bolt "go.etcd.io/bbolt"
)

// ProviderInfo is the persisted reconstruction metadata for a ByteProvider.
type ProviderInfo struct {
	// Ref is the registry reference (e.g. "example.com/repo@sha256:...").
	// Not a secret; stored in the clear.
	Ref string

	// Credential is the (decrypted) credential blob, or nil when none was
	// stored.  On disk it is always sealed with AES-256-GCM.
	Credential []byte
}

// PutProvider persists reconstruction metadata for the named provider.  The
// ref is stored in the clear; cred, when non-nil, is sealed with AES-256-GCM
// using the store's provider key (created on first use) so it is never
// written to disk in plaintext.
func (s *Store) PutProvider(ctx context.Context, name, ref string, cred []byte) error {
	if name == "" {
		return fmt.Errorf("content/index: provider name required: %w", errdefs.ErrInvalidArgument)
	}
	return update(ctx, s.db, func(tx *bolt.Tx) error {
		cfg, err := createBucketIfNotExists(tx, bucketKeyVersion, bucketKeyIndexedContent)
		if err != nil {
			return err
		}
		provBkt, err := cfg.CreateBucketIfNotExists(bucketKeyObjectProviders)
		if err != nil {
			return err
		}
		nb, err := provBkt.CreateBucketIfNotExists([]byte(name))
		if err != nil {
			return err
		}
		if err := nb.Put(bucketKeyProviderRef, []byte(ref)); err != nil {
			return err
		}
		if len(cred) == 0 {
			return nil
		}
		key, err := providerKey(cfg)
		if err != nil {
			return err
		}
		sealed, err := sealCredential(key, cred)
		if err != nil {
			return err
		}
		return nb.Put(bucketKeyProviderCred, sealed)
	})
}

// GetProvider returns the persisted reconstruction metadata for the named
// provider, decrypting the credential if one was stored.  Returns
// errdefs.ErrNotFound when no record exists.
func (s *Store) GetProvider(ctx context.Context, name string) (ProviderInfo, error) {
	var out ProviderInfo
	err := view(ctx, s.db, func(tx *bolt.Tx) error {
		cfg := getBucket(tx, bucketKeyVersion, bucketKeyIndexedContent)
		if cfg == nil {
			return errdefs.ErrNotFound
		}
		provBkt := cfg.Bucket(bucketKeyObjectProviders)
		if provBkt == nil {
			return errdefs.ErrNotFound
		}
		nb := provBkt.Bucket([]byte(name))
		if nb == nil {
			return errdefs.ErrNotFound
		}
		out.Ref = string(nb.Get(bucketKeyProviderRef))
		if sealed := nb.Get(bucketKeyProviderCred); len(sealed) > 0 {
			key, err := providerKey(cfg)
			if err != nil {
				return err
			}
			cred, err := openCredential(key, sealed)
			if err != nil {
				return err
			}
			out.Credential = cred
		}
		return nil
	})
	if err != nil {
		return ProviderInfo{}, err
	}
	return out, nil
}

// deleteProvider removes the persisted record for the named provider within
// the supplied (writable) transaction.  Absence is not an error.  Called from
// GC Finish when the last blob that referenced the provider is reaped.
func deleteProvider(tx *bolt.Tx, name string) error {
	if name == "" {
		return nil
	}
	cfg := getBucket(tx, bucketKeyVersion, bucketKeyIndexedContent)
	if cfg == nil {
		return nil
	}
	provBkt := cfg.Bucket(bucketKeyObjectProviders)
	if provBkt == nil {
		return nil
	}
	if provBkt.Bucket([]byte(name)) == nil {
		return nil
	}
	return provBkt.DeleteBucket([]byte(name))
}

// providerKey returns the store's AES-256 provider key, generating and
// persisting one in the config bucket on first use.  Must be called within a
// transaction that holds cfg writable when the key may not yet exist; reads
// are satisfied without a write.
func providerKey(cfg *bolt.Bucket) ([]byte, error) {
	if k := cfg.Get(bucketKeyProviderKey); len(k) == 32 {
		return append([]byte(nil), k...), nil
	}
	// Generate a fresh 256-bit key.  cfg must be writable here; PutProvider
	// always opens the config bucket in a writable tx, and GetProvider only
	// reaches this path when a sealed credential exists, which implies the
	// key was already written.
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("content/index: generate provider key: %w", err)
	}
	if err := cfg.Put(bucketKeyProviderKey, key); err != nil {
		return nil, err
	}
	return key, nil
}

// sealCredential encrypts plaintext with AES-256-GCM, returning nonce||ciphertext.
func sealCredential(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("content/index: aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("content/index: gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("content/index: nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// openCredential decrypts a nonce||ciphertext blob produced by sealCredential.
func openCredential(key, sealed []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("content/index: aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("content/index: gcm: %w", err)
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, fmt.Errorf("content/index: sealed credential too short: %w", errdefs.ErrInvalidArgument)
	}
	nonce, ct := sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("content/index: open credential: %w", err)
	}
	return pt, nil
}
