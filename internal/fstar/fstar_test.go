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

package fstar_test

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/Microsoft/hcsshim/ext4/tar2ext4"
	"github.com/containerd/containerd/v2/internal/ext4reader"
	"github.com/containerd/containerd/v2/internal/fstar"
)

// createOverlayExt4 creates an ext4 image with overlay-style upper/ directory
// from the given tar entries. Whiteout entries are converted to overlay format.
func createOverlayExt4(t *testing.T, tarWriter func(*tar.Writer)) string {
	t.Helper()

	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	tarWriter(tw)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	imgPath := t.TempDir() + "/test.img"
	f, err := os.Create(imgPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if err := tar2ext4.ConvertTarToExt4(&tarBuf, f, tar2ext4.ConvertWhiteout); err != nil {
		t.Fatal(err)
	}

	return imgPath
}

// openExt4FS opens an ext4 image file and returns an fs.FS.
func openExt4FS(t *testing.T, imgPath string) (*os.File, fs.FS) {
	t.Helper()

	f, err := os.Open(imgPath)
	if err != nil {
		t.Fatal(err)
	}

	fsys, err := ext4reader.FS(f)
	if err != nil {
		f.Close()
		t.Fatal(err)
	}

	return f, fsys
}

func TestWriteOverlayTar_BasicFile(t *testing.T) {
	content := []byte("hello world")
	imgPath := createOverlayExt4(t, func(tw *tar.Writer) {
		tw.WriteHeader(&tar.Header{Name: "upper/", Mode: 0755, Typeflag: tar.TypeDir})
		tw.WriteHeader(&tar.Header{
			Name:     "upper/file.txt",
			Mode:     0644,
			Size:     int64(len(content)),
			Uid:      1000,
			Gid:      2000,
			Typeflag: tar.TypeReg,
			ModTime:  time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		})
		tw.Write(content)
	})

	f, fsys := openExt4FS(t, imgPath)
	defer f.Close()

	var buf bytes.Buffer
	if err := fstar.WriteOverlayTar(context.Background(), &buf, fsys, "upper", ext4reader.NewMetadataFunc()); err != nil {
		t.Fatal(err)
	}

	tr := tar.NewReader(&buf)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Name != "file.txt" {
		t.Fatalf("expected 'file.txt', got %q", hdr.Name)
	}
	if hdr.Uid != 1000 || hdr.Gid != 2000 {
		t.Fatalf("expected uid:gid 1000:2000, got %d:%d", hdr.Uid, hdr.Gid)
	}
	if hdr.Mode != 0644 {
		t.Fatalf("expected mode 0644, got %o", hdr.Mode)
	}

	data, _ := io.ReadAll(tr)
	if !bytes.Equal(data, content) {
		t.Fatalf("content mismatch: got %q, want %q", data, content)
	}
}

func TestWriteOverlayTar_Whiteout(t *testing.T) {
	imgPath := createOverlayExt4(t, func(tw *tar.Writer) {
		tw.WriteHeader(&tar.Header{Name: "upper/", Mode: 0755, Typeflag: tar.TypeDir})
		// OCI whiteout: .wh.deleted-file -> overlay char 0:0 "deleted-file"
		tw.WriteHeader(&tar.Header{
			Name:     "upper/.wh.deleted-file",
			Mode:     0,
			Size:     0,
			Typeflag: tar.TypeReg,
		})
	})

	f, fsys := openExt4FS(t, imgPath)
	defer f.Close()

	var buf bytes.Buffer
	if err := fstar.WriteOverlayTar(context.Background(), &buf, fsys, "upper", ext4reader.NewMetadataFunc()); err != nil {
		t.Fatal(err)
	}

	tr := tar.NewReader(&buf)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	// The overlay char 0:0 "deleted-file" should become OCI whiteout ".wh.deleted-file"
	if hdr.Name != ".wh.deleted-file" {
		t.Fatalf("expected '.wh.deleted-file', got %q", hdr.Name)
	}
	if hdr.Typeflag != tar.TypeReg {
		t.Fatalf("expected regular file type, got %d", hdr.Typeflag)
	}
	if hdr.Size != 0 {
		t.Fatalf("expected size 0, got %d", hdr.Size)
	}
}

func TestWriteOverlayTar_OpaqueDir(t *testing.T) {
	imgPath := createOverlayExt4(t, func(tw *tar.Writer) {
		tw.WriteHeader(&tar.Header{Name: "upper/", Mode: 0755, Typeflag: tar.TypeDir})
		tw.WriteHeader(&tar.Header{Name: "upper/mydir/", Mode: 0755, Typeflag: tar.TypeDir})
		// OCI opaque whiteout -> overlay trusted.overlay.opaque xattr on parent
		tw.WriteHeader(&tar.Header{
			Name:     "upper/mydir/.wh..wh..opq",
			Mode:     0,
			Size:     0,
			Typeflag: tar.TypeReg,
		})
		tw.WriteHeader(&tar.Header{
			Name:     "upper/mydir/kept.txt",
			Mode:     0644,
			Size:     4,
			Typeflag: tar.TypeReg,
		})
		tw.Write([]byte("kept"))
	})

	f, fsys := openExt4FS(t, imgPath)
	defer f.Close()

	var buf bytes.Buffer
	if err := fstar.WriteOverlayTar(context.Background(), &buf, fsys, "upper", ext4reader.NewMetadataFunc()); err != nil {
		t.Fatal(err)
	}

	tr := tar.NewReader(&buf)
	foundOpq := false
	foundDir := false
	foundFile := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case hdr.Name == "mydir/" || hdr.Name == "mydir":
			foundDir = true
		case hdr.Name == "mydir/.wh..wh..opq":
			foundOpq = true
			if hdr.Size != 0 {
				t.Fatal("opaque whiteout should have size 0")
			}
		case hdr.Name == "mydir/kept.txt":
			foundFile = true
		}
	}
	if !foundDir {
		t.Fatal("missing mydir/ directory entry")
	}
	if !foundOpq {
		t.Fatal("missing .wh..wh..opq entry for opaque directory")
	}
	if !foundFile {
		t.Fatal("missing mydir/kept.txt entry")
	}
}

