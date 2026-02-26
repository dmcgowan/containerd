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

package ext4reader

import (
	"archive/tar"
	"bytes"
	"io"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/Microsoft/hcsshim/ext4/tar2ext4"
)

// createTestExt4 creates an ext4 image from a tar stream and returns an
// io.ReaderAt for testing.
func createTestExt4(t *testing.T, tarWriter func(*tar.Writer)) *os.File {
	t.Helper()

	// Create tar
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	tarWriter(tw)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	// Convert to ext4
	f, err := os.CreateTemp(t.TempDir(), "ext4-test-*.img")
	if err != nil {
		t.Fatal(err)
	}
	if err := tar2ext4.ConvertTarToExt4(&tarBuf, f); err != nil {
		f.Close()
		t.Fatal(err)
	}

	// Seek back to start for reading
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		t.Fatal(err)
	}

	return f
}

func TestReadRegularFile(t *testing.T) {
	content := []byte("hello, ext4 world!")
	f := createTestExt4(t, func(tw *tar.Writer) {
		tw.WriteHeader(&tar.Header{
			Name:     "test.txt",
			Mode:     0644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		})
		tw.Write(content)
	})
	defer f.Close()

	fsys, err := FS(f)
	if err != nil {
		t.Fatal(err)
	}

	// Read via fs.FS
	data, err := fs.ReadFile(fsys, "test.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, content) {
		t.Fatalf("content mismatch: got %q, want %q", data, content)
	}
}

func TestReadDirectory(t *testing.T) {
	f := createTestExt4(t, func(tw *tar.Writer) {
		tw.WriteHeader(&tar.Header{
			Name:     "dir/",
			Mode:     0755,
			Typeflag: tar.TypeDir,
		})
		tw.WriteHeader(&tar.Header{
			Name:     "dir/file1.txt",
			Mode:     0644,
			Size:     5,
			Typeflag: tar.TypeReg,
		})
		tw.Write([]byte("file1"))
		tw.WriteHeader(&tar.Header{
			Name:     "dir/file2.txt",
			Mode:     0644,
			Size:     5,
			Typeflag: tar.TypeReg,
		})
		tw.Write([]byte("file2"))
	})
	defer f.Close()

	fsys, err := FS(f)
	if err != nil {
		t.Fatal(err)
	}

	entries, err := fs.ReadDir(fsys, "dir")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}
	if !names["file1.txt"] || !names["file2.txt"] {
		t.Fatalf("unexpected entries: %v", names)
	}
}

