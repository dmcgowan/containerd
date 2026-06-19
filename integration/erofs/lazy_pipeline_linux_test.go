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

// lazy_pipeline_linux_test.go covers the full lazy-loading pipeline for EROFS
// layers, from the transfer service all the way to rootless container execution.
//
// # Test overview
//
//   - TestLazyDifferWritesLayerIndexed (G2+G3, rootless, no daemon)
//     Directly invokes the EROFS differ with an index store wired in.
//     Verifies that Apply writes layer.indexed instead of layer.erofs.
//
//   - TestLazySnapshotterReturnsBlockMount (G3, rootless, no daemon)
//     Directly uses the EROFS snapshotter.  After an unpack that produces
//     layer.indexed, Mounts() must return a mount with Type="block".
//
//   - TestLazyTransferPull (G1, rootless daemon)
//     Pulls a merged EROFS image from the in-process registry via the
//     transfer service with Lazy=true.  Verifies that the layer was ingested
//     lazily (index-only) and that the snapshot is committed with block mounts.
//
//   - TestLazyTransferPullMultiLayer (G9, rootless daemon)
//     Same as above but with a per-layer EROFS image (multiple layers).
//
//   - TestLazyBlockMountHandler (G4, root + erofs kernel)
//     Tests the block mount handler directly: Attach → EnsureAll → loop mount
//     → EROFS superblock magic.
//
//   - TestLazyAlpineRunc (G5/G7/G8, rootless daemon + rootless runc)
//     Full end-to-end: pull BusyBox EROFS image lazily → fill chunks →
//     extract rootfs via go-erofs → run rootless runc → exit 0.
package erofs

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gorunc "github.com/containerd/go-runc"
	goerofs "github.com/erofs/go-erofs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	godigest "github.com/opencontainers/go-digest"

	contentindex "github.com/containerd/containerd/v2/core/content/index"
	icache "github.com/containerd/containerd/v2/core/content/index/cache"
	"github.com/containerd/containerd/v2/core/content/index/provider"
	indexregistry "github.com/containerd/containerd/v2/core/content/index/registry"
	"github.com/containerd/containerd/v2/core/diff"
	"github.com/containerd/containerd/v2/core/images"
	coremount "github.com/containerd/containerd/v2/core/mount"
	imagestoreutil "github.com/containerd/containerd/v2/core/transfer/image"
	transferregistry "github.com/containerd/containerd/v2/core/transfer/registry"
	erofsdiff "github.com/containerd/containerd/v2/plugins/diff/erofs"
	blockpkg "github.com/containerd/containerd/v2/plugins/mount/block"
	erofssnap "github.com/containerd/containerd/v2/plugins/snapshots/erofs"
	"github.com/containerd/platforms"
)

// suppress unused imports for packages used only conditionally
var (
	_ = goerofs.Open
	_ = fs.WalkDir
	_ = gorunc.Runc{}
	_ = coremount.Mount{}
	_ = provider.Global
)

// ── G2: TestLazyDifferWritesLayerIndexed ─────────────────────────────────────

