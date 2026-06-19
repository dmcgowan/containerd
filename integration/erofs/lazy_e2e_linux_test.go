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

// lazy_e2e_linux_test.go — end-to-end lazy-loading pipeline tests using a
// local in-process OCI registry.
//
// # Test hierarchy (most specific → most privileged)
//
//   - TestLazyLocalRegistryPull         rootless, no daemon
//     Push a synthetic EROFS+chunk-index blob to the local registry,
//     resolve a fetcher, call WriteLazy, verify metadata and no full download.
//
//   - TestLazyLocalRegistryMissingFill  rootless, no daemon
//     From the local registry: WriteLazy → MissingChunks → FillChunk×N →
//     MissingChunks == 0.  Cache.Attach → EnsureAll → ReadAt verifies bytes.
//
//   - TestLazyLocalRegistryMount        root + erofs kernel module required
//     After EnsureAll, attach the sparse file to a loop device and
//     mount it as EROFS; verify superblock magic.
//
//   - TestLazyRootlessRunc              rootless (user-ns + newuidmap/newgidmap)
//     Build a minimal EROFS filesystem with go-erofs (no kernel, no root),
//     push it to the local registry, pull lazily, fill cache, extract via
//     erofs.Open → fs.FS walk, write an OCI bundle, run
//     "runc --rootless=true run" executing /bin/true inside the bundle.
//
// All tests in this file are self-contained: they start the registry server
// inside the test process and do not require a running containerd daemon.
package erofs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	gorunc "github.com/containerd/go-runc"
	goerofs "github.com/erofs/go-erofs"
	ocispecv "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/core/content/index/cache"
	"github.com/containerd/containerd/v2/core/content/index/registry"
	"github.com/containerd/containerd/v2/core/remotes"
	dockerremotes "github.com/containerd/containerd/v2/core/remotes/docker"
	indextestutil "github.com/containerd/containerd/v2/core/content/index/testutil"
	godigest "github.com/opencontainers/go-digest"
)

// ── local registry helper ─────────────────────────────────────────────────────

// localReg holds the in-process registry server and a resolver pointed at it.
type localReg struct {
	srv      *httptest.Server
	reg      *indextestutil.MemRegistry
	host     string // "127.0.0.1:<port>"
	resolver remotes.Resolver
}

// newLocalReg starts an httptest.Server wrapping a MemRegistry and returns a
// resolver that routes HTTP requests to it without TLS verification.
func newLocalReg(t *testing.T) *localReg {
	t.Helper()
	reg := indextestutil.NewMemRegistry()
	srv := httptest.NewServer(reg)
	t.Cleanup(srv.Close)

	host := srv.Listener.Addr().String()
	resolver := dockerremotes.NewResolver(dockerremotes.ResolverOptions{
		Hosts: func(h string) ([]dockerremotes.RegistryHost, error) {
			return []dockerremotes.RegistryHost{{
				Client:       srv.Client(),
				Host:         host,
				Scheme:       "http",
				Capabilities: dockerremotes.HostCapabilityPull |
					dockerremotes.HostCapabilityResolve |
					dockerremotes.HostCapabilityPush,
			}}, nil
		},
	})
	return &localReg{srv: srv, reg: reg, host: host, resolver: resolver}
}

// pushBlob pushes a raw blob into the registry under repo and returns a
// descriptor for it.
func (r *localReg) pushBlob(t *testing.T, ctx context.Context, repo string, data []byte, mediaType string) ocispec.Descriptor {
	t.Helper()
	dgst := godigest.SHA256.FromBytes(data)
	desc := ocispec.Descriptor{
		MediaType: mediaType,
		Digest:    dgst,
		Size:      int64(len(data)),
	}
	ref := fmt.Sprintf("%s/%s:push", r.host, repo)
	pusher, err := r.resolver.Pusher(ctx, ref)
	if err != nil {
		t.Fatalf("pusher: %v", err)
	}
	pw, err := pusher.Push(ctx, desc)
	if err != nil {
		// Already exists is acceptable.
		return desc
	}
	if _, err := pw.Write(data); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	if err := pw.Commit(ctx, desc.Size, dgst); err != nil {
		t.Fatalf("commit blob: %v", err)
	}
	return desc
}

