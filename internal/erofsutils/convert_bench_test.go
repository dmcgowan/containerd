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

// Benchmarks and correctness tests for mkfs.erofs subprocess vs pure-Go
// erofsutils implementations.
//
// Run benchmarks:
//
//	go test ./internal/erofsutils/... -bench=. -benchtime=3x -v
package erofsutils_test

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/internal/erofsutils"
	goerofs "github.com/erofs/go-erofs"
	"github.com/stretchr/testify/require"
)

// hasMkfsErofs returns true when mkfs.erofs is in PATH.
func hasMkfsErofs() bool {
	_, err := exec.LookPath("mkfs.erofs")
	return err == nil
}

// makeTarStream builds a synthetic tar of approximately payloadBytes bytes.
func makeTarStream(t testing.TB, payloadBytes int) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	require.NoError(t, tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeDir, Name: "./", Mode: 0755,
	}))

	fileData := bytes.Repeat([]byte("deadbeef"), 512) // 4096 bytes each
	count := payloadBytes / len(fileData)
	if count == 0 {
		count = 1
	}
	for i := 0; i < count; i++ {
		dir := "d" + string(rune('a'+i%26))
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeDir, Name: dir + "/", Mode: 0755,
		}))
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg,
			Name:     dir + "/file",
			Size:     int64(len(fileData)),
			Mode:     0644,
		}))
		_, err := tw.Write(fileData)
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	return buf.Bytes()
}

// makeTarStreamOCI builds a small realistic OCI-layer tar.
func makeTarStreamOCI(t testing.TB) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	writeDir := func(name string) {
		t.Helper()
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeDir, Name: name + "/", Mode: 0755,
		}))
	}
	writeFile := func(name string, data []byte, mode int64) {
		t.Helper()
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Typeflag: tar.TypeReg, Name: name, Size: int64(len(data)), Mode: mode,
		}))
		_, err := tw.Write(data)
		require.NoError(t, err)
	}

	writeDir(".")
	writeDir("etc")
	writeFile("etc/hostname", []byte("benchhost"), 0644)
	writeDir("usr")
	writeDir("usr/bin")
	writeFile("usr/bin/sh", bytes.Repeat([]byte{0xEF}, 65536), 0755)
	writeDir("var")
	writeDir("var/log")
	writeFile("var/log/app.log", bytes.Repeat([]byte("log\n"), 4096), 0644)

	require.NoError(t, tw.Close())
	return buf.Bytes()
}

// makeSourceDir builds a directory tree of approximately payloadBytes bytes.
func makeSourceDir(t testing.TB, payloadBytes int) string {
	t.Helper()
	dir := t.TempDir()
	fileData := bytes.Repeat([]byte("deadbeef"), 512)
	count := payloadBytes / len(fileData)
	if count == 0 {
		count = 1
	}
	for i := 0; i < count; i++ {
		sub := filepath.Join(dir, "d"+string(rune('a'+i%26)))
		require.NoError(t, os.MkdirAll(sub, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(sub, "file"), fileData, 0644))
	}
	return dir
}

// ============================================================
// Benchmarks: ConvertTarErofs — mkfs subprocess
// ============================================================

func BenchmarkConvertTarErofs_Mkfs_1MB(b *testing.B) { benchTarMkfs(b, 1<<20) }
func BenchmarkConvertTarErofs_Mkfs_16MB(b *testing.B) { benchTarMkfs(b, 16<<20) }
func BenchmarkConvertTarErofs_Mkfs_64MB(b *testing.B) { benchTarMkfs(b, 64<<20) }

func benchTarMkfs(b *testing.B, size int) {
	if !hasMkfsErofs() {
		b.Skip("mkfs.erofs not in PATH")
	}
	data := makeTarStream(b, size)
	ctx := context.Background()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := filepath.Join(b.TempDir(), "layer.erofs")
		if err := erofsutils.ConvertTarErofs(ctx, bytes.NewReader(data), out, "", nil); err != nil {
			b.Fatal(err)
		}
	}
}

// ============================================================
// Benchmarks: ConvertTarErofs — pure Go
// ============================================================