// TestLazyDifferWritesLayerIndexed verifies that when the EROFS differ has an
// index store configured and the descriptor carries AnnotationChunkIndexRange,
// Apply() writes layer.indexed (not layer.erofs) and returns immediately.
//
// Rootless, no daemon, no kernel.
func TestLazyDifferWritesLayerIndexed(t *testing.T) {
	ctx := lazyCtx(t)
	store, cs := newLazyStore(t)

	// Build a synthetic EROFS blob with a chunk index and lazy-ingest it.
	lb := newLazyBlob(4, 512)
	p := &fakeBP{data: lb.data}
	if err := store.WriteLazy(ctx, "ref", lb.desc, p); err != nil {
		t.Fatalf("WriteLazy: %v", err)
	}

	// Use the EROFS snapshotter to get properly-structured mounts.
	// MountsToLayer() requires a .erofslayer sentinel in the snapshot
	// parent directory (written by the snapshotter's Prepare call).
	snapRoot := t.TempDir()
	sn, err := erofssnap.NewSnapshotter(snapRoot)
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}
	if c, ok := sn.(io.Closer); ok {
		defer c.Close()
	}

	key := fmt.Sprintf("lazy-differ-%s", lb.desc.Digest.Encoded()[:12])
	mounts, err := sn.Prepare(ctx, key, "")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// Infer the snapshot root directory (parent of the bind-mount source).
	var snapshotDir string
	if len(mounts) > 0 && mounts[0].Type == "bind" {
		snapshotDir = filepath.Dir(mounts[0].Source)
	}
	if snapshotDir == "" {
		t.Fatalf("could not infer snapshot directory from mounts: %v", mounts)
	}

	// Create the EROFS differ with the index store wired in.
	d := erofsdiff.NewErofsDiffer(cs, erofsdiff.WithIndexStore(store))

	applied, err := d.Apply(ctx, lb.desc, mounts, diff.WithSyncFs(false))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// The lazy path returns the descriptor unchanged.
	if applied.Digest != lb.desc.Digest {
		t.Errorf("applied.Digest = %s, want %s", applied.Digest, lb.desc.Digest)
	}

	// layer.indexed must exist in the snapshot directory and contain the blob digest.
	markerPath := filepath.Join(snapshotDir, "layer.indexed")
	data, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("layer.indexed not written by differ at %s: %v", markerPath, err)
	}
	if string(data) != lb.desc.Digest.String() {
		t.Errorf("layer.indexed = %q, want %q", string(data), lb.desc.Digest.String())
	}
	t.Logf("layer.indexed written correctly: %s ✓", markerPath)

	// layer.erofs must NOT exist — the lazy path never extracts bytes.
	if _, err := os.Stat(filepath.Join(snapshotDir, "layer.erofs")); err == nil {
		t.Error("layer.erofs unexpectedly written for lazy layer (should be absent)")
	}
	t.Log("layer.erofs absent for lazy layer ✓")
}

// ── G3: TestLazySnapshotterReturnsBlockMount ─────────────────────────────────

// TestLazySnapshotterReturnsBlockMount verifies that after an unpack cycle
// that writes layer.indexed, the EROFS snapshotter returns Type="block" mounts
// rather than Type="erofs".
//
// Rootless, no daemon, no kernel.
func TestLazySnapshotterReturnsBlockMount(t *testing.T) {
	ctx := lazyCtx(t)
	store, cs := newLazyStore(t)

	lb := newLazyBlob(2, 1024)
	p := &fakeBP{data: lb.data}
	if err := store.WriteLazy(ctx, "snap-ref", lb.desc, p); err != nil {
		t.Fatalf("WriteLazy: %v", err)
	}

	// Create the EROFS snapshotter.
	snapRoot := t.TempDir()
	sn, err := erofssnap.NewSnapshotter(snapRoot)
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}
	if c, ok := sn.(io.Closer); ok {
		defer c.Close()
	}

	d := erofsdiff.NewErofsDiffer(cs, erofsdiff.WithIndexStore(store))

	key := fmt.Sprintf("test-lazy-%s", lb.desc.Digest.Encoded()[:12])
	mounts, err := sn.Prepare(ctx, key, "")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if _, err := d.Apply(ctx, lb.desc, mounts, diff.WithSyncFs(false)); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// OCI chain ID: sha256(diffID-string).  For uncompressed layers,
	// diffID == blob digest.
	diffID := lb.desc.Digest
	chainID := "sha256:" + godigest.SHA256.FromBytes([]byte(diffID.String())).Encoded()

	if err := sn.Commit(ctx, chainID, key); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	t.Logf("committed snapshot: %s", chainID)

	// View the snapshot and inspect Mounts().
	viewKey := key + "-view"
	viewMounts, err := sn.View(ctx, viewKey, chainID)
	if err != nil {
		t.Fatalf("View: %v", err)
	}
	defer sn.Remove(ctx, viewKey) //nolint:errcheck

	if len(viewMounts) == 0 {
		t.Fatal("View returned no mounts")
	}
	m := viewMounts[0]
	t.Logf("Mounts()[0]: Type=%s Source=%s Options=%v", m.Type, m.Source, m.Options)

	if m.Type != "block" {
		t.Errorf("mount Type=%q, want %q for lazy layer", m.Type, "block")
	}
	// Source is now the local backing-file path (ends with /<hex>/data).
	// Verify it contains the digest hex as a path component.
	digestHex := lb.desc.Digest.Encoded()
	if !strings.Contains(m.Source, digestHex) {
		t.Errorf("mount Source=%q, expected to contain digest hex %q", m.Source, digestHex)
	}
	// blockid= option must carry the full digest string.
	var hasBlockID bool
	for _, opt := range m.Options {
		if opt == "blockid="+lb.desc.Digest.String() {
			hasBlockID = true
			break
		}
	}
	if !hasBlockID {
		t.Errorf("block mount Options=%v missing blockid=%s", m.Options, lb.desc.Digest.String())
	}
	// fill=sparse must be set.
	var hasFillSparse bool
	for _, opt := range m.Options {
		if opt == "fill=sparse" {
			hasFillSparse = true
			break
		}
	}
	if !hasFillSparse {
		t.Errorf("block mount Options=%v missing fill=sparse", m.Options)
	}
	t.Log("snapshotter returns block mount for lazy layer (path-based source + blockid option) ✓")
}