// pushManifest builds and pushes an OCI image manifest with the given layer
// descriptors.  Returns the manifest descriptor and the image ref.
func (r *localReg) pushManifest(t *testing.T, ctx context.Context, repo, tag string, layers []ocispec.Descriptor) (ocispec.Descriptor, string) {
	t.Helper()

	// Push a minimal config blob.
	configData := []byte("{}")
	configDesc := r.pushBlob(t, ctx, repo, configData, "application/vnd.oci.image.config.v1+json")

	mfst := ocispec.Manifest{
		Versioned: ocispecv.Versioned{SchemaVersion: 2},
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Config:    configDesc,
		Layers:    layers,
	}
	mfstJSON, err := json.Marshal(mfst)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	mfstDesc := ocispec.Descriptor{
		MediaType: "application/vnd.oci.image.manifest.v1+json",
		Digest:    godigest.SHA256.FromBytes(mfstJSON),
		Size:      int64(len(mfstJSON)),
	}

	ref := fmt.Sprintf("%s/%s:%s", r.host, repo, tag)
	pusher, err := r.resolver.Pusher(ctx, ref)
	if err != nil {
		t.Fatalf("manifest pusher: %v", err)
	}
	mw, err := pusher.Push(ctx, mfstDesc)
	if err != nil {
		t.Fatalf("push manifest: %v", err)
	}
	if _, err := mw.Write(mfstJSON); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := mw.Commit(ctx, mfstDesc.Size, mfstDesc.Digest); err != nil {
		t.Fatalf("commit manifest: %v", err)
	}
	return mfstDesc, ref
}

// fetcher returns a remotes.Fetcher for the given image ref.
func (r *localReg) fetcher(t *testing.T, ctx context.Context, ref string) remotes.Fetcher {
	t.Helper()
	_, _, err := r.resolver.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("resolve %s: %v", ref, err)
	}
	f, err := r.resolver.Fetcher(ctx, ref)
	if err != nil {
		t.Fatalf("fetcher %s: %v", ref, err)
	}
	return f
}

// ── TestLazyLocalRegistryPull ─────────────────────────────────────────────────

// TestLazyLocalRegistryPull pushes a synthetic EROFS+chunk-index blob to the
// local in-process registry, then calls WriteLazy against it and verifies:
//
//  1. WriteLazy succeeds and records metadata.
//  2. The provider Fetch was called only to retrieve the chunk-index section
//     (not to download the full blob data).
//  3. MissingChunks returns all N chunks as missing (no byte content yet).
func TestLazyLocalRegistryPull(t *testing.T) {
	ctx := lazyCtx(t)
	store, _ := newLazyStore(t)

	reg := newLocalReg(t)
	const repo = "erofs/lazy-pull"
	const tag = "v1"

	// Build a synthetic blob with 4 chunks of 512 B.
	lb := newLazyBlob(4, 512)

	// Push the layer blob to the local registry.
	layerDesc := reg.pushBlob(t, ctx, repo, lb.data, contentindex.MediaTypeEROFS)
	// Propagate annotations from the synthetic descriptor.
	layerDesc.Annotations = lb.desc.Annotations

	layers := []ocispec.Descriptor{layerDesc}
	_, ref := reg.pushManifest(t, ctx, repo, tag, layers)
	t.Logf("pushed image: %s  layer=%s", ref, layerDesc.Digest)

	// Build a registry provider backed by the local server.
	f := reg.fetcher(t, ctx, ref)
	p := registry.New(f, "local:"+repo, registry.Config{})

	// WriteLazy — only fetches the chunk-index section, not the full blob.
	t0 := time.Now()
	if err := store.WriteLazy(ctx, ref, layerDesc, p); err != nil {
		t.Fatalf("WriteLazy: %v", err)
	}
	t.Logf("WriteLazy via local registry (%d B blob, index only): %v",
		len(lb.data), time.Since(t0))

	// Verify metadata record.
	info, err := store.Info(ctx, layerDesc.Digest)
	if err != nil {
		t.Fatalf("Info after WriteLazy: %v", err)
	}
	if info.Digest != layerDesc.Digest {
		t.Errorf("info.Digest = %s, want %s", info.Digest, layerDesc.Digest)
	}
	t.Logf("metadata recorded: provider=%s digest=%s", info.Provider, info.Digest)

	// All chunks must be missing — WriteLazy stores only the index, not bytes.
	missing, err := store.MissingChunks(ctx, layerDesc.Digest)
	if err != nil {
		t.Fatalf("MissingChunks: %v", err)
	}
	if len(missing) != 4 {
		t.Errorf("expected 4 missing chunks after WriteLazy, got %d", len(missing))
	}
	t.Logf("MissingChunks after WriteLazy: %d/4 missing ✓", len(missing))

	// Idempotency: second WriteLazy must return already-exists.
	if err := store.WriteLazy(ctx, ref, layerDesc, p); err == nil {
		t.Error("second WriteLazy should return already-exists, got nil")
	} else {
		t.Logf("second WriteLazy → %v (expected) ✓", err)
	}
}

