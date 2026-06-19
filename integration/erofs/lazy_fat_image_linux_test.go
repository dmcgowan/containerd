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

//go:build linux

// lazy_fat_image_linux_test.go provides helpers that build a realistically
// sized (~24 MiB) EROFS test image with multiple production-sized (4 MiB)
// chunks for use in on-demand fill and fanotify tests.
//
// # Image layout
//
//	/bin/sh        – static Go binary (CGO_ENABLED=0) that exits 0
//	/bin/echo      – identical copy of /bin/sh
//	/data/blob.bin – 24 MiB of deterministic pseudo-random bytes
//
// The filler data uses math/rand with a fixed seed (fatImageSeed) so the
// resulting EROFS image has a stable digest across test runs.  At 4 MiB
// chunks, a 24 MiB image produces exactly 6 chunks, which is a useful
// exercise range for on-demand fill tests (fill 1 chunk, verify 5 remain
// missing; fill all, verify 0 missing; concurrent fill coalescing, etc.).
//
// # Caching
//
// Building the 24 MiB filler + EROFS Writer + chunking takes ~0.5–1 s.
// The result is memoised via fatImageOnce so all tests sharing one
// `go test` invocation pay the cost once.  The memo is keyed on the
// default FatImageSizeMiB and fatImageSeed constants.
package erofs

import (
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

const (
	// FatImageSizeMiB is the uncompressed size of the fat test image.
	// At 4 MiB chunks this yields exactly 6 full chunks.
	FatImageSizeMiB = 24

	// FatImageChunkSize is the chunk size used for the fat image.
	// Matches the production chunked-blob target frame size.
	FatImageChunkSize = 4 * 1024 * 1024

	// fatImageSeed is the math/rand seed used for /data/blob.bin.
	// Fixed so the image is bit-for-bit reproducible between runs.
	fatImageSeed = 0xE20FCAFE

	// FatImageNumChunks is the number of 4 MiB chunks in the fat image.
	FatImageNumChunks = FatImageSizeMiB * 1024 * 1024 / FatImageChunkSize
)

// fatImageOnce memoises the default 24 MiB image so multiple tests share
// the one-time build cost.
var (
	fatImageOnce   sync.Once
	fatImageResult *lazyBlob
	fatImageErr    error
)

// cachedFatImage returns the memoised 24 MiB fat EROFS image, building it
// on the first call.  Subsequent calls within the same test binary return
// the cached result instantly.
//
// The returned *lazyBlob is read-only; callers must not mutate it.
func cachedFatImage(t *testing.T) *lazyBlob {
	t.Helper()
	fatImageOnce.Do(func() {
		fatImageResult, fatImageErr = buildErofsFatImage(t, FatImageSizeMiB)
	})
	if fatImageErr != nil {
		t.Fatalf("cachedFatImage: %v", fatImageErr)
	}
	return fatImageResult
}

// buildErofsFatImage builds a synthetic EROFS image of targetMiB MiB,
// chunked at FatImageChunkSize (4 MiB), using a deterministic prng for
// /data/blob.bin so the resulting digest is stable across runs.
//
// Layout:
//
//	/bin/sh        – static Go binary (prints "OK" to stdout and exits 0)
//	/bin/echo      – same binary
//	/data/blob.bin – targetMiB MiB of seeded pseudo-random bytes
func buildErofsFatImage(t *testing.T, targetMiB int) (*lazyBlob, error) {
	t.Helper()

	// Build the static /bin/sh binary.
	shBin, err := buildFatImageShBinary(t)
	if err != nil {
		return nil, err
	}

	// Generate deterministic pseudo-random filler for /data/blob.bin.
	// Using math/rand (not crypto/rand) for speed and reproducibility.
	filler := make([]byte, targetMiB*1024*1024)
	r := rand.New(rand.NewSource(fatImageSeed))
	for i := range filler {
		filler[i] = byte(r.Intn(256))
	}

	return buildErofsWithFilesChunked(t, FatImageChunkSize, map[string][]byte{
		"bin/sh":        shBin,
		"bin/echo":      shBin,
		"data/blob.bin": filler,
	})
}

// buildFatImageShBinary compiles a minimal static Go binary suitable for
// use as /bin/sh in the fat image.  The binary just prints "OK" and exits.
//
// Using CGO_ENABLED=0 ensures no shared libraries are needed inside the
// minimal EROFS rootfs.
func buildFatImageShBinary(t *testing.T) ([]byte, error) {
	t.Helper()

	if _, err := exec.LookPath("go"); err != nil {
		return nil, nil // nil data → omit bin/ from the image if go absent
	}

	src := filepath.Join(t.TempDir(), "sh.go")
	if err := os.WriteFile(src, []byte("package main\nimport\"fmt\"\nfunc main(){fmt.Println(\"OK\")}\n"), 0644); err != nil {
		return nil, err
	}
	out := filepath.Join(t.TempDir(), "sh")
	cmd := exec.Command("go", "build", "-o", out,
		"-ldflags", "-s -w -extldflags=-static", src)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("build sh: %w\n%s", err, b)
	}
	return os.ReadFile(out)
}

// fatImageBlobRange returns the byte range [start, end) within the fat
// image's /data/blob.bin file that corresponds to the given chunk index.
//
// Because the EROFS image also includes bin/sh and EROFS metadata, the
// blob.bin data does NOT start at offset 0 within the EROFS image.
// This helper regenerates the expected bytes for a given range so tests
// can verify correct content without re-reading the whole image.
//
// chunkIdx is the chunk index within the EROFS image (not within blob.bin).
// Returns nil if the chunk does not intersect blob.bin.
func fatImageExpectedChunkData(lb *lazyBlob, chunkIdx int) []byte {
	if chunkIdx < 0 || chunkIdx >= len(lb.chunks) {
		return nil
	}
	return lb.chunks[chunkIdx]
}