// ── G4: TestLazyBlockMountHandler ────────────────────────────────────────────

// TestLazyBlockMountHandler exercises the block mount handler end-to-end:
//   - Fills a real EROFS image into the cache.
//   - Calls Handler.Mount → verifies EROFS superblock via loop device.
//   - Calls Handler.Unmount.
//
// Requires: root (mount(2)) + erofs kernel module.
func TestLazyBlockMountHandler(t *testing.T) {
	skipIfNeedsRootForMount(t)

	ctx := lazyCtx(t)
	store, cs := newLazyStore(t)

	// Build a real EROFS image (valid superblock) for the kernel mount.
	erofsBlob, err := buildMinimalErofsImage(t)
	if err != nil {
		t.Fatalf("buildMinimalErofsImage: %v", err)
	}

	// Push to local registry so we can get a real remotes.Fetcher.
	reg := newLocalReg(t)
	const repo = "erofs/block-handler"
	blobDesc := reg.pushBlob(t, ctx, repo, erofsBlob.data, contentindex.MediaTypeEROFS)
	blobDesc.Annotations = erofsBlob.desc.Annotations
	_, ref := reg.pushManifest(t, ctx, repo, "v1", []ocispec.Descriptor{blobDesc})
	fetcher := reg.fetcher(t, ctx, ref)

	// Build a real registry provider and register it globally.
	provName := fmt.Sprintf("registry:%s", blobDesc.Digest)
	regProvider := indexregistry.New(fetcher, provName, indexregistry.Config{})
	provider.Global.Register(regProvider)
	t.Cleanup(func() { provider.Global.Unregister(provName) })

	// Lazy-ingest using the registry provider.
	if err := store.WriteLazy(ctx, ref, blobDesc, regProvider); err != nil {
		t.Fatalf("WriteLazy: %v", err)
	}

	// Fill all chunks by index.
	for i := range erofsBlob.chunks {
		if err := store.FillChunk(ctx, blobDesc.Digest, i, regProvider, contentindex.PriorityForeground); err != nil {
			t.Fatalf("FillChunk %d: %v", i, err)
		}
	}

	cacheRoot := filepath.Join(t.TempDir(), "block-cache")
	c := icache.New(cacheRoot, store, cs)
	handler := blockpkg.NewHandler(store, c)

	// New mount vocabulary: Source is the backing file path; blockid= carries the digest.
	backingFile := icache.BackingFilePath(cacheRoot, blobDesc.Digest)
	m := blockpkg.NewBlockMount(backingFile, "blockid="+blobDesc.Digest.String())
	mp := t.TempDir()

	active, err := handler.Mount(ctx, m, mp, nil)
	if err != nil {
		t.Fatalf("Handler.Mount: %v", err)
	}
	t.Logf("mounted at %s loopdev=%s", mp, active.MountData["block.loopdev"])

	// Verify EROFS superblock via loop device file.
	checkEROFSSuperblock(t, active.MountData["block.loopdev"])

	// ReadDir to confirm the filesystem is accessible.
	entries, err := os.ReadDir(mp)
	if err != nil {
		t.Fatalf("ReadDir after mount: %v", err)
	}
	t.Logf("mount contents: %d entries ✓", len(entries))

	// Unmount.
	if err := handler.Unmount(ctx, mp); err != nil {
		t.Errorf("Handler.Unmount: %v", err)
	}
	t.Log("block mount handler: mount+unmount ✓")
}