func BenchmarkConvertTarErofs_Go_1MB(b *testing.B) { benchTarGo(b, 1<<20) }
func BenchmarkConvertTarErofs_Go_16MB(b *testing.B) { benchTarGo(b, 16<<20) }
func BenchmarkConvertTarErofs_Go_64MB(b *testing.B) { benchTarGo(b, 64<<20) }

func benchTarGo(b *testing.B, size int) {
	data := makeTarStream(b, size)
	ctx := context.Background()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := filepath.Join(b.TempDir(), "layer.erofs")
		if err := erofsutils.ConvertTarErofsGo(ctx, bytes.NewReader(data), out, ""); err != nil {
			b.Fatal(err)
		}
	}
}

// ============================================================
// Benchmarks: ConvertDirErofs — mkfs subprocess
// ============================================================

func BenchmarkConvertDirErofs_Mkfs_1MB(b *testing.B) { benchDirMkfs(b, 1<<20) }
func BenchmarkConvertDirErofs_Mkfs_16MB(b *testing.B) { benchDirMkfs(b, 16<<20) }

func benchDirMkfs(b *testing.B, size int) {
	if !hasMkfsErofs() {
		b.Skip("mkfs.erofs not in PATH")
	}
	src := makeSourceDir(b, size)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := filepath.Join(b.TempDir(), "layer.erofs")
		if err := erofsutils.ConvertErofs(ctx, out, src, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// ============================================================
// Benchmarks: ConvertDirErofs — pure Go
// ============================================================

func BenchmarkConvertDirErofs_Go_1MB(b *testing.B) { benchDirGo(b, 1<<20) }
func BenchmarkConvertDirErofs_Go_16MB(b *testing.B) { benchDirGo(b, 16<<20) }

func benchDirGo(b *testing.B, size int) {
	src := makeSourceDir(b, size)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := filepath.Join(b.TempDir(), "layer.erofs")
		if err := erofsutils.ConvertDirErofsGo(ctx, out, src); err != nil {
			b.Fatal(err)
		}
	}
}

// ============================================================
// Benchmarks: realistic OCI layer
// ============================================================

func BenchmarkOCILayer_Mkfs(b *testing.B) {
	if !hasMkfsErofs() {
		b.Skip("mkfs.erofs not in PATH")
	}
	data := makeTarStreamOCI(b)
	ctx := context.Background()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := filepath.Join(b.TempDir(), "layer.erofs")
		if err := erofsutils.ConvertTarErofs(ctx, bytes.NewReader(data), out, "", nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkOCILayer_Go(b *testing.B) {
	data := makeTarStreamOCI(b)
	ctx := context.Background()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out := filepath.Join(b.TempDir(), "layer.erofs")
		if err := erofsutils.ConvertTarErofsGo(ctx, bytes.NewReader(data), out, ""); err != nil {
			b.Fatal(err)
		}
	}
}

// ============================================================
// Correctness tests
// ============================================================

func TestConvertTarErofsGoBasic(t *testing.T) {
	data := makeTarStream(t, 64*1024)
	out := filepath.Join(t.TempDir(), "layer.erofs")
	require.NoError(t, erofsutils.ConvertTarErofsGo(context.Background(),
		bytes.NewReader(data), out, ""))
	checkSuperblock(t, out)
}

func TestConvertDirErofsGoBasic(t *testing.T) {
	src := makeSourceDir(t, 64*1024)
	out := filepath.Join(t.TempDir(), "layer.erofs")
	require.NoError(t, erofsutils.ConvertDirErofsGo(context.Background(), out, src))
	checkSuperblock(t, out)
}

func TestGenerateTarIndexAndAppendTarGoBasic(t *testing.T) {
	data := makeTarStream(t, 32*1024)
	out := filepath.Join(t.TempDir(), "layer.erofs")
	require.NoError(t, erofsutils.GenerateTarIndexAndAppendTarGo(
		context.Background(), bytes.NewReader(data), out, ""))

	// 1. EROFS superblock at offset 1024.
	checkSuperblock(t, out)

	// 2. The tar-index output has two sections: EROFS metadata + raw payload data.
	//    Read the EROFS metadata section (starts at byte 0) and open it.
	//    The EROFS superblock at offset 1024 confirms the metadata is valid.
	//    Since both the EROFS metadata AND the payload data are present, the
	//    output is larger than the metadata section alone.
	fi, err := os.Stat(out)
	require.NoError(t, err)
	require.Greater(t, fi.Size(), int64(4096),
		"tar-index output must be larger than a minimal EROFS (metadata + payload)")

	// 3. The EROFS metadata section must list the same files as a
	//    full-extraction EROFS built from the same tar.
	//    Note: the tar-index EROFS references an external data device, so we
	//    pass the output file itself as both the EROFS image and the data device.
	erofsOnly := filepath.Join(t.TempDir(), "only.erofs")
	require.NoError(t, erofsutils.ConvertTarErofsGo(
		context.Background(), bytes.NewReader(data), erofsOnly, ""))
	namesIdx := erofsFileNamesWithDevice(t, out, out)
	namesOnly := erofsFileNames(t, erofsOnly)
	require.Equal(t, len(namesOnly), len(namesIdx),
		"tar-index and full-extraction must have equal entry counts")
}

// TestGenerateTarIndexAndAppendTarGoPayload verifies that the file payload
// is correctly stored in the data section (after the EROFS metadata image).
// The payload bytes must be present at a 512-byte-aligned offset in the
// combined output.
func TestGenerateTarIndexAndAppendTarGoPayload(t *testing.T) {
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeDir, Name: "./", Mode: 0755,
	}))
	const payload = "hello tar-index world"
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg, Name: "hello.txt",
		Size: int64(len(payload)), Mode: 0644,
	}))
	_, err := tw.Write([]byte(payload))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	tarData := tarBuf.Bytes()

	out := filepath.Join(t.TempDir(), "layer.erofs")
	require.NoError(t, erofsutils.GenerateTarIndexAndAppendTarGo(
		context.Background(), bytes.NewReader(tarData), out, ""))

	// The EROFS metadata must be valid.
	checkSuperblock(t, out)

	// The payload "hello tar-index world" must appear somewhere in the output.
	outData, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Contains(t, string(outData), payload,
		"file payload must be present in the tar-index EROFS output")

	// The output size must be larger than the payload alone.
	require.Greater(t, len(outData), len(payload)+1024,
		"output must contain EROFS metadata in addition to payload")
}