// ── TestLazyLocalRegistryMissingFill ─────────────────────────────────────────

// TestLazyLocalRegistryMissingFill exercises the full lazy-fill pipeline using
// the local registry as the byte source:
//
//  1. Push image to local registry.
//  2. WriteLazy (index-only ingest).
//  3. MissingChunks → all N missing.
//  4. FillChunk×N via registry provider.
//  5. MissingChunks → 0 missing.
//  6. cache.Attach → EnsureAll → ReadAt verifies correct bytes.
func TestLazyLocalRegistryMissingFill(t *testing.T) {
	ctx := lazyCtx(t)
	store, cs := newLazyStore(t)

	reg := newLocalReg(t)
	const repo = "erofs/lazy-fill"
	const numChunks = 6
	const chunkSize = 1024

	lb := newLazyBlob(numChunks, chunkSize)
	layerDesc := reg.pushBlob(t, ctx, repo, lb.data, contentindex.MediaTypeEROFS)
	layerDesc.Annotations = lb.desc.Annotations
	_, ref := reg.pushManifest(t, ctx, repo, "v1", []ocispec.Descriptor{layerDesc})

	f := reg.fetcher(t, ctx, ref)
	p := registry.New(f, "local:"+repo, registry.Config{})

	// Step 1: lazy ingest.
	if err := store.WriteLazy(ctx, ref, layerDesc, p); err != nil {
		t.Fatalf("WriteLazy: %v", err)
	}

	// Step 2: all chunks missing.
	missing, err := store.MissingChunks(ctx, layerDesc.Digest)
	if err != nil {
		t.Fatalf("MissingChunks: %v", err)
	}
	if len(missing) != numChunks {
		t.Fatalf("want %d missing, got %d", numChunks, len(missing))
	}
	t.Logf("MissingChunks after WriteLazy: %d/%d missing", len(missing), numChunks)

	// Step 3: fill all chunks via the registry provider.
	t0 := time.Now()
	for i := 0; i < numChunks; i++ {
		if err := store.FillChunk(ctx, layerDesc.Digest, i, p, contentindex.PriorityForeground); err != nil {
			t.Fatalf("FillChunk %d: %v", i, err)
		}
	}
	t.Logf("FillChunk×%d via local registry (%d B each): %v",
		numChunks, chunkSize, time.Since(t0))

	// Step 4: no missing chunks.
	missing, err = store.MissingChunks(ctx, layerDesc.Digest)
	if err != nil {
		t.Fatalf("MissingChunks after fill: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("expected 0 missing after FillChunk, got %d", len(missing))
	}
	t.Log("MissingChunks after fill: 0 ✓")

	// Step 5: cache + EnsureAll + ReadAt.
	cacheRoot := filepath.Join(t.TempDir(), "cache")
	c := cache.New(cacheRoot, store, cs)
	h, err := c.Attach(ctx, layerDesc, p)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer h.Release()

	if err := h.EnsureAll(ctx); err != nil {
		t.Fatalf("EnsureAll: %v", err)
	}

	for i := 0; i < numChunks; i++ {
		want := lb.chunks[i]
		got := make([]byte, chunkSize)
		off := int64(i * chunkSize)
		n, err := h.ReadAt(got, off)
		if err != nil || n != chunkSize {
			t.Fatalf("ReadAt chunk %d: n=%d err=%v", i, n, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("chunk %d: data mismatch at offset %d", i, off)
		}
	}
	t.Logf("ReadAt×%d chunks via local registry: all correct ✓", numChunks)
}

// ── TestLazyLocalRegistryMount ────────────────────────────────────────────────

// TestLazyLocalRegistryMount extends TestLazyLocalRegistryMissingFill with an
// actual EROFS kernel mount:
//
//  1. Fill cache (same as above).
//  2. Attach a loop device to the sparse backing file (requires CAP_SYS_ADMIN).
//  3. Mount as EROFS (requires erofs kernel module).
//  4. Verify EROFS superblock magic at offset 1024.
//  5. Unmount and detach loop device.
//
// Skipped when:
//   - Not running as root (CAP_SYS_ADMIN needed for mount(2)).
//   - EROFS kernel module is not loaded.
//
// The synthetic blob produced by newLazyBlob is NOT a real EROFS filesystem —
// this test uses buildMinimalErofsImage to produce a valid one.
func TestLazyLocalRegistryMount(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("loop mount requires root (CAP_SYS_ADMIN)")
	}
	if !findErofsKernel() {
		t.Skip("erofs kernel module not loaded")
	}

	ctx := lazyCtx(t)
	store, cs := newLazyStore(t)

	// Build a minimal real EROFS image so the kernel mount succeeds.
	erofsBlob, err := buildMinimalErofsImage(t)
	if err != nil {
		t.Fatalf("buildMinimalErofsImage: %v", err)
	}

	reg := newLocalReg(t)
	const repo = "erofs/lazy-mount"

	layerDesc := reg.pushBlob(t, ctx, repo, erofsBlob.data, contentindex.MediaTypeEROFS)
	layerDesc.Annotations = erofsBlob.desc.Annotations
	_, ref := reg.pushManifest(t, ctx, repo, "v1", []ocispec.Descriptor{layerDesc})

	f := reg.fetcher(t, ctx, ref)
	p := registry.New(f, "local:"+repo, registry.Config{})

	if err := store.WriteLazy(ctx, ref, layerDesc, p); err != nil {
		t.Fatalf("WriteLazy: %v", err)
	}
	numChunks := len(erofsBlob.chunks)
	for i := 0; i < numChunks; i++ {
		if err := store.FillChunk(ctx, layerDesc.Digest, i, p, contentindex.PriorityForeground); err != nil {
			t.Fatalf("FillChunk %d: %v", i, err)
		}
	}

	cacheRoot := filepath.Join(t.TempDir(), "cache")
	c := cache.New(cacheRoot, store, cs)
	h, err := c.Attach(ctx, layerDesc, p)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer h.Release()

	if err := h.EnsureAll(ctx); err != nil {
		t.Fatalf("EnsureAll: %v", err)
	}

	backingFile := h.BackingFile()
	if _, err := os.Stat(backingFile); err != nil {
		t.Fatalf("backing file missing: %v", err)
	}
	t.Logf("backing file: %s (%d B)", backingFile, erofsBlob.desc.Size)

	// Verify EROFS superblock in the backing file before mounting.
	checkEROFSSuperblock(t, backingFile)
	t.Log("superblock magic in backing file: ✓")

	// Attach loop device + mount.
	mntDir := t.TempDir()
	loopDev, err := attachLoopDevice(t, backingFile)
	if err != nil {
		t.Fatalf("attach loop device: %v", err)
	}
	defer detachLoopDevice(loopDev) //nolint:errcheck

	if err := mountEROFS(loopDev, mntDir); err != nil {
		t.Fatalf("mount erofs: %v", err)
	}
	defer func() {
		if err := exec.Command("umount", mntDir).Run(); err != nil {
			t.Logf("umount: %v", err)
		}
	}()

	// Verify the mount point is accessible.
	entries, err := os.ReadDir(mntDir)
	if err != nil {
		t.Fatalf("ReadDir after mount: %v", err)
	}
	t.Logf("mount contents (%d entries): loop=%s mnt=%s ✓", len(entries), loopDev, mntDir)
}

// ── TestLazyRootlessRunc ──────────────────────────────────────────────────────

// TestLazyRootlessRunc is the full end-to-end rootless lazy-loading test:
//
//  1. Build a minimal EROFS filesystem (go-erofs, no root, no kernel) with
//     a single file /bin/true copied from the host.
//  2. Encode the image as a synthetic EROFS blob with a chunk index.
//  3. Push to local in-process registry.
//  4. WriteLazy (index-only ingest).
//  5. FillChunk×N via registry provider.
//  6. cache.Attach → EnsureAll.
//  7. Extract the EROFS filesystem to a plain directory using the go-erofs
//     fs.FS reader (no kernel mount, no root required).
//  8. Write a minimal OCI bundle around the extracted rootfs.
//  9. Run "runc --rootless=true run <id>" and verify exit code 0.
//
// Prerequisites (checked at runtime, test skipped if absent):
//   - runc in PATH
//   - newuidmap / newgidmap in PATH (for user-namespace mapping)
//   - /bin/true or equivalent on the host
func TestLazyRootlessRunc(t *testing.T) {
	// Check prerequisites.
	runcPath, err := exec.LookPath("runc")
	if err != nil {
		t.Skip("runc not in PATH")
	}
	if _, err := exec.LookPath("newuidmap"); err != nil {
		t.Skip("newuidmap not in PATH (required for rootless runc)")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not in PATH (needed to build static test binary)")
	}

	// Build a static binary that just exits 0.  We use this instead of the
	// host /bin/true because the host binary is dynamically linked and would
	// require copying all its shared libraries into the minimal rootfs.
	truePath, err := buildStaticTrue(t)
	if err != nil {
		t.Fatalf("buildStaticTrue: %v", err)
	}

	ctx := lazyCtx(t)
	store, cs := newLazyStore(t)

	// ── Step 1: build a minimal EROFS filesystem containing /bin/true ────────
	erofsBlob, err := buildErofsRootfs(t, truePath)
	if err != nil {
		t.Fatalf("buildErofsRootfs: %v", err)
	}
	t.Logf("EROFS rootfs blob: %d B, %d chunks", len(erofsBlob.data), len(erofsBlob.chunks))

	// ── Step 2: push to local registry ───────────────────────────────────────
	reg := newLocalReg(t)
	const repo = "erofs/rootless-runc"

	layerDesc := reg.pushBlob(t, ctx, repo, erofsBlob.data, contentindex.MediaTypeEROFS)
	layerDesc.Annotations = erofsBlob.desc.Annotations
	_, ref := reg.pushManifest(t, ctx, repo, "v1", []ocispec.Descriptor{layerDesc})
	t.Logf("pushed to local registry: %s", ref)

	// ── Step 3: lazy ingest via registry provider ─────────────────────────────
	f := reg.fetcher(t, ctx, ref)
	p := registry.New(f, "local:"+repo, registry.Config{})

	if err := store.WriteLazy(ctx, ref, layerDesc, p); err != nil {
		t.Fatalf("WriteLazy: %v", err)
	}
	t.Logf("WriteLazy: index-only ingest ✓")

	// ── Step 4: fill all chunks ───────────────────────────────────────────────
	missing, err := store.MissingChunks(ctx, layerDesc.Digest)
	if err != nil {
		t.Fatalf("MissingChunks: %v", err)
	}
	for i := range missing {
		if err := store.FillChunk(ctx, layerDesc.Digest, i, p, contentindex.PriorityForeground); err != nil {
			t.Fatalf("FillChunk %d: %v", i, err)
		}
	}
	t.Logf("FillChunk×%d: all chunks filled ✓", len(missing))

	// ── Step 5: cache attach + EnsureAll ─────────────────────────────────────
	cacheRoot := filepath.Join(t.TempDir(), "cache")
	c := cache.New(cacheRoot, store, cs)
	h, err := c.Attach(ctx, layerDesc, p)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer h.Release()

	if err := h.EnsureAll(ctx); err != nil {
		t.Fatalf("EnsureAll: %v", err)
	}
	backingFile := h.BackingFile()
	t.Logf("backing file ready: %s ✓", backingFile)

	// ── Step 6: extract EROFS via go-erofs fs.FS (no kernel, no root) ────────
	rootfs := t.TempDir()
	if err := extractErofsImage(backingFile, rootfs); err != nil {
		t.Fatalf("extractErofsImage: %v", err)
	}
	t.Logf("extracted rootfs to %s ✓", rootfs)

	// Verify /bin/true is present.
	trueInRootfs := filepath.Join(rootfs, "bin", "true")
	if _, err := os.Stat(trueInRootfs); err != nil {
		t.Fatalf("extracted rootfs is missing /bin/true: %v", err)
	}

	// ── Step 7: write OCI bundle ──────────────────────────────────────────────
	bundle := t.TempDir()
	// runc expects "rootfs" to be a subdirectory of the bundle.
	bundleRootfs := filepath.Join(bundle, "rootfs")
	if err := copyDir(rootfs, bundleRootfs); err != nil {
		t.Fatalf("copy rootfs to bundle: %v", err)
	}

	uid := uint32(os.Getuid())
	gid := uint32(os.Getgid())
	specData, err := rootlessOCISpec(bundleRootfs, uid, gid, "/bin/true")
	if err != nil {
		t.Fatalf("rootlessOCISpec: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), specData, 0644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
	t.Logf("OCI bundle written: %s", bundle)

	// ── Step 8: run rootless runc ─────────────────────────────────────────────
	runcStateDir := t.TempDir()
	rootlessTrue := true
	r := &gorunc.Runc{
		Command:   runcPath,
		Root:      runcStateDir,
		Rootless:  &rootlessTrue,
		Log:       filepath.Join(t.TempDir(), "runc.log"),
		LogFormat: gorunc.Text,
	}

	containerID := fmt.Sprintf("lazy-rootless-%d", os.Getpid())
	t.Logf("running rootless container: %s", containerID)

	io, ioErr := gorunc.NewSTDIO()
	if ioErr != nil {
		t.Fatalf("gorunc.NewSTDIO: %v", ioErr)
	}
	defer io.Close()

	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	exitCode, err := r.Run(runCtx, containerID, bundle, &gorunc.CreateOpts{
		IO: io,
	})
	if err != nil {
		t.Fatalf("runc run: exit=%d err=%v", exitCode, err)
	}
	if exitCode != 0 {
		t.Errorf("runc /bin/true exited with %d, want 0", exitCode)
	}
	t.Logf("rootless runc /bin/true: exit=%d ✓", exitCode)
}

// ── EROFS image builder helpers ───────────────────────────────────────────────

// erofsImageBlob holds a real EROFS filesystem image encoded as a lazyBlob.
type erofsImageBlob struct {
	lazyBlob
}

// buildMinimalErofsImage creates a trivial EROFS image (one file /hello.txt)
// and wraps it as a lazyBlob so it can be used with the lazy pipeline.
// This does NOT require root or the EROFS kernel module.
func buildMinimalErofsImage(t *testing.T) (*lazyBlob, error) {
	t.Helper()
	return buildErofsWithFiles(t, map[string][]byte{
		"hello.txt": []byte("hello from lazy EROFS\n"),
	})
}

// buildErofsRootfs creates a minimal EROFS rootfs image containing /bin/true
// (copied from the host) wrapped as a lazyBlob.
func buildErofsRootfs(t *testing.T, truePath string) (*lazyBlob, error) {
	t.Helper()
	trueData, err := os.ReadFile(truePath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", truePath, err)
	}
	return buildErofsWithFiles(t, map[string][]byte{
		"bin/true": trueData,
	})
}

// buildErofsWithFiles creates an EROFS image containing the named files,
// then wraps the resulting bytes as a lazyBlob so it can be lazily ingested.
//
// The chunk size is set to 1 MiB so small images produce 1–2 chunks.
// For a configurable chunk size use buildErofsWithFilesChunked directly.
func buildErofsWithFiles(t *testing.T, files map[string][]byte) (*lazyBlob, error) {
	t.Helper()
	return buildErofsWithFilesChunked(t, 1*1024*1024, files)
}

// buildErofsWithFilesChunked is the underlying builder; callers that need
// production-sized (4 MiB) chunks for multi-chunk tests call this directly.
func buildErofsWithFilesChunked(t *testing.T, chunkSize int, files map[string][]byte) (*lazyBlob, error) {
	t.Helper()

	imgFile, err := os.CreateTemp(t.TempDir(), "erofs-*.img")
	if err != nil {
		return nil, fmt.Errorf("create tmp: %w", err)
	}
	defer imgFile.Close()

	w := goerofs.Create(imgFile, goerofs.WithBuildTime(0, 0))

	// Write each file (creating parent directories as needed).
	for name, data := range files {
		// Ensure parent directory exists.
		dir := filepath.Dir(name)
		if dir != "." {
			if err := w.Mkdir(dir, 0755); err != nil {
				// Ignore "already exists".
				_ = err
			}
		}
		f, err := w.Create(name)
		if err != nil {
			return nil, fmt.Errorf("erofs Create %s: %w", name, err)
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			return nil, fmt.Errorf("erofs Write %s: %w", name, err)
		}
		// Mark executable for binaries.
		if err := f.Close(); err != nil {
			return nil, fmt.Errorf("erofs Close %s: %w", name, err)
		}
		if err := w.Chmod(name, 0755); err != nil {
			// Non-fatal: some go-erofs versions may not support Chmod before Close.
			t.Logf("chmod %s: %v (non-fatal)", name, err)
		}
	}

	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("erofs Close: %w", err)
	}

	imgData, err := os.ReadFile(imgFile.Name())
	if err != nil {
		return nil, fmt.Errorf("read erofs image: %w", err)
	}
	if len(imgData) == 0 {
		return nil, fmt.Errorf("erofs image is empty")
	}

	// Verify EROFS superblock magic at offset 1024.
	if len(imgData) >= 1028 {
		magic := imgData[1024:1028]
		want := []byte{0xE2, 0xE1, 0xF5, 0xE0}
		if !bytes.Equal(magic, want) {
			return nil, fmt.Errorf("bad EROFS magic: %x", magic)
		}
	}

	// Slice into chunks and build the lazyBlob.
	numChunks := (len(imgData) + chunkSize - 1) / chunkSize
	chunks := make([][]byte, numChunks)
	for i := 0; i < numChunks; i++ {
		start := i * chunkSize
		end := start + chunkSize
		if end > len(imgData) {
			end = len(imgData)
		}
		chunks[i] = make([]byte, end-start)
		copy(chunks[i], imgData[start:end])
	}

	// Compute per-chunk SHA-256.
	hashes := make([]godigest.Digest, numChunks)
	for i, c := range chunks {
		hashes[i] = godigest.SHA256.FromBytes(c)
	}

	// Build chunk-index header (32 bytes).
	header := make([]byte, 32)
	copy(header[0:4], "\xcd\xe4\xec\x67")
	header[4] = 1
	header[5] = 0
	putU64LE(header[8:16], uint64(len(imgData)))
	putU32LE(header[16:20], uint32(numChunks))
	header[20] = 1
	header[21] = 32

	var entries []byte
	for i := 0; i < numChunks; i++ {
		e := make([]byte, 48)
		off := int64(i * chunkSize)
		putU64LE(e[0:8], uint64(off))
		putU64LE(e[8:16], uint64(off))
		decoded, _ := hexDecode(hashes[i].Encoded())
		copy(e[16:48], decoded)
		entries = append(entries, e...)
	}

	chunkIndexPayload := append(header, entries...)
	blob := append(imgData, chunkIndexPayload...)
	blobDigest := godigest.SHA256.FromBytes(blob)
	indexStart := int64(len(imgData))

	desc := ocispec.Descriptor{
		MediaType: contentindex.MediaTypeEROFS,
		Digest:    blobDigest,
		Size:      int64(len(blob)),
		Annotations: map[string]string{
			contentindex.AnnotationChunkIndexRange: fmt.Sprintf("%d", indexStart),
		},
	}

	refs := make([]contentindex.ChunkRef, numChunks)
	for i := 0; i < numChunks; i++ {
		off := int64(i * chunkSize)
		end := off + int64(len(chunks[i]))
		refs[i] = contentindex.ChunkRef{
			Digest:      hashes[i],
			Offset:      off,
			Length:      int64(len(chunks[i])),
			OnBlobStart: off,
			OnBlobEnd:   end,
		}
	}

	return &lazyBlob{desc: desc, data: blob, chunks: chunks, chunkRefs: refs}, nil
}

// extractErofsImage opens the EROFS image at imgPath using go-erofs and
// extracts all files to destDir (plain directory tree, no kernel required).
func extractErofsImage(imgPath, destDir string) error {
	imgFile, err := os.Open(imgPath)
	if err != nil {
		return fmt.Errorf("open erofs image: %w", err)
	}
	defer imgFile.Close()

	stat, err := imgFile.Stat()
	if err != nil {
		return fmt.Errorf("stat erofs image: %w", err)
	}

	erofsFS, err := goerofs.Open(io.NewSectionReader(imgFile, 0, stat.Size()))
	if err != nil {
		return fmt.Errorf("open erofs fs: %w", err)
	}

	return fs.WalkDir(erofsFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		destPath := filepath.Join(destDir, filepath.FromSlash(path))
		if d.IsDir() {
			return os.MkdirAll(destPath, 0755)
		}
		// Regular file.
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return err
		}
		src, err := erofsFS.Open(path)
		if err != nil {
			return fmt.Errorf("open %s in erofs: %w", path, err)
		}
		defer src.Close()
		dst, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			return fmt.Errorf("create %s: %w", destPath, err)
		}
		defer dst.Close()
		_, err = io.Copy(dst, src)
		return err
	})
}