func TestReadSymlink(t *testing.T) {
	f := createTestExt4(t, func(tw *tar.Writer) {
		tw.WriteHeader(&tar.Header{
			Name:     "target.txt",
			Mode:     0644,
			Size:     4,
			Typeflag: tar.TypeReg,
		})
		tw.Write([]byte("data"))
		tw.WriteHeader(&tar.Header{
			Name:     "link.txt",
			Linkname: "target.txt",
			Mode:     0777,
			Typeflag: tar.TypeSymlink,
		})
	})
	defer f.Close()

	fsys, err := FS(f)
	if err != nil {
		t.Fatal(err)
	}

	file, err := fsys.Open("link.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	fi, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}

	if fi.Mode().Type() != fs.ModeSymlink {
		t.Fatalf("expected symlink, got %v", fi.Mode().Type())
	}

	info := fi.Sys().(*InodeInfo)
	if info.LinkTarget != "target.txt" {
		t.Fatalf("expected link target 'target.txt', got %q", info.LinkTarget)
	}
}

func TestDeviceFile(t *testing.T) {
	f := createTestExt4(t, func(tw *tar.Writer) {
		tw.WriteHeader(&tar.Header{
			Name:     "whiteout",
			Mode:     0,
			Typeflag: tar.TypeChar,
			Devmajor: 0,
			Devminor: 0,
		})
		tw.WriteHeader(&tar.Header{
			Name:     "dev",
			Mode:     0666,
			Typeflag: tar.TypeChar,
			Devmajor: 1,
			Devminor: 3,
		})
	})
	defer f.Close()

	fsys, err := FS(f)
	if err != nil {
		t.Fatal(err)
	}

	// Whiteout device (char 0:0)
	file, err := fsys.Open("whiteout")
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := file.Stat()
	info := fi.Sys().(*InodeInfo)
	if info.DevMajor != 0 || info.DevMinor != 0 {
		t.Fatalf("expected device 0:0, got %d:%d", info.DevMajor, info.DevMinor)
	}
	file.Close()

	// Regular char device
	file, err = fsys.Open("dev")
	if err != nil {
		t.Fatal(err)
	}
	fi, _ = file.Stat()
	info = fi.Sys().(*InodeInfo)
	if info.DevMajor != 1 || info.DevMinor != 3 {
		t.Fatalf("expected device 1:3, got %d:%d", info.DevMajor, info.DevMinor)
	}
	file.Close()
}

func TestXattrs(t *testing.T) {
	f := createTestExt4(t, func(tw *tar.Writer) {
		tw.WriteHeader(&tar.Header{
			Name:     "dir/",
			Mode:     0755,
			Typeflag: tar.TypeDir,
			PAXRecords: map[string]string{
				"SCHILY.xattr.trusted.overlay.opaque": "y",
			},
		})
		tw.WriteHeader(&tar.Header{
			Name:     "file.txt",
			Mode:     0644,
			Size:     4,
			Typeflag: tar.TypeReg,
			PAXRecords: map[string]string{
				"SCHILY.xattr.security.capability": "test-cap",
			},
		})
		tw.Write([]byte("data"))
	})
	defer f.Close()

	fsys, err := FS(f)
	if err != nil {
		t.Fatal(err)
	}

	// Check opaque xattr on directory
	dirFile, err := fsys.Open("dir")
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := dirFile.Stat()
	info := fi.Sys().(*InodeInfo)
	if v, ok := info.Xattrs["trusted.overlay.opaque"]; !ok {
		t.Fatal("missing trusted.overlay.opaque xattr")
	} else if string(v) != "y" {
		t.Fatalf("expected xattr value 'y', got %q", v)
	}
	dirFile.Close()

	// Check capability xattr on file
	regFile, err := fsys.Open("file.txt")
	if err != nil {
		t.Fatal(err)
	}
	fi, _ = regFile.Stat()
	info = fi.Sys().(*InodeInfo)
	if v, ok := info.Xattrs["security.capability"]; !ok {
		t.Fatal("missing security.capability xattr")
	} else if string(v) != "test-cap" {
		t.Fatalf("expected xattr value 'test-cap', got %q", v)
	}
	regFile.Close()
}

func TestOverlayWhiteoutConversion(t *testing.T) {
	// Create ext4 with overlay-style whiteouts (what tar2ext4 with ConvertWhiteout produces)
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)

	// Write an OCI whiteout
	tw.WriteHeader(&tar.Header{
		Name:     ".wh.deleted-file",
		Mode:     0,
		Size:     0,
		Typeflag: tar.TypeReg,
	})
	// Write an opaque directory whiteout
	tw.WriteHeader(&tar.Header{
		Name:     "opaque-dir/",
		Mode:     0755,
		Typeflag: tar.TypeDir,
	})
	tw.WriteHeader(&tar.Header{
		Name:     "opaque-dir/.wh..wh..opq",
		Mode:     0,
		Size:     0,
		Typeflag: tar.TypeReg,
	})
	tw.Close()

	f, err := os.CreateTemp(t.TempDir(), "ext4-whiteout-*.img")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// Convert with ConvertWhiteout to create overlay-style whiteouts
	if err := tar2ext4.ConvertTarToExt4(&tarBuf, f, tar2ext4.ConvertWhiteout); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}

	fsys, err := FS(f)
	if err != nil {
		t.Fatal(err)
	}

	// The OCI whiteout .wh.deleted-file should become a char 0:0 device named "deleted-file"
	file, err := fsys.Open("deleted-file")
	if err != nil {
		t.Fatal("expected 'deleted-file' char device from converted whiteout:", err)
	}
	fi, _ := file.Stat()
	info := fi.Sys().(*InodeInfo)
	if fi.Mode()&fs.ModeCharDevice == 0 {
		t.Fatal("expected char device for whiteout")
	}
	if info.DevMajor != 0 || info.DevMinor != 0 {
		t.Fatalf("expected device 0:0, got %d:%d", info.DevMajor, info.DevMinor)
	}
	file.Close()

	// opaque-dir should have trusted.overlay.opaque xattr
	dirFile, err := fsys.Open("opaque-dir")
	if err != nil {
		t.Fatal(err)
	}
	fi, _ = dirFile.Stat()
	info = fi.Sys().(*InodeInfo)
	if v, ok := info.Xattrs["trusted.overlay.opaque"]; !ok {
		t.Fatal("missing trusted.overlay.opaque xattr on opaque directory")
	} else if string(v) != "y" {
		t.Fatalf("expected 'y', got %q", v)
	}
	dirFile.Close()
}