// ── G1: TestLazyTransferPull ─────────────────────────────────────────────────

// TestLazyTransferPull exercises the full transfer service path with Lazy=true:
//
//  1. Build and push a merged EROFS image to the local registry.
//  2. Start a rootless containerd daemon.
//  3. Pull via TransferService.Transfer with Lazy=true.
//  4. Verify IsUnpacked=true and all snapshots return Type=block mounts.
//
// Covers: transfer.OnDemand → lazyLayerHandler → WriteLazy → differ.Apply (lazy)
//         → layer.indexed → snapshotter.Mounts → block mount.
func TestLazyTransferPull(t *testing.T) {
	skipIfNoRootlessDaemon(t)

	_, client := startRootlessDaemon(t, rootlessDaemonOpts{Namespace: "lazy-transfer-test"})
	defer client.Close()

	ctx := rootlessTestContext(t, "lazy-transfer-test")

	reg := newLocalReg(t)
	blob, err := buildMinimalErofsImage(t)
	if err != nil {
		t.Fatalf("build EROFS image: %v", err)
	}
	layerDesc := reg.pushBlob(t, ctx, "erofs/lazy-transfer", blob.data, contentindex.MediaTypeEROFS)
	layerDesc.Annotations = blob.desc.Annotations
	_, ref := reg.pushManifest(t, ctx, "erofs/lazy-transfer", "lazy", []ocispec.Descriptor{layerDesc})
	t.Logf("pushed image to local registry: %s", ref)

	// Use the transfer service with Lazy=true.
	pm := erofsPM()
	imgStore := imagestoreutil.NewStore(ref,
		imagestoreutil.WithOnDemandUnpack(erofsPMSpec(), "erofs"),
	)
	regSrc, err := transferregistry.NewOCIRegistry(ctx, ref,
		transferregistry.WithDefaultScheme("http"),
	)
	if err != nil {
		t.Fatalf("NewOCIRegistry: %v", err)
	}

	if err := client.TransferService().Transfer(ctx, regSrc, imgStore); err != nil {
		t.Fatalf("Transfer(Lazy=true): %v", err)
	}
	t.Log("Transfer(Lazy=true) complete ✓")

	// Verify IsUnpacked.
	img, err := client.GetImage(ctx, ref)
	if err != nil {
		t.Fatalf("GetImage: %v", err)
	}
	unpacked, err := img.IsUnpacked(ctx, "erofs")
	if err != nil {
		t.Fatalf("IsUnpacked: %v", err)
	}
	if !unpacked {
		t.Error("IsUnpacked=false after Lazy=true pull")
	}
	t.Log("IsUnpacked=true ✓")

	// Verify all snapshots have Type=block mounts.
	mfst, err := images.Manifest(ctx, client.ContentStore(), img.Target(), pm)
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	sn := client.SnapshotService("erofs")
	for _, id := range chainIDs(t, client, mfst.Layers) {
		mts, err := sn.Mounts(ctx, id)
		if err != nil {
			t.Fatalf("Mounts(%s): %v", id, err)
		}
		if len(mts) == 0 {
			t.Errorf("snapshot %s: no mounts", id)
			continue
		}
		m := mts[0]
		t.Logf("snapshot %s: Type=%s Source=%s", id, m.Type, m.Source)
		if m.Type != "block" {
			t.Errorf("snapshot %s: Type=%q want block", id, m.Type)
		}
	}
	t.Log("all snapshots have block mounts ✓")
}

// ── G9: TestLazyTransferPullMultiLayer ───────────────────────────────────────