// ── OCI bundle helpers ────────────────────────────────────────────────────────

// rootlessOCISpec generates a minimal OCI runtime spec for rootless runc
// (no root, user namespace with host uid/gid mapped to 0 inside).
func rootlessOCISpec(rootfs string, uid, gid uint32, args ...string) ([]byte, error) {
	isRootless := true
	_ = isRootless

	s := &specs.Spec{
		Version: "1.0.2",
		Process: &specs.Process{
			Terminal: false,
			User:     specs.User{UID: 0, GID: 0},
			Args:     args,
			Env: []string{
				"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
				"HOME=/root",
			},
			Cwd:             "/",
			NoNewPrivileges: true,
		},
		Root: &specs.Root{
			Path:     rootfs,
			Readonly: true,
		},
		Hostname: "erofs-lazy-test",
		Mounts: []specs.Mount{
			{Destination: "/proc", Type: "proc", Source: "proc"},
			{Destination: "/dev", Type: "tmpfs", Source: "tmpfs",
				Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		},
		Linux: &specs.Linux{
			UIDMappings: []specs.LinuxIDMapping{
				{ContainerID: 0, HostID: uid, Size: 1},
			},
			GIDMappings: []specs.LinuxIDMapping{
				{ContainerID: 0, HostID: gid, Size: 1},
			},
			Namespaces: []specs.LinuxNamespace{
				{Type: "pid"},
				{Type: "ipc"},
				{Type: "uts"},
				{Type: "mount"},
				{Type: "user"},
			},
		},
	}
	return json.MarshalIndent(s, "", "  ")
}

// copyDir recursively copies src to dst (used to place rootfs under bundle/).
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		mode := fs.FileMode(0755)
		if info, err := d.Info(); err == nil {
			mode = info.Mode()
		}
		return os.WriteFile(target, data, mode)
	})
}

