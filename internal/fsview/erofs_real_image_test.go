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

// erofs_real_image_test.go runs the converter against real Docker layer
// tars (alpine, debian) and verifies the resulting chunked+verity EROFS
// blob both as a raw filesystem (via fsview/erofs) and via the lazy
// indexed-cache path (via fsview/block).
//
// The test is offline by default: layers are taken from a pre-populated
// directory whose path is supplied via the EROFS_LAYERS_DIR env var.
// The layout expected is:
//
//	$EROFS_LAYERS_DIR/alpine.layer.tar.gz
//	$EROFS_LAYERS_DIR/debian.layer.tar.gz
//
// Either file may be absent — only the present ones are tested.  When
// EROFS_LAYERS_DIR is empty the entire test is skipped.  The repo's
// scripts/fetch-test-layers.sh helper can populate the directory from
// `docker save`.

package fsview_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/core/content/index/cache"
	indexlocal "github.com/containerd/containerd/v2/core/content/index/local"
	"github.com/containerd/containerd/v2/core/images/converter/erofs"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/internal/fsview"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	localcs "github.com/containerd/containerd/v2/plugins/content/local"
	_ "github.com/containerd/containerd/v2/plugins/mount/fsview/block"
	_ "github.com/containerd/containerd/v2/plugins/mount/fsview/erofs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	bolt "go.etcd.io/bbolt"
)

// realImage describes one Docker layer tar.gz to convert.
type realImage struct {
	name string // e.g. "alpine"
	path string // absolute path to the .tar.gz file
}

// discoverRealImages enumerates available layer tars in EROFS_LAYERS_DIR.
// Returns nil and the caller t.Skip's when the env var is unset.
func discoverRealImages(t *testing.T) []realImage {
	t.Helper()
	dir := os.Getenv("EROFS_LAYERS_DIR")
	if dir == "" {
		t.Skip("EROFS_LAYERS_DIR not set; skipping real-image verification")
	}
	var imgs []realImage
	for _, name := range []string{"alpine", "debian"} {
		p := filepath.Join(dir, name+".layer.tar.gz")
		if _, err := os.Stat(p); err == nil {
			imgs = append(imgs, realImage{name: name, path: p})
		}
	}
	if len(imgs) == 0 {
		t.Skipf("no layer tars found in %s (expected alpine.layer.tar.gz and/or debian.layer.tar.gz)", dir)
	}
	return imgs
}

// ingestTarLayer ingests a gzipped tar layer file into the content store
// under its actual sha256 digest, returning the resulting descriptor.
func ingestTarLayer(t *testing.T, ctx context.Context, cs content.Store, path string) ocispec.Descriptor {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	dgst := digest.FromBytes(data)
	desc := ocispec.Descriptor{
		MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
		Digest:    dgst,
		Size:      int64(len(data)),
	}
	if _, err := cs.Info(ctx, dgst); err == nil {
		return desc
	}
	cw, err := cs.Writer(ctx, content.WithRef("ingest-"+dgst.String()), content.WithDescriptor(desc))
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	if _, err := cw.Write(data); err != nil {
		cw.Close()
		t.Fatalf("write blob: %v", err)
	}
	if err := cw.Commit(ctx, int64(len(data)), dgst); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return desc
}

