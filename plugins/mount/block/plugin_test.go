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

package block

import (
	"reflect"
	"strings"
	"testing"
)

func TestDmVerityOpts_emptyRootHashDisabled(t *testing.T) {
	// No root hash → verity not requested → no option strings.
	if got := DmVerityOpts(DmVerityOptions{HashOffset: 1024, BlockSize: 4096}); got != nil {
		t.Errorf("DmVerityOpts(no roothash) = %v, want nil (verity disabled)", got)
	}
}

func TestDmVerityOpts_minimal(t *testing.T) {
	// roothash + hashoffset only, blocksize=0 (default downstream).
	// The blocksize option must NOT be emitted; the handler will
	// fall back to dmverity.DefaultBlockSize.
	got := DmVerityOpts(DmVerityOptions{
		RootHash:   "sha256:abc",
		HashOffset: 8388608,
	})
	want := []string{
		"dmverity-roothash=sha256:abc",
		"dmverity-hashoffset=8388608",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got = %v\nwant = %v", got, want)
	}
}

func TestDmVerityOpts_explicitBlockSize(t *testing.T) {
	got := DmVerityOpts(DmVerityOptions{
		RootHash:   "sha256:abc",
		HashOffset: 8388608,
		BlockSize:  8192,
	})
	want := []string{
		"dmverity-roothash=sha256:abc",
		"dmverity-hashoffset=8388608",
		"dmverity-blocksize=8192",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got = %v\nwant = %v", got, want)
	}
}

func TestNewBlockMount_appendsVerityOpts(t *testing.T) {
	// Producer side: the snapshotter passes verity options through
	// NewBlockMount; they must appear in m.Options unmodified so the
	// daemon/shim handlers can parse them downstream.
	verityOpts := DmVerityOpts(DmVerityOptions{
		RootHash:   "sha256:deadbeef",
		HashOffset: 4194304,
	})
	args := append([]string{"blockid=sha256:xyz", "fill=sparse"}, verityOpts...)
	m := NewBlockMount("/var/lib/data", args...)
	if m.Type != MountType {
		t.Errorf("Type = %q, want %q", m.Type, MountType)
	}
	if m.Source != "/var/lib/data" {
		t.Errorf("Source = %q, want %q", m.Source, "/var/lib/data")
	}
	for _, want := range verityOpts {
		found := false
		for _, opt := range m.Options {
			if opt == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("option %q missing from m.Options = %v", want, m.Options)
		}
	}
}

func TestOptionKeyPrefixesAreStable(t *testing.T) {
	// Lock the wire format.  These prefixes are split across the
	// snapshotter (producer), the daemon handler, and the shim
	// handler — changing them breaks the contract silently.
	checks := []struct {
		name string
		got  string
		want string
	}{
		{"target", OptTarget, "target="},
		{"blockid", OptBlockID, "blockid="},
		{"fill", OptFill, "fill="},
		{"dmverity-roothash", OptDmVerityRootHash, "dmverity-roothash="},
		{"dmverity-hashoffset", OptDmVerityHashOffset, "dmverity-hashoffset="},
		{"dmverity-blocksize", OptDmVerityBlockSize, "dmverity-blocksize="},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s prefix = %q, want %q (wire-format change)", c.name, c.got, c.want)
		}
		if !strings.HasSuffix(c.got, "=") {
			t.Errorf("%s prefix %q does not end with '=' (KV convention)", c.name, c.got)
		}
	}
}