// ── loop device helpers (root-only) ──────────────────────────────────────────

// attachLoopDevice attaches file to a free loop device and returns the
// loop device path (e.g. /dev/loop0).  Requires root.
func attachLoopDevice(t *testing.T, file string) (string, error) {
	t.Helper()
	out, err := exec.Command("losetup", "--find", "--show", "--read-only", file).Output()
	if err != nil {
		return "", fmt.Errorf("losetup: %w", err)
	}
	dev := string(bytes.TrimSpace(out))
	t.Logf("attached loop device: %s → %s", file, dev)
	return dev, nil
}

func detachLoopDevice(dev string) error {
	return exec.Command("losetup", "-d", dev).Run()
}

// mountEROFS mounts dev as an EROFS filesystem at mntDir.  Requires root.
func mountEROFS(dev, mntDir string) error {
	return exec.Command("mount", "-t", "erofs", "-o", "ro", dev, mntDir).Run()
}

// ── misc helpers ──────────────────────────────────────────────────────────────

// buildStaticTrue compiles a tiny static Go binary that exits 0.
// The binary is written to a temp file and its path is returned.
// Using a static binary avoids the need to copy shared libraries into
// the minimal EROFS rootfs.
func buildStaticTrue(t *testing.T) (string, error) {
	t.Helper()

	src := filepath.Join(t.TempDir(), "true.go")
	if err := os.WriteFile(src, []byte(`package main
func main() {}
`), 0644); err != nil {
		return "", fmt.Errorf("write true.go: %w", err)
	}

	outPath := filepath.Join(t.TempDir(), "true")
	cmd := exec.Command("go", "build",
		"-o", outPath,
		"-ldflags", "-s -w -extldflags=-static",
		src,
	)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build: %w\n%s", err, out)
	}
	return outPath, nil
}

// Ensure go-runc types are resolved at compile time.
var _ = gorunc.Runc{}