func TestWriteOverlayTar_Symlink(t *testing.T) {
	imgPath := createOverlayExt4(t, func(tw *tar.Writer) {
		tw.WriteHeader(&tar.Header{Name: "upper/", Mode: 0755, Typeflag: tar.TypeDir})
		tw.WriteHeader(&tar.Header{
			Name:     "upper/link",
			Linkname: "/etc/target",
			Mode:     0777,
			Typeflag: tar.TypeSymlink,
		})
	})

	f, fsys := openExt4FS(t, imgPath)
	defer f.Close()

	var buf bytes.Buffer
	if err := fstar.WriteOverlayTar(context.Background(), &buf, fsys, "upper", ext4reader.NewMetadataFunc()); err != nil {
		t.Fatal(err)
	}

	tr := tar.NewReader(&buf)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Name != "link" {
		t.Fatalf("expected 'link', got %q", hdr.Name)
	}
	if hdr.Typeflag != tar.TypeSymlink {
		t.Fatalf("expected symlink type, got %d", hdr.Typeflag)
	}
	if hdr.Linkname != "/etc/target" {
		t.Fatalf("expected linkname '/etc/target', got %q", hdr.Linkname)
	}
}

func TestWriteOverlayTar_Xattrs(t *testing.T) {
	imgPath := createOverlayExt4(t, func(tw *tar.Writer) {
		tw.WriteHeader(&tar.Header{Name: "upper/", Mode: 0755, Typeflag: tar.TypeDir})
		tw.WriteHeader(&tar.Header{
			Name:     "upper/cap-file",
			Mode:     0755,
			Size:     4,
			Typeflag: tar.TypeReg,
			PAXRecords: map[string]string{
				"SCHILY.xattr.security.capability": "test-cap-value",
			},
		})
		tw.Write([]byte("data"))
	})

	f, fsys := openExt4FS(t, imgPath)
	defer f.Close()

	var buf bytes.Buffer
	if err := fstar.WriteOverlayTar(context.Background(), &buf, fsys, "upper", ext4reader.NewMetadataFunc()); err != nil {
		t.Fatal(err)
	}

	tr := tar.NewReader(&buf)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Name != "cap-file" {
		t.Fatalf("expected 'cap-file', got %q", hdr.Name)
	}

	capVal, ok := hdr.PAXRecords["SCHILY.xattr.security.capability"]
	if !ok {
		t.Fatal("missing security.capability xattr in tar PAX records")
	}
	if capVal != "test-cap-value" {
		t.Fatalf("expected 'test-cap-value', got %q", capVal)
	}
}