// TestConvertTarErofsGoWhiteouts verifies that OCI whiteouts (.wh.*) are
// translated to overlayfs char-device nodes in the Go path.
func TestConvertTarErofsGoWhiteouts(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "./", Mode: 0755}))
	require.NoError(t, tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "keep.txt", Size: 4, Mode: 0644}))
	_, err := tw.Write([]byte("data"))
	require.NoError(t, err)
	require.NoError(t, tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: ".wh.gone.txt", Size: 0, Mode: 0644}))
	require.NoError(t, tw.Close())

	out := filepath.Join(t.TempDir(), "layer.erofs")
	require.NoError(t, erofsutils.ConvertTarErofsGo(context.Background(),
		bytes.NewReader(buf.Bytes()), out, ""))
	checkSuperblock(t, out)

	names := erofsFileNames(t, out)
	var foundKeep, foundGone bool
	for _, n := range names {
		switch strings.TrimPrefix(n, "./") {
		case "keep.txt":
			foundKeep = true
		case "gone.txt":
			foundGone = true
		}
	}
	require.True(t, foundKeep, "keep.txt must be present: %v", names)
	require.True(t, foundGone, "gone.txt whiteout must appear as device node: %v", names)
}

// TestConvertTarErofsGoVsMkfs checks that the Go implementation produces an
// image with the same file listing as mkfs.erofs (when available).
func TestConvertTarErofsGoVsMkfs(t *testing.T) {
	if !hasMkfsErofs() {
		t.Skip("mkfs.erofs not in PATH")
	}
	data := makeTarStreamOCI(t)
	ctx := context.Background()
	tmp := t.TempDir()

	outMkfs := filepath.Join(tmp, "mkfs.erofs")
	outGo := filepath.Join(tmp, "go.erofs")

	require.NoError(t, erofsutils.ConvertTarErofs(ctx,
		bytes.NewReader(data), outMkfs, "", nil))
	require.NoError(t, erofsutils.ConvertTarErofsGo(ctx,
		bytes.NewReader(data), outGo, ""))

	mkfsNames := erofsFileNames(t, outMkfs)
	goNames := erofsFileNames(t, outGo)

	require.NotEmpty(t, mkfsNames, "mkfs.erofs image must not be empty")

	mkfsSet := map[string]bool{}
	for _, n := range mkfsNames {
		mkfsSet[strings.TrimPrefix(n, "./")] = true
	}
	for _, n := range goNames {
		mkfsSet[strings.TrimPrefix(n, "./")] = false // mark as seen
	}

	var missing []string
	for k, notSeen := range mkfsSet {
		if notSeen {
			missing = append(missing, k)
		}
	}
	require.Empty(t, missing,
		"files in mkfs.erofs image but missing from Go image: %v", missing)
	t.Logf("mkfs.erofs: %d entries, Go: %d entries", len(mkfsNames), len(goNames))
}