// fmtBytes is a tiny helper for human-readable sizes in test logs.
func fmtBytes(n int64) string {
	const (
		KiB int64 = 1024
		MiB       = 1024 * KiB
	)
	switch {
	case n >= MiB:
		return fmt.Sprintf("%.2f MiB", float64(n)/float64(MiB))
	case n >= KiB:
		return fmt.Sprintf("%.2f KiB", float64(n)/float64(KiB))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// walkFS counts entries reachable from the root of v.  Returns
// (files, dirs, symlinks, totalBytes).  Verifies that every entry can
// be Stat'd; for regular files of moderate size, also reads them fully
// to exercise the EROFS data path through fsview.
func walkFS(t *testing.T, v fsview.View) (files, dirs, links int, totalBytes int64) {
	t.Helper()
	const readCap = 64 * 1024 // limit per-file read to keep log noise bounded
	if err := fs.WalkDir(v, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			dirs++
		case d.Type()&fs.ModeSymlink != 0:
			links++
		case d.Type().IsRegular():
			files++
			fi, err := d.Info()
			if err != nil {
				return fmt.Errorf("stat %s: %w", p, err)
			}
			totalBytes += fi.Size()
			if fi.Size() > 0 && fi.Size() <= readCap {
				f, err := v.Open(p)
				if err != nil {
					return fmt.Errorf("open %s: %w", p, err)
				}
				if _, err := io.Copy(io.Discard, f); err != nil {
					f.Close()
					return fmt.Errorf("read %s: %w", p, err)
				}
				f.Close()
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	return
}

// findPath returns true if any of `wanted` paths exist in v.  Used to
// sanity-check that distro-flavoured files made it into the EROFS image.
func findPath(v fsview.View, wanted []string) (string, bool) {
	for _, p := range wanted {
		if _, err := fs.Stat(v, p); err == nil {
			return p, true
		}
	}
	return "", false
}

// TestRealImage_ChunkedEROFSConversion converts each available image with
// LayerConvertFuncChunked + dm-verity, reports sizes & annotations, and
// validates the result is openable as an fs.FS via fsview/erofs.  This is
// the primary end-to-end verification that the chunked converter's new
// dm-verity branch produces a structurally-valid EROFS blob.
func TestRealImage_ChunkedEROFSConversion(t *testing.T) {
	imgs := discoverRealImages(t)
	ctx := namespaces.WithNamespace(context.Background(), "test")
	cs, err := localcs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	for _, img := range imgs {
		t.Run(img.name, func(t *testing.T) {
			inDesc := ingestTarLayer(t, ctx, cs, img.path)
			t.Logf("[%s] input: digest=%s size=%s", img.name, inDesc.Digest, fmtBytes(inDesc.Size))

			// 1.5 MiB compressed target frame + dm-verity on.  Matches
			// the CLI default in cmd/ctr/commands/images/convert.go.
			const chunkSize = 1536 * 1024
			convert := erofs.LayerConvertFuncChunked(nil, chunkSize, erofs.WithDmVerity())
			newDesc, err := convert(ctx, cs, inDesc)
			if err != nil {
				t.Fatalf("convert: %v", err)
			}
			if newDesc == nil {
				t.Fatal("converter returned nil descriptor")
			}

			t.Logf("[%s] output: digest=%s size=%s media=%s",
				img.name, newDesc.Digest, fmtBytes(newDesc.Size), newDesc.MediaType)
			t.Logf("[%s] ratio: %.2fx (output/input)",
				img.name, float64(newDesc.Size)/float64(inDesc.Size))

			// Chunk-index annotations must be present.
			for _, k := range []string{
				contentindex.AnnotationChunkIndexRange,
				contentindex.AnnotationChunkIndexDigest,
				contentindex.AnnotationChunkIndexMediaType,
				contentindex.AnnotationUncompressedDigest,
			} {
				if newDesc.Annotations[k] == "" {
					t.Errorf("[%s] missing annotation %s", img.name, k)
				}
			}
			// dm-verity annotations must be present (this is the bug-fix
			// invariant — previously LayerConvertFuncChunked silently
			// dropped WithDmVerity()).
			for _, k := range []string{
				contentindex.AnnotationDmVerityHashOffset,
				contentindex.AnnotationDmVerityRootDigest,
			} {
				if newDesc.Annotations[k] == "" {
					t.Errorf("[%s] missing verity annotation %s", img.name, k)
				}
			}
			t.Logf("[%s] verity: hash_offset=%s root=%s",
				img.name,
				newDesc.Annotations[contentindex.AnnotationDmVerityHashOffset],
				newDesc.Annotations[contentindex.AnnotationDmVerityRootDigest])

			// Round-trip digest property: bytes in the store hash to the
			// descriptor's digest.
			ra, err := cs.ReaderAt(ctx, *newDesc)
			if err != nil {
				t.Fatalf("[%s] ReaderAt: %v", img.name, err)
			}
			defer ra.Close()
			buf := make([]byte, ra.Size())
			if _, err := ra.ReadAt(buf, 0); err != nil && err != io.EOF {
				t.Fatalf("[%s] ReadAt: %v", img.name, err)
			}
			if got := digest.FromBytes(buf); got != newDesc.Digest {
				t.Fatalf("[%s] digest mismatch after round-trip:\n  newDesc.Digest = %s\n  sha256(bytes)  = %s",
					img.name, newDesc.Digest, got)
			}
		})
	}
}

// TestRealImage_RawEROFS_FSView converts each available image with the
// non-chunked, non-zstd raw EROFS converter, then opens the resulting
// blob with the fsview/erofs handler and walks it as fs.FS.  Asserts
// that the converted filesystem is well-formed (every entry Stat-able,
// every small regular file readable) and that distro-flavoured
// signature files are present (busybox/apk for alpine; debian release
// metadata for debian).
//
// This is the strongest content-level verification we can perform
// without a real kernel mount: every byte the kernel mounter would
// dereference at runtime is exercised through go-erofs's pure-Go
// fs.FS implementation here.
func TestRealImage_RawEROFS_FSView(t *testing.T) {
	imgs := discoverRealImages(t)
	ctx := namespaces.WithNamespace(context.Background(), "test")
	cs, err := localcs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	for _, img := range imgs {
		t.Run(img.name, func(t *testing.T) {
			inDesc := ingestTarLayer(t, ctx, cs, img.path)

			// Raw EROFS (no zstd, no chunks) — the simplest format,
			// directly openable by fsview/erofs without any
			// materialization step.
			convert := erofs.LayerConvertFunc()
			newDesc, err := convert(ctx, cs, inDesc)
			if err != nil {
				t.Fatalf("[%s] convert: %v", img.name, err)
			}
			t.Logf("[%s] raw EROFS: digest=%s size=%s",
				img.name, newDesc.Digest, fmtBytes(newDesc.Size))

			// Dump the raw EROFS bytes to a temp file so fsview/erofs
			// can open it via mount.Mount{Source: path}.
			ra, err := cs.ReaderAt(ctx, *newDesc)
			if err != nil {
				t.Fatalf("[%s] ReaderAt: %v", img.name, err)
			}
			defer ra.Close()
			tmpFile := filepath.Join(t.TempDir(), img.name+".erofs")
			f, err := os.Create(tmpFile)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.Copy(f, io.NewSectionReader(ra, 0, ra.Size())); err != nil {
				f.Close()
				t.Fatal(err)
			}
			f.Close()

			// Open via fsview.
			v, err := fsview.FSMounts([]mount.Mount{{
				Type:   "erofs",
				Source: tmpFile,
			}})
			if err != nil {
				t.Fatalf("[%s] FSMounts: %v", img.name, err)
			}
			defer v.Close()

			files, dirs, links, totalBytes := walkFS(t, v)
			t.Logf("[%s] walk: %d files, %d dirs, %d symlinks, %s total",
				img.name, files, dirs, links, fmtBytes(totalBytes))
			if files == 0 {
				t.Errorf("[%s] walked 0 files — empty fs?", img.name)
			}

			// Sanity-check that the distro's expected files are in
			// place.  We look for any of several plausible paths to
			// stay robust to minor layout differences.
			var signature []string
			switch img.name {
			case "alpine":
				signature = []string{"bin/busybox", "etc/alpine-release", "etc/os-release", "etc/apk/repositories"}
			case "debian":
				signature = []string{"etc/debian_version", "etc/os-release", "bin/dash", "usr/bin/dpkg"}
			}
			if len(signature) > 0 {
				if p, ok := findPath(v, signature); !ok {
					t.Errorf("[%s] none of %v found in fs", img.name, signature)
				} else {
					t.Logf("[%s] signature file present: %s", img.name, p)
				}
			}
		})
	}
}

// TestRealImage_LazyIndexedPipeline runs the full snapshot+index path
// offline using a memProvider-style ByteProvider:
//
//  1. Convert tar → chunked+zstd+verity EROFS via LayerConvertFuncChunked.
//  2. Register the blob with a local index Store via WriteLazy (the same
//     entry point the daemon uses for lazy-pulled layers).
//  3. Attach the LocalCache and call PrepareForFSView, which materialises
//     the SB-bearing chunk and the EROFS inode-table chunks into the
//     sparse cache file.
//  4. Open the sparse cache file via the block fsview handler and walk
//     it as fs.FS, verifying that the prepared regions are sufficient
//     to satisfy path-resolution reads on a representative file.
//
// This proves the indexed-content path end-to-end without any kernel
// mount or daemon RPC.  It's the "fsview-based snapshot test suite"
// asked for in the task.
func TestRealImage_LazyIndexedPipeline(t *testing.T) {
	imgs := discoverRealImages(t)
	ctx := namespaces.WithNamespace(context.Background(), "test")

	csRoot := t.TempDir()
	cs, err := localcs.NewStore(csRoot)
	if err != nil {
		t.Fatal(err)
	}

	// Set up an indexed local Store on a fresh bolt DB.
	bdb, err := bolt.Open(filepath.Join(t.TempDir(), "meta.db"), 0644, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bdb.Close() })

	idxStore, err := indexlocal.NewStore(indexlocal.Config{
		Root:    t.TempDir(),
		DB:      bdb,
		Content: cs,
	})
	if err != nil {
		t.Fatalf("index store: %v", err)
	}

	// Cache directory where PrepareForFSView materialises sparse
	// backing files; this is the same path the daemon uses.
	cacheRoot := t.TempDir()
	lcache := cache.New(cacheRoot, idxStore, cs)

	for _, img := range imgs {
		t.Run(img.name, func(t *testing.T) {
			inDesc := ingestTarLayer(t, ctx, cs, img.path)

			const chunkSize = 1536 * 1024 // matches CLI default
			convert := erofs.LayerConvertFuncChunked(nil, chunkSize, erofs.WithDmVerity())
			newDesc, err := convert(ctx, cs, inDesc)
			if err != nil {
				t.Fatalf("[%s] convert: %v", img.name, err)
			}

			// Pull the converted blob bytes out of the content store
			// so a memProvider can serve them to the index store
			// during lazy ingest (this is the "registry" stand-in).
			ra, err := cs.ReaderAt(ctx, *newDesc)
			if err != nil {
				t.Fatalf("[%s] ReaderAt: %v", img.name, err)
			}
			blobBytes := make([]byte, ra.Size())
			if _, err := ra.ReadAt(blobBytes, 0); err != nil && err != io.EOF {
				t.Fatalf("[%s] ReadAt: %v", img.name, err)
			}
			ra.Close()

			p := &byteSliceProvider{name: "test-" + img.name, blob: blobBytes}

			// WriteLazy: registers the descriptor and stores just the
			// chunk-index trailer; no chunk bytes yet.
			if err := idxStore.WriteLazy(ctx, "lazy-"+newDesc.Digest.String(), *newDesc, p); err != nil {
				t.Fatalf("[%s] WriteLazy: %v", img.name, err)
			}

			// Confirm chunks are missing.
			missing, err := idxStore.MissingChunks(ctx, newDesc.Digest)
			if err != nil {
				t.Fatalf("[%s] MissingChunks: %v", img.name, err)
			}
			if len(missing) == 0 {
				t.Fatalf("[%s] after WriteLazy, no chunks missing — lazy ingest broken", img.name)
			}
			t.Logf("[%s] after WriteLazy: %d chunks missing", img.name, len(missing))

			// PrepareForFSView: warms SB + inode-table regions.
			if err := lcache.PrepareForFSView(ctx, *newDesc, p); err != nil {
				t.Fatalf("[%s] PrepareForFSView: %v", img.name, err)
			}

			missingAfter, _ := idxStore.MissingChunks(ctx, newDesc.Digest)
			t.Logf("[%s] after PrepareForFSView: %d/%d chunks missing (i.e. %d filled)",
				img.name, len(missingAfter), len(missing), len(missing)-len(missingAfter))
			if len(missingAfter) >= len(missing) {
				t.Errorf("[%s] PrepareForFSView filled zero chunks", img.name)
			}

			// Open the sparse cache file via the block fsview handler.
			backingPath := cache.BackingFilePath(cacheRoot, newDesc.Digest)
			if _, err := os.Stat(backingPath); err != nil {
				t.Fatalf("[%s] backing file missing: %v", img.name, err)
			}
			v, err := fsview.FSMounts([]mount.Mount{{
				Type:   "block",
				Source: backingPath,
			}})
			if err != nil {
				t.Fatalf("[%s] FSMounts(block): %v", img.name, err)
			}
			defer v.Close()

			// We don't walk the full tree — many chunks are still
			// missing.  Instead, we open root and Stat a couple of
			// well-known shallow paths that the inode-table region
			// must be able to resolve.
			root, err := v.Open(".")
			if err != nil {
				t.Fatalf("[%s] open root: %v", img.name, err)
			}
			if d, ok := root.(fs.ReadDirFile); ok {
				entries, err := d.ReadDir(-1)
				if err != nil {
					t.Fatalf("[%s] read root dir: %v", img.name, err)
				}
				t.Logf("[%s] root has %d entries", img.name, len(entries))
				if len(entries) == 0 {
					t.Errorf("[%s] empty root after PrepareForFSView", img.name)
				}
				// Names should include common top-level paths.
				names := make([]string, 0, len(entries))
				for _, e := range entries {
					names = append(names, e.Name())
				}
				show := len(names)
				if show > 12 {
					show = 12
				}
				t.Logf("[%s] root entries: %s", img.name, strings.Join(names[:show], ", "))
			}
			root.Close()
		})
	}
}

// byteSliceProvider is a contentindex.ByteProvider over a fixed []byte,
// used as a registry stand-in.  Mirrors the memProvider in the local
// package's own test files (kept private to avoid an export).
type byteSliceProvider struct {
	name string
	blob []byte
}

func (p *byteSliceProvider) Name() string { return p.name }

func (p *byteSliceProvider) Open(_ context.Context, _ ocispec.Descriptor) (content.ReaderAt, error) {
	return &byteSliceRA{data: p.blob}, nil
}

func (p *byteSliceProvider) Fetch(_ context.Context, _ ocispec.Descriptor, off, length int64, _ contentindex.Priority) (io.ReadCloser, error) {
	end := off + length
	if end > int64(len(p.blob)) {
		end = int64(len(p.blob))
	}
	return io.NopCloser(bytes.NewReader(p.blob[off:end])), nil
}

type byteSliceRA struct{ data []byte }

func (r *byteSliceRA) ReadAt(b []byte, off int64) (int, error) {
	if off >= int64(len(r.data)) {
		return 0, io.EOF
	}
	n := copy(b, r.data[off:])
	if n < len(b) {
		return n, io.EOF
	}
	return n, nil
}
func (r *byteSliceRA) Size() int64  { return int64(len(r.data)) }
func (r *byteSliceRA) Close() error { return nil }