func TestWriteOverlayTar_RoundTrip(t *testing.T) {
	// Original OCI tar -> ext4 (with ConvertWhiteout) -> OCI tar -> verify
	content := []byte("file content here")
	originalEntries := map[string]struct {
		typeflag byte
		content  []byte
		linkname string
		mode     int64
		uid      int
		gid      int
	}{
		"upper/dir/":             {typeflag: tar.TypeDir, mode: 0755},
		"upper/dir/file.txt":     {typeflag: tar.TypeReg, content: content, mode: 0644, uid: 1000, gid: 2000},
		"upper/dir/link":         {typeflag: tar.TypeSymlink, linkname: "file.txt", mode: 0777},
		"upper/.wh.removed-file": {typeflag: tar.TypeReg, mode: 0},
	}

	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	tw.WriteHeader(&tar.Header{Name: "upper/", Mode: 0755, Typeflag: tar.TypeDir})
	for name, entry := range originalEntries {
		if name == "upper/" {
			continue
		}
		hdr := &tar.Header{
			Name:     name,
			Mode:     entry.mode,
			Uid:      entry.uid,
			Gid:      entry.gid,
			Typeflag: entry.typeflag,
			Linkname: entry.linkname,
		}
		if entry.content != nil {
			hdr.Size = int64(len(entry.content))
		}
		tw.WriteHeader(hdr)
		if entry.content != nil {
			tw.Write(entry.content)
		}
	}
	tw.Close()

	// Convert to ext4 with whiteout conversion
	imgPath := t.TempDir() + "/roundtrip.img"
	imgFile, err := os.Create(imgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := tar2ext4.ConvertTarToExt4(&tarBuf, imgFile, tar2ext4.ConvertWhiteout); err != nil {
		imgFile.Close()
		t.Fatal(err)
	}
	imgFile.Close()

	// Convert back to OCI tar
	f, fsys := openExt4FS(t, imgPath)
	defer f.Close()

	var outBuf bytes.Buffer
	if err := fstar.WriteOverlayTar(context.Background(), &outBuf, fsys, "upper", ext4reader.NewMetadataFunc()); err != nil {
		t.Fatal(err)
	}

	// Verify output tar
	tr := tar.NewReader(&outBuf)
	foundEntries := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		foundEntries[hdr.Name] = true

		switch hdr.Name {
		case "dir/":
			if hdr.Typeflag != tar.TypeDir {
				t.Fatalf("dir: expected directory type")
			}
		case "dir/file.txt":
			if hdr.Typeflag != tar.TypeReg {
				t.Fatalf("file.txt: expected regular type")
			}
			if hdr.Uid != 1000 || hdr.Gid != 2000 {
				t.Fatalf("file.txt: expected uid:gid 1000:2000, got %d:%d", hdr.Uid, hdr.Gid)
			}
			data, _ := io.ReadAll(tr)
			if !bytes.Equal(data, content) {
				t.Fatalf("file.txt: content mismatch")
			}
		case "dir/link":
			if hdr.Typeflag != tar.TypeSymlink {
				t.Fatalf("link: expected symlink type")
			}
			if hdr.Linkname != "file.txt" {
				t.Fatalf("link: expected linkname 'file.txt', got %q", hdr.Linkname)
			}
		case ".wh.removed-file":
			// Whiteout round-tripped: OCI -> overlay char 0:0 -> OCI .wh.
			if hdr.Typeflag != tar.TypeReg {
				t.Fatalf("whiteout: expected regular file type")
			}
			if hdr.Size != 0 {
				t.Fatalf("whiteout: expected size 0")
			}
		}
	}

	// Verify all expected entries are present
	for _, required := range []string{"dir/", "dir/file.txt", "dir/link", ".wh.removed-file"} {
		if !foundEntries[required] {
			t.Fatalf("missing expected entry %q in output tar", required)
		}
	}
}