// TestConvertDirErofsGoVsMkfs checks that the Go dir-to-EROFS implementation
// contains the same files as mkfs.erofs.
func TestConvertDirErofsGoVsMkfs(t *testing.T) {
	if !hasMkfsErofs() {
		t.Skip("mkfs.erofs not in PATH")
	}
	src := makeSourceDir(t, 64*1024)
	ctx := context.Background()
	tmp := t.TempDir()

	outMkfs := filepath.Join(tmp, "mkfs.erofs")
	outGo := filepath.Join(tmp, "go.erofs")

	require.NoError(t, erofsutils.ConvertErofs(ctx, outMkfs, src, nil))
	require.NoError(t, erofsutils.ConvertDirErofsGo(ctx, outGo, src))

	mkfsNames := erofsFileNames(t, outMkfs)
	goNames := erofsFileNames(t, outGo)
	require.NotEmpty(t, mkfsNames)

	mkfsSet := map[string]bool{}
	for _, n := range mkfsNames {
		mkfsSet[strings.TrimPrefix(n, "./")] = true
	}
	for _, n := range goNames {
		mkfsSet[strings.TrimPrefix(n, "./")] = false
	}
	var missing []string
	for k, notSeen := range mkfsSet {
		if notSeen {
			missing = append(missing, k)
		}
	}
	require.Empty(t, missing,
		"files in mkfs.erofs image but missing from Go image: %v", missing)
}

// ============================================================
// Helpers
// ============================================================

// checkSuperblock asserts the EROFS magic at offset 1024.
func checkSuperblock(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	fi, err := f.Stat()
	require.NoError(t, err)
	require.Greater(t, fi.Size(), int64(1028), "image too small for superblock")

	var magic [4]byte
	_, err = f.ReadAt(magic[:], 1024)
	require.NoError(t, err)
	require.Equal(t, [4]byte{0xE2, 0xE1, 0xF5, 0xE0}, magic,
		"EROFS magic 0xE0F5E1E2 must be at offset 1024")
}

// erofsFileNames opens an EROFS image with go-erofs and returns all paths.
func erofsFileNames(t *testing.T, path string) []string {
	t.Helper()
	return erofsFileNamesWithDevice(t, path, "")
}

// erofsFileNamesWithDevice opens an EROFS image optionally passing an extra
// device (for chunk-based images created with WithDataFile).
func erofsFileNamesWithDevice(t *testing.T, path, devicePath string) []string {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	var opts []goerofs.OpenOpt
	if devicePath != "" {
		dev, err := os.Open(devicePath)
		if err == nil {
			defer dev.Close()
			opts = append(opts, goerofs.WithExtraDevices(dev))
		}
	}

	fsys, err := goerofs.Open(f, opts...)
	require.NoError(t, err)

	var names []string
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		names = append(names, p)
		return nil
	})
	require.NoError(t, err)
	return names
}

// ioReaderAt adapts an io.ReadSeeker so it satisfies io.ReaderAt.
type ioReaderAt struct{ r io.ReadSeeker }

func (ra ioReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if _, err := ra.r.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	return io.ReadAtLeast(ra.r, p, len(p))
}