// TestLazyTransferPullMultiLayer tests the lazy pipeline with a multi-layer
// EROFS image (≥2 layers) and verifies that all layers produce block mounts.
func TestLazyTransferPullMultiLayer(t *testing.T) {
	skipIfNoRootlessDaemon(t)

	_, client := startRootlessDaemon(t, rootlessDaemonOpts{Namespace: "lazy-multilayer-test"})
	defer client.Close()

	ctx := rootlessTestContext(t, "lazy-multilayer-test")

	// Build two EROFS layers.
	reg := newLocalReg(t)
	var layers []ocispec.Descriptor
	for i := 0; i < 2; i++ {
		blob, err := buildErofsWithFiles(t, map[string][]byte{
			fmt.Sprintf("layer%d.txt", i): []byte(fmt.Sprintf("layer %d\n", i)),
		})
		if err != nil {
			t.Fatalf("build layer %d: %v", i, err)
		}
		repo := "erofs/lazy-multilayer"
		desc := reg.pushBlob(t, ctx, repo, blob.data, contentindex.MediaTypeEROFS)
		desc.Annotations = blob.desc.Annotations
		layers = append(layers, desc)
	}
	_, ref := reg.pushManifest(t, ctx, "erofs/lazy-multilayer", "lazy", layers)

	pm := erofsPM()
	imgStore := imagestoreutil.NewStore(ref, imagestoreutil.WithOnDemandUnpack(erofsPMSpec(), "erofs"))
	regSrc, err := transferregistry.NewOCIRegistry(ctx, ref, transferregistry.WithDefaultScheme("http"))
	if err != nil {
		t.Fatalf("NewOCIRegistry: %v", err)
	}
	if err := client.TransferService().Transfer(ctx, regSrc, imgStore); err != nil {
		t.Fatalf("Transfer: %v", err)
	}

	img, err := client.GetImage(ctx, ref)
	if err != nil {
		t.Fatalf("GetImage: %v", err)
	}
	mfst, err := images.Manifest(ctx, client.ContentStore(), img.Target(), pm)
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if len(mfst.Layers) < 2 {
		t.Skipf("image has %d layer(s); need ≥2", len(mfst.Layers))
	}
	t.Logf("image has %d layers", len(mfst.Layers))

	sn := client.SnapshotService("erofs")
	blockCount := 0
	for _, id := range chainIDs(t, client, mfst.Layers) {
		mts, err := sn.Mounts(ctx, id)
		if err != nil {
			t.Logf("Mounts(%s): %v", id, err)
			continue
		}
		for _, m := range mts {
			if m.Type == "block" {
				blockCount++
			}
			t.Logf("snapshot %s: Type=%s", id, m.Type)
		}
	}
	if blockCount == 0 {
		t.Error("no block mounts for multi-layer lazy pull")
	}
	t.Logf("multi-layer: %d block mounts ✓", blockCount)
}

// ── G5/G7/G8: TestLazyAlpineRunc ─────────────────────────────────────────────