func TestWriteOverlayTar_Hardlinks(t *testing.T) {
	content := []byte("shared data")
	imgPath := createOverlayExt4(t, func(tw *tar.Writer) {
		tw.WriteHeader(&tar.Header{Name: "upper/", Mode: 0755, Typeflag: tar.TypeDir})
		tw.WriteHeader(&tar.Header{
			Name:     "upper/original.txt",
			Mode:     0644,
			Size:     int64(len(content)),
			Typeflag: tar.TypeReg,
		})
		tw.Write(content)
		tw.WriteHeader(&tar.Header{
			Name:     "upper/hardlink.txt",
			Linkname: "upper/original.txt",
			Mode:     0644,
			Typeflag: tar.TypeLink,
		})
	})

	f, fsys := openExt4FS(t, imgPath)
	defer f.Close()

	var buf bytes.Buffer
	if err := fstar.WriteOverlayTar(context.Background(), &buf, fsys, "upper", ext4reader.NewMetadataFunc()); err != nil {
		t.Fatal(err)
	}

	tr := tar.NewReader(&buf)
	var entries []tar.Header
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, *hdr)
		if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 {
			io.Copy(io.Discard, tr)
		}
	}

	// Should have exactly 2 entries
	if len(entries) != 2 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name
		}
		t.Fatalf("expected 2 entries, got %d: %v", len(entries), names)
	}

	// One should be TypeReg and one TypeLink
	hasReg := false
	hasLink := false
	for _, e := range entries {
		switch e.Typeflag {
		case tar.TypeReg:
			hasReg = true
		case tar.TypeLink:
			hasLink = true
		}
	}
	if !hasReg || !hasLink {
		t.Fatalf("expected one regular and one hardlink entry, got: %+v", entries)
	}
}

func TestWriteOverlayTar_NestedWhiteout(t *testing.T) {
	imgPath := createOverlayExt4(t, func(tw *tar.Writer) {
		tw.WriteHeader(&tar.Header{Name: "upper/", Mode: 0755, Typeflag: tar.TypeDir})
		tw.WriteHeader(&tar.Header{Name: "upper/sub/", Mode: 0755, Typeflag: tar.TypeDir})
		tw.WriteHeader(&tar.Header{
			Name:     "upper/sub/.wh.gone",
			Mode:     0,
			Size:     0,
			Typeflag: tar.TypeReg,
		})
	})

	f, fsys := openExt4FS(t, imgPath)
	defer f.Close()

	var buf bytes.Buffer
	if err := fstar.WriteOverlayTar(context.Background(), &buf, fsys, "upper", ext4reader.NewMetadataFunc()); err != nil {
		t.Fatal(err)
	}

	tr := tar.NewReader(&buf)
	foundWhiteout := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		name := hdr.Name
		if strings.HasSuffix(name, "/") {
			name = name[:len(name)-1]
		}
		base := path.Base(name)
		if base == ".wh.gone" {
			foundWhiteout = true
			if hdr.Name != "sub/.wh.gone" {
				t.Fatalf("expected 'sub/.wh.gone', got %q", hdr.Name)
			}
		}
	}
	if !foundWhiteout {
		t.Fatal("missing nested whiteout entry")
	}
}

func TestWriteOverlayTar_NoUpper(t *testing.T) {
	// Ext4 without upper/ should error
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	tw.WriteHeader(&tar.Header{Name: "other/", Mode: 0755, Typeflag: tar.TypeDir})
	tw.Close()

	imgPath := t.TempDir() + "/no-upper.img"
	imgFile, err := os.Create(imgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := tar2ext4.ConvertTarToExt4(&tarBuf, imgFile); err != nil {
		imgFile.Close()
		t.Fatal(err)
	}
	imgFile.Close()

	f, fsys := openExt4FS(t, imgPath)
	defer f.Close()

	var buf bytes.Buffer
	err = fstar.WriteOverlayTar(context.Background(), &buf, fsys, "upper", ext4reader.NewMetadataFunc())
	if err == nil {
		t.Fatal("expected error when upper/ directory missing")
	}
}
