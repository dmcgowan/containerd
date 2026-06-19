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

package dmverity

import (
	"strings"
	"testing"
)

// TestNormalizeRootHash_BareHex verifies that bare-hex root hashes (the
// form produced by Format() — fmt.Sprintf("%x", ...)) pass through
// unchanged so existing eager-format sidecars and tests continue to
// work.
func TestNormalizeRootHash_BareHex(t *testing.T) {
	const bare = "12822d74822aeb95cf79f646311aadd40db2410a94b41a9fb3ce3b36552560de"
	got, err := normalizeRootHash(bare)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != bare {
		t.Errorf("bare hex altered: got %q, want %q", got, bare)
	}
}

// TestNormalizeRootHash_Sha256Prefix verifies that the "sha256:<hex>"
// form carried by the org.erofs.dmverity.root_digest annotation (and
// thereby by the dmverity-roothash= mount option and convert-time
// sidecars) is accepted and reduced to the bare-hex form ParseRootHash
// expects.  This is the bug reported in the field:
//
//	dmverity.Open failed: invalid root hash: invalid root hex:
//	encoding/hex: invalid byte: U+0073 's'
//
// The 's' came from the "sha256:" prefix being fed verbatim into
// hex.Decode.
func TestNormalizeRootHash_Sha256Prefix(t *testing.T) {
	const bare = "12822d74822aeb95cf79f646311aadd40db2410a94b41a9fb3ce3b36552560de"
	got, err := normalizeRootHash("sha256:" + bare)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != bare {
		t.Errorf("prefix not stripped: got %q, want %q", got, bare)
	}
}

// TestNormalizeRootHash_TrimsWhitespace verifies that incidental
// whitespace from sidecar files or option string concatenation
// doesn't break parsing.
func TestNormalizeRootHash_TrimsWhitespace(t *testing.T) {
	const bare = "abc123"
	got, err := normalizeRootHash("  sha256:" + bare + "\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != bare {
		t.Errorf("whitespace not trimmed: got %q, want %q", got, bare)
	}
}

// TestNormalizeRootHash_RejectsOtherAlgorithms verifies the helper
// refuses non-sha256 algorithm prefixes rather than silently treating
// them as data.  dm-verity in this codebase is SHA-256 only; surfacing
// the mismatch at the boundary is preferable to letting an unrelated
// hex string flow through and produce a confusing "wrong root hash"
// failure further down.
func TestNormalizeRootHash_RejectsOtherAlgorithms(t *testing.T) {
	for _, prefix := range []string{"sha512:", "sha1:", "md5:"} {
		t.Run(prefix, func(t *testing.T) {
			_, err := normalizeRootHash(prefix + "abc")
			if err == nil {
				t.Fatalf("%s should be rejected", prefix)
			}
			if !strings.Contains(err.Error(), "unsupported algorithm") {
				t.Errorf("expected 'unsupported algorithm' in error, got: %v", err)
			}
		})
	}
}

// TestNormalizeRootHash_Empty preserves the existing "empty rootHash"
// invariant: callers (Open, VerifyDevice) check for empty and emit
// their own error.  The helper itself returns "" for empty input.
func TestNormalizeRootHash_Empty(t *testing.T) {
	got, err := normalizeRootHash("")
	if err != nil {
		t.Errorf("empty input should not error, got: %v", err)
	}
	if got != "" {
		t.Errorf("empty input should return empty, got %q", got)
	}
}