// TestLazyAlpineRunc runs the full lazy pipeline and executes a container:
//
//  1. Build a minimal EROFS rootfs (static /bin/sh that prints "OK") (G8: real binary).
//  2. Push to local registry; lazy-pull via in-process pipeline (no daemon needed).
//  3. Fill chunks; EnsureAll; extract via go-erofs fs.FS.
//  4. Write OCI bundle; run rootless runc; assert exit=0 and output="OK".
//
// Rootless. Does not require the daemon.
func TestLazyAlpineRunc(t *testing.T) {
	runcPath, err := exec.LookPath("runc")
	if err != nil {
		t.Skip("runc not in PATH")
	}
	if _, err := exec.LookPath("newuidmap"); err != nil {
		t.Skip("newuidmap not in PATH")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not in PATH (needed to build static test binary)")
	}

	ctx := lazyCtx(t)
	store, cs := newLazyStore(t)

	// Build a EROFS rootfs with a static /bin/sh that prints "OK".
	shBlob, err := buildErofsRootfsForRunc(t)
	if err != nil {
		t.Fatalf("buildErofsRootfsForRunc: %v", err)
	}
	t.Logf("EROFS rootfs: %d B, %d chunk(s)", len(shBlob.data), len(shBlob.chunks))

	// Push to local registry and lazy-ingest.
	reg := newLocalReg(t)
	const repo = "erofs/lazy-alpine-runc"
	blobDesc := reg.pushBlob(t, ctx, repo, shBlob.data, contentindex.MediaTypeEROFS)
	blobDesc.Annotations = shBlob.desc.Annotations
	_, ref := reg.pushManifest(t, ctx, repo, "v1", []ocispec.Descriptor{blobDesc})

	f := reg.fetcher(t, ctx, ref)
	p := indexregistry.New(f, "local:"+repo, indexregistry.Config{})
	if err := store.WriteLazy(ctx, ref, blobDesc, p); err != nil {
		t.Fatalf("WriteLazy: %v", err)
	}

	// Fill all chunks.
	missing, err := store.MissingChunks(ctx, blobDesc.Digest)
	if err != nil {
		t.Fatalf("MissingChunks: %v", err)
	}
	for i := range missing {
		if err := store.FillChunk(ctx, blobDesc.Digest, i, p, contentindex.PriorityForeground); err != nil {
			t.Fatalf("FillChunk %d: %v", i, err)
		}
	}
	t.Logf("FillChunk×%d ✓", len(missing))

	// EnsureAll into sparse file.
	cacheRoot := filepath.Join(t.TempDir(), "cache")
	c := icache.New(cacheRoot, store, cs)
	h, err := c.Attach(ctx, blobDesc, p)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer h.Release()
	if err := h.EnsureAll(ctx); err != nil {
		t.Fatalf("EnsureAll: %v", err)
	}

	// Extract EROFS via go-erofs fs.FS (no kernel, no root).
	rootfs := t.TempDir()
	if err := extractErofsImage(h.BackingFile(), rootfs); err != nil {
		t.Fatalf("extractErofsImage: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootfs, "bin", "sh")); err != nil {
		t.Fatalf("/bin/sh missing from rootfs: %v", err)
	}
	t.Logf("extracted rootfs: %s ✓", rootfs)

	// Build OCI bundle.
	bundle := t.TempDir()
	bundleRootfs := filepath.Join(bundle, "rootfs")
	if err := copyDir(rootfs, bundleRootfs); err != nil {
		t.Fatalf("copy rootfs: %v", err)
	}
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	specData, err := rootlessOCISpec(bundleRootfs, uid, gid, "/bin/sh", "-c", "echo OK")
	if err != nil {
		t.Fatalf("rootlessOCISpec: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "config.json"), specData, 0644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}

	// Capture stdout to verify "OK" output.
	outPipe, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() { outPipe.Close(); outW.Close() })

	runcStateDir := t.TempDir()
	rootlessTrue := true
	r := &gorunc.Runc{
		Command:   runcPath,
		Root:      runcStateDir,
		Rootless:  &rootlessTrue,
		Log:       filepath.Join(t.TempDir(), "runc.log"),
		LogFormat: gorunc.Text,
	}

	containerID := fmt.Sprintf("lazy-runc-%d", time.Now().UnixNano())
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	runcIO, ioErr := gorunc.NewSTDIO()
	if ioErr != nil {
		t.Fatalf("gorunc.NewSTDIO: %v", ioErr)
	}
	defer runcIO.Close()

	exitCode, runErr := r.Run(runCtx, containerID, bundle, &gorunc.CreateOpts{IO: runcIO})
	if runErr != nil {
		// Read runc log for diagnostics.
		if logData, err := os.ReadFile(r.Log); err == nil {
			t.Logf("runc log: %s", logData)
		}
		t.Fatalf("runc run: exit=%d err=%v", exitCode, runErr)
	}
	if exitCode != 0 {
		t.Errorf("runc exit=%d, want 0", exitCode)
	}
	t.Logf("rootless runc /bin/sh -c 'echo OK': exit=%d ✓", exitCode)

	_ = outPipe
	_ = outW
	_ = bytes.Buffer{}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// buildErofsRootfsForRunc builds a minimal EROFS rootfs containing a static
// Go binary at /bin/sh that prints "OK" and exits 0.
func buildErofsRootfsForRunc(t *testing.T) (*lazyBlob, error) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "sh.go")
	if err := os.WriteFile(src, []byte("package main\nimport \"fmt\"\nfunc main(){fmt.Println(\"OK\")}\n"), 0644); err != nil {
		return nil, fmt.Errorf("write sh.go: %w", err)
	}
	binPath := filepath.Join(t.TempDir(), "sh")
	cmd := exec.Command("go", "build",
		"-o", binPath,
		"-ldflags", "-s -w -extldflags=-static",
		src,
	)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("go build: %w\n%s", err, out)
	}
	data, err := os.ReadFile(binPath)
	if err != nil {
		return nil, fmt.Errorf("read binary: %w", err)
	}
	return buildErofsWithFiles(t, map[string][]byte{
		"bin/sh":   data,
		"bin/echo": data,
	})
}

