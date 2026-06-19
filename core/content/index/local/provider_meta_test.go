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
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/containerd/errdefs"
	localcs "github.com/containerd/containerd/v2/plugins/content/local"
	bolt "go.etcd.io/bbolt"
)

// newProviderTestStore opens a bolt DB at a known path so the raw file can be
// inspected, and returns the store plus the DB path.
func newProviderTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "meta.db")
	bdb, err := bolt.Open(dbPath, 0644, nil)
	if err != nil {
		t.Fatalf("open bolt db: %v", err)
	}
	t.Cleanup(func() { bdb.Close() })
	cs, err := localcs.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("content store: %v", err)
	}
	store, err := NewStore(Config{Root: t.TempDir(), DB: bdb, Content: cs})
	if err != nil {
		t.Fatalf("new indexed store: %v", err)
	}
	return store, dbPath
}

// TestProviderMetadataRoundTrip verifies that PutProvider/GetProvider persist
// the registry ref in the clear and the credential encrypted, and that the
// plaintext credential never appears in the on-disk bolt file.
func TestProviderMetadataRoundTrip(t *testing.T) {
	store, dbPath := newProviderTestStore(t)
	ctx := context.Background()

	const (
		name = "registry:sha256:deadbeef"
		ref  = "example.com/repo@sha256:deadbeef"
	)
	// A recognisable plaintext secret we can grep for in the raw DB file.
	secret := []byte("BEARER-SUPER-SECRET-TOKEN-0123456789")

	if err := store.PutProvider(ctx, name, ref, secret); err != nil {
		t.Fatalf("PutProvider: %v", err)
	}

	got, err := store.GetProvider(ctx, name)
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if got.Ref != ref {
		t.Errorf("ref = %q, want %q", got.Ref, ref)
	}
	if !bytes.Equal(got.Credential, secret) {
		t.Errorf("credential round-trip mismatch: got %q want %q", got.Credential, secret)
	}

	// The plaintext secret must NOT be present anywhere in the raw DB file.
	if err := store.db.(*bolt.DB).Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read db file: %v", err)
	}
	if bytes.Contains(raw, secret) {
		t.Errorf("plaintext credential found in on-disk bolt file %s", dbPath)
	}
	// The ref, by contrast, is not a secret and is stored in the clear.
	if !bytes.Contains(raw, []byte(ref)) {
		t.Errorf("provider ref not found in on-disk bolt file (expected plaintext)")
	}
}

// TestProviderMetadataNotFound verifies GetProvider returns ErrNotFound for an
// unknown provider name.
func TestProviderMetadataNotFound(t *testing.T) {
	store, _ := newProviderTestStore(t)
	if _, err := store.GetProvider(context.Background(), "missing"); !errdefs.IsNotFound(err) {
		t.Errorf("GetProvider(missing) err = %v, want NotFound", err)
	}
}

// TestProviderMetadataRefOnly verifies a provider with no credential persists
// the ref and reports a nil credential.
func TestProviderMetadataRefOnly(t *testing.T) {
	store, _ := newProviderTestStore(t)
	ctx := context.Background()
	if err := store.PutProvider(ctx, "p", "host/repo", nil); err != nil {
		t.Fatalf("PutProvider: %v", err)
	}
	got, err := store.GetProvider(ctx, "p")
	if err != nil {
		t.Fatalf("GetProvider: %v", err)
	}
	if got.Ref != "host/repo" || got.Credential != nil {
		t.Errorf("got %+v, want ref=host/repo cred=nil", got)
	}
}
