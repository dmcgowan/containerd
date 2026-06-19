//go:build linux

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
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/internal/dmverity"
)

func TestEffectiveBlockSize(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   uint32
		want uint32
	}{
		{"zero → default", 0, dmverity.DefaultBlockSize},
		{"explicit 4096", 4096, 4096},
		{"explicit 8192", 8192, 8192},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveBlockSize(tc.in); got != tc.want {
				t.Errorf("effectiveBlockSize(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestComputeVerityName_stable(t *testing.T) {
	// Stability: the same (mp, blockID) pair maps to the same name
	// across runs.  This is what lets daemon-restart cleanup
	// discover orphaned /dev/mapper entries via
	// `dmsetup ls | grep containerd-block-` without having to
	// persist additional state.
	const mp = "/run/containerd/io.containerd.runtime.v2.task/default/foo/rootfs"
	const blob = "sha256:deadbeef0123456789"
	a := computeVerityName(mp, blob)
	b := computeVerityName(mp, blob)
	if a != b {
		t.Errorf("computeVerityName not stable: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "containerd-block-") {
		t.Errorf("computeVerityName missing prefix: %q", a)
	}
}

func TestComputeVerityName_distinguishesByMountpoint(t *testing.T) {
	// Two simultaneous mounts of the SAME blob at DIFFERENT
	// mountpoints must get distinct verity device names; the
	// dm-mapper API rejects creating a second device with the
	// same name.
	const blob = "sha256:abc"
	a := computeVerityName("/mp/a", blob)
	b := computeVerityName("/mp/b", blob)
	if a == b {
		t.Errorf("computeVerityName collided for distinct mountpoints: %q == %q", a, b)
	}
}

func TestComputeVerityName_distinguishesByBlockID(t *testing.T) {
	// The blockID component matters too — distinct blobs at the
	// same mountpoint (re-creation across container starts) should
	// not stomp on the previous one's residue.
	const mp = "/mp"
	a := computeVerityName(mp, "sha256:aaa")
	b := computeVerityName(mp, "sha256:bbb")
	if a == b {
		t.Errorf("computeVerityName collided for distinct blockIDs: %q == %q", a, b)
	}
}

func TestComputeVerityName_dmsetupSafeFormat(t *testing.T) {
	// dm-mapper accepts most ASCII but a "/" in a name causes
	// `dmsetup` confusion (it treats it as a UUID/path).  Our
	// computeVerityName uses hex, so the test asserts this
	// property rather than just trusting it.
	name := computeVerityName("/mp/x/y", "sha256:abc:def")
	for _, ch := range name {
		switch {
		case ch >= 'a' && ch <= 'z':
		case ch >= '0' && ch <= '9':
		case ch == '-':
		default:
			t.Errorf("computeVerityName produced disallowed character %q in %q", ch, name)
			return
		}
	}
}