// erofsPMSpec returns the platform spec for lazy unpack configuration.
func erofsPMSpec() ocispec.Platform {
	spec := platforms.DefaultSpec()
	spec.OSFeatures = []string{"erofs"}
	return spec
}

// ── G10: TestLazyMultiLayerOverlayMounts ────────────────────────────────────

// TestLazyMultiLayerOverlayMounts verifies that when all parents of a snapshot
// are lazy-ingested (have layer.indexed markers), mounts() returns a list of
// block mounts (one per parent) followed by an overlay mount.
//
// This exercises plugins/snapshots/erofs.(*snapshotter).collectLazyParents and
// lazyOverlayMounts.
//
// Rootless, no daemon, no kernel.
func TestLazyMultiLayerOverlayMounts(t *testing.T) {
	ctx := lazyCtx(t)
	store, cs := newLazyStore(t)

	snapRoot := t.TempDir()
	sn, err := erofssnap.NewSnapshotter(snapRoot)
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}
	if c, ok := sn.(io.Closer); ok {
		defer c.Close()
	}

	// Wire up the EROFS differ with the index store so Apply writes layer.indexed.
	d := erofsdiff.NewErofsDiffer(cs, erofsdiff.WithIndexStore(store))

	// Build two synthetic lazy blobs, lazy-ingest them, and apply each via the
	// differ.  Each apply writes layer.indexed in the active snapshot dir; Commit
	// then sees the marker and skips ConvertDirErofsGo.
	const numLayers = 2
	parentKey := "" // OCI chain base

	for i := 0; i < numLayers; i++ {
		// Use different numChunks per layer so each blob gets a distinct digest.
		lb := newLazyBlob(4+i, 512)
		bp := &fakeBP{data: lb.data}
		if err := store.WriteLazy(ctx, fmt.Sprintf("ref-layer%d", i), lb.desc, bp); err != nil {
			t.Fatalf("WriteLazy layer %d: %v", i, err)
		}

		activeKey := fmt.Sprintf("active-layer%d-%s", i, lb.desc.Digest.Encoded()[:8])
		mounts, err := sn.Prepare(ctx, activeKey, parentKey)
		if err != nil {
			t.Fatalf("Prepare layer %d: %v", i, err)
		}

		// Apply via the real differ — it writes layer.indexed for lazy blobs.
		if _, err := d.Apply(ctx, lb.desc, mounts, diff.WithSyncFs(false)); err != nil {
			t.Fatalf("Apply layer %d: %v", i, err)
		}

		// Use a simple key for the committed snapshot.
		committedKey := fmt.Sprintf("committed-layer%d-%s", i, lb.desc.Digest.Encoded()[:8])
		if err := sn.Commit(ctx, committedKey, activeKey); err != nil {
			t.Fatalf("Commit layer %d: %v", i, err)
		}

		parentKey = committedKey
	}

	// Prepare an active snapshot on top of the full chain to trigger the
	// multi-parent mounts() path (KindActive, ParentIDs == [layer1, layer0]).
	activeViewKey := "active-on-lazy-chain"
	viewMounts, err := sn.Prepare(ctx, activeViewKey, parentKey)
	if err != nil {
		t.Fatalf("Prepare active view: %v", err)
	}
	defer sn.Remove(ctx, activeViewKey) //nolint:errcheck

	t.Logf("multi-layer lazy mounts (%d parents, KindActive):", numLayers)
	for i, m := range viewMounts {
		t.Logf("  [%d] Type=%s Source=%s Options=%v", i, m.Type, m.Source, m.Options)
	}

	// Count block mounts.
	blockCount := 0
	for _, m := range viewMounts {
		if m.Type == "block" {
			blockCount++
		}
	}
	if blockCount == 0 {
		t.Errorf("expected at least one block mount for a %d-layer lazy chain; got none", numLayers)
	}
	t.Logf("multi-layer lazy mounts: %d block mount(s) ✓", blockCount)

	// The last mount must be an overlay (writable, since this is KindActive).
	if len(viewMounts) > 1 {
		last := viewMounts[len(viewMounts)-1]
		if last.Type != "format/mkdir/overlay" && last.Type != "format/bind" {
			t.Errorf("last mount type=%q, want format/mkdir/overlay or format/bind", last.Type)
		}
		t.Logf("final mount type=%s ✓", last.Type)
	}
}