func TestInodeMetadata(t *testing.T) {
	mtime := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	f := createTestExt4(t, func(tw *tar.Writer) {
		tw.WriteHeader(&tar.Header{
			Name:     "file.txt",
			Mode:     0o755,
			Size:     3,
			Typeflag: tar.TypeReg,
			Uid:      1000,
			Gid:      2000,
			ModTime:  mtime,
		})
		tw.Write([]byte("abc"))
	})
	defer f.Close()

	fsys, err := FS(f)
	if err != nil {
		t.Fatal(err)
	}

	file, err := fsys.Open("file.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	fi, _ := file.Stat()
	info := fi.Sys().(*InodeInfo)

	if info.UID != 1000 {
		t.Fatalf("expected UID 1000, got %d", info.UID)
	}
	if info.GID != 2000 {
		t.Fatalf("expected GID 2000, got %d", info.GID)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("expected mode 0755, got %o", fi.Mode().Perm())
	}
	if !fi.ModTime().Equal(mtime) {
		t.Fatalf("expected mtime %v, got %v", mtime, fi.ModTime())
	}
	if fi.Size() != 3 {
		t.Fatalf("expected size 3, got %d", fi.Size())
	}
}

func TestWalk(t *testing.T) {
	f := createTestExt4(t, func(tw *tar.Writer) {
		tw.WriteHeader(&tar.Header{Name: "a/", Mode: 0755, Typeflag: tar.TypeDir})
		tw.WriteHeader(&tar.Header{Name: "a/b/", Mode: 0755, Typeflag: tar.TypeDir})
		tw.WriteHeader(&tar.Header{Name: "a/b/c.txt", Mode: 0644, Size: 1, Typeflag: tar.TypeReg})
		tw.Write([]byte("x"))
		tw.WriteHeader(&tar.Header{Name: "d.txt", Mode: 0644, Size: 1, Typeflag: tar.TypeReg})
		tw.Write([]byte("y"))
	})
	defer f.Close()

	fsys, err := FS(f)
	if err != nil {
		t.Fatal(err)
	}

	var paths []string
	err = Walk(fsys, ".", func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// ext4 always includes a lost+found directory
	expectedSet := map[string]bool{
		".": true, "a": true, "a/b": true, "a/b/c.txt": true, "d.txt": true, "lost+found": true,
	}
	for _, p := range paths {
		if !expectedSet[p] {
			t.Fatalf("unexpected path %q in walk results: %v", p, paths)
		}
	}
	// Must contain at least the non-lost+found entries
	for _, required := range []string{".", "a", "a/b", "a/b/c.txt", "d.txt"} {
		found := false
		for _, p := range paths {
			if p == required {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing expected path %q in walk results: %v", required, paths)
		}
	}
}

func TestLargeFile(t *testing.T) {
	// Test with a file larger than one block (4096 bytes by default)
	size := 8192
	content := make([]byte, size)
	for i := range content {
		content[i] = byte(i % 256)
	}

	f := createTestExt4(t, func(tw *tar.Writer) {
		tw.WriteHeader(&tar.Header{
			Name:     "large.bin",
			Mode:     0644,
			Size:     int64(size),
			Typeflag: tar.TypeReg,
		})
		tw.Write(content)
	})
	defer f.Close()

	fsys, err := FS(f)
	if err != nil {
		t.Fatal(err)
	}

	data, err := fs.ReadFile(fsys, "large.bin")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, content) {
		t.Fatal("large file content mismatch")
	}
}

func TestNotExist(t *testing.T) {
	f := createTestExt4(t, func(tw *tar.Writer) {
		tw.WriteHeader(&tar.Header{Name: "exists.txt", Mode: 0644, Size: 1, Typeflag: tar.TypeReg})
		tw.Write([]byte("x"))
	})
	defer f.Close()

	fsys, err := FS(f)
	if err != nil {
		t.Fatal(err)
	}

	_, err = fsys.Open("does-not-exist.txt")
	if err == nil {
		t.Fatal("expected error for non-existent file")
	}
}

func TestHardlinks(t *testing.T) {
	content := []byte("shared content")
	f := createTestExt4(t, func(tw *tar.Writer) {
		tw.WriteHeader(&tar.Header{
			Name:     "original.txt",
			Mode:     0644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		})
		tw.Write(content)
		tw.WriteHeader(&tar.Header{
			Name:     "hardlink.txt",
			Linkname: "original.txt",
			Mode:     0644,
			Typeflag: tar.TypeLink,
		})
	})
	defer f.Close()

	fsys, err := FS(f)
	if err != nil {
		t.Fatal(err)
	}

	// Both should have same content
	data1, err := fs.ReadFile(fsys, "original.txt")
	if err != nil {
		t.Fatal(err)
	}
	data2, err := fs.ReadFile(fsys, "hardlink.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data1, data2) {
		t.Fatal("hardlink content mismatch")
	}

	// Both should have same inode number and nlink > 1
	f1, _ := fsys.Open("original.txt")
	fi1, _ := f1.Stat()
	info1 := fi1.Sys().(*InodeInfo)
	f1.Close()

	f2, _ := fsys.Open("hardlink.txt")
	fi2, _ := f2.Stat()
	info2 := fi2.Sys().(*InodeInfo)
	f2.Close()

	if info1.Ino != info2.Ino {
		t.Fatalf("expected same inode, got %d and %d", info1.Ino, info2.Ino)
	}
	if info1.Nlink < 2 {
		t.Fatalf("expected nlink >= 2, got %d", info1.Nlink)
	}
}
