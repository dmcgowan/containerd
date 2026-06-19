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

// lazy_gc_linux_test.go — end-to-end verification that the sparse-file cache
// is reclaimed by the indexed-content store's garbage collection.
//
// The cache backend holds no GC state of its own: it is a pure digest-keyed
// byte store (<root>/blobs/<hex>/{data,present.bm}).  Lifetime is bound to the
// indexed-content blob it caches, which is kept reachable by the manifest's
// forward reference:
//
//	Manifest content blob
//	  └─ gc.ref.content-index.l.<i> = <digest>   (pins the indexed blob)
//	       indexed-content blob (ResourceContentIndex)
//	         └─ References()  → content chunks (kept alive on demand)
//	         └─ when collected, the index store's GC calls Cache.Remove(digest)
//
// Scenarios covered (single rootless-daemon spin-up):
//
//  1. Pull with OnDemand=true → warmer materialises the cache directory,
//     keyed purely by digest (no namespace, no GC sidecar).
//
//  2. Manifest carries the gc.ref.content-index.l.<i> forward edge.
//
//  3. Release the pull lease + GC → cache PERSISTS, because it is bound to
//     the indexed blob (still referenced by the image), not the lease.
//
//  4. images.SynchronousDelete + GC → the indexed blob becomes unreferenced,
//     the index store's collector removes it and calls Cache.Remove, and the
//     on-disk cache directory is reaped.
package erofs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	imagestoreutil "github.com/containerd/containerd/v2/core/transfer/image"
	transferregistry "github.com/containerd/containerd/v2/core/transfer/registry"

	containerd "github.com/containerd/containerd/v2/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// TestLazyCacheGCLifecycle is the canonical integration test for the
// io.containerd.cache.v1 plugin's GC wiring.  It walks an EROFS image
// through the full lazy-load lifecycle and asserts the side-effects at
// each stage are observable through the public API plus the cache plugin's
// on-disk state.
//
// The test deliberately avoids exercising the containerd runtime / runc —
// the cache-blob resource is created at Attach time (warmer), and the GC
// behaviour we care about is independent of whether a container ever ran.
// See TestLazyAlpineRunc for the runc-backed end-to-end happy path.
func TestLazyCacheGCLifecycle(t *testing.T) {
	skipIfNoRootlessDaemon(t)

	const ns = "lazy-gc-test"
	d, client := startRootlessDaemon(t, rootlessDaemonOpts{
		Namespace: ns,
		ExtraConfig: `
[plugins."io.containerd.gc.v1.scheduler"]
  mutation_threshold = 1
`,
	})
	defer client.Close()

	ctx := rootlessTestContext(t, ns)

	// ── Push a minimal EROFS image with an embedded chunk-index ──────────
	reg := newLocalReg(t)
	blob, err := buildMinimalErofsImage(t)
	if err != nil {
		t.Fatalf("buildMinimalErofsImage: %v", err)
	}
	layerDesc := reg.pushBlob(t, ctx, "erofs/lazy-gc", blob.data, contentindex.MediaTypeEROFS)
	layerDesc.Annotations = blob.desc.Annotations
	_, ref := reg.pushManifest(t, ctx, "erofs/lazy-gc", "lazy", []ocispec.Descriptor{layerDesc})
	t.Logf("pushed lazy image: %s\n  layer digest: %s", ref, layerDesc.Digest)

	// Sanity-check: the layer descriptor must carry the chunk-index annotation.
	if _, ok := layerDesc.Annotations[contentindex.AnnotationChunkIndexRange]; !ok {
		t.Fatalf("layer descriptor missing %s annotation; cannot exercise lazy path",
			contentindex.AnnotationChunkIndexRange)
	}

	// ── Step 1: Create a lease and pull under it ─────────────────────────
	// The lease pins the warm cache during the window between pull and run.
	// If no container attaches before the lease expires, the cache may be
	// reaped — that is the intended fallback.
	lease, err := client.LeasesService().Create(ctx, leases.WithID("lazy-gc-test-pull-lease"))
	if err != nil {
		t.Fatalf("create lease: %v", err)
	}
	leaseCtx := leases.WithLease(ctx, lease.ID)

	imgStore := imagestoreutil.NewStore(ref,
		imagestoreutil.WithOnDemandUnpack(erofsPMSpec(), "erofs"),
	)
	regSrc, err := transferregistry.NewOCIRegistry(leaseCtx, ref,
		transferregistry.WithDefaultScheme("http"),
	)
	if err != nil {
		t.Fatalf("NewOCIRegistry: %v", err)
	}
	if err := client.TransferService().Transfer(leaseCtx, regSrc, imgStore); err != nil {
		t.Fatalf("Transfer(OnDemand=true): %v", err)
	}
	t.Log("transfer with OnDemand=true complete ✓")

	// ── Step 2: Cache file materialised by the warmer ─────────────────────
	// The cache backend is addressed purely by blob digest — no namespace and
	// no per-blob GC sidecar.  Layout: <root>/io.containerd.cache.v1.local/blobs/<hex>/
	cacheBlobsRoot := filepath.Join(d.rootDir, "io.containerd.cache.v1.local", "blobs")
	layerHex := strings.TrimPrefix(layerDesc.Digest.String(), "sha256:")
	cacheBlobDir := filepath.Join(cacheBlobsRoot, layerHex)
	waitForCondition(t, 5*time.Second, "cache directory materialised", func() bool {
		fi, err := os.Stat(cacheBlobDir)
		return err == nil && fi.IsDir()
	})
	t.Logf("cache blob directory materialised at %s ✓", cacheBlobDir)

	// ── Step 3: Manifest has forward gc.ref.content-index.l.<i> edge ─────
	img, err := client.GetImage(ctx, ref)
	if err != nil {
		t.Fatalf("GetImage: %v", err)
	}
	mfstInfo, err := client.ContentStore().Info(ctx, img.Target().Digest)
	if err != nil {
		t.Fatalf("manifest content Info: %v", err)
	}
	var fwdEdgeKey string
	for k, v := range mfstInfo.Labels {
		if strings.HasPrefix(k, "containerd.io/gc.ref.content-index.l.") && v == layerDesc.Digest.String() {
			fwdEdgeKey = k
			break
		}
	}
	if fwdEdgeKey == "" {
		t.Errorf("manifest %s missing gc.ref.content-index.l.* forward edge to %s\n  labels: %#v",
			img.Target().Digest, layerDesc.Digest, mfstInfo.Labels)
	} else {
		t.Logf("manifest forward edge: %s = %s ✓", fwdEdgeKey, layerDesc.Digest)
	}

	// ── Step 4: Release the lease + GC — cache must persist ──────────────
	// The cache is now bound to the indexed-content blob's lifetime, not the
	// lease.  Releasing the pull lease and running GC must NOT reap the cache,
	// because the image still references the indexed blob via the manifest.
	if err := client.LeasesService().Delete(ctx, lease, leases.SynchronousDelete); err != nil {
		t.Fatalf("delete lease: %v", err)
	}
	triggerGC(t, ctx, client)
	if fi, err := os.Stat(cacheBlobDir); err != nil || !fi.IsDir() {
		t.Errorf("cache reaped after lease release while image still present: %v", err)
	} else {
		t.Log("cache persists across GC while the image references the indexed blob ✓")
	}

	// ── Step 5: image rm + GC → indexed blob collected → cache reaped ────
	if err := client.ImageService().Delete(ctx, ref, images.SynchronousDelete()); err != nil {
		t.Fatalf("ImageService.Delete: %v", err)
	}
	t.Log("image deleted; triggering GC...")
	triggerGC(t, ctx, client)

	if !eventuallyTrue(t, 5*time.Second, func() bool {
		_, err := os.Stat(cacheBlobDir)
		return os.IsNotExist(err)
	}) {
		t.Errorf("cache blob directory still present after image rm + GC: %s", cacheBlobDir)
		dumpCacheDir(t, cacheBlobsRoot)
	} else {
		t.Log("cache blob directory reaped after image rm + GC (index-driven) ✓")
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// triggerGC churns leases to push past the scheduler's mutation threshold
// (set to 1 via ExtraConfig) and waits a moment for the sweep to finish.
func triggerGC(t *testing.T, ctx context.Context, c *containerd.Client) {
	t.Helper()
	ls := c.LeasesService()
	for i := 0; i < 16; i++ {
		l, err := ls.Create(ctx, leases.WithID(fmt.Sprintf("gc-tick-%d-%d", time.Now().UnixNano(), i)))
		if err != nil {
			continue
		}
		_ = ls.Delete(ctx, l)
	}
	time.Sleep(500 * time.Millisecond)
}

// waitForCondition polls until cond is true or the timeout elapses.
func waitForCondition(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s did not become true within %v", what, timeout)
}

// eventuallyTrue polls until cond returns true or the timeout elapses.
func eventuallyTrue(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return cond()
}

// dumpCacheDir lists the contents of the cache namespace directory for
// debugging failed assertions.
func dumpCacheDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Logf("[cache dir] ReadDir %s: %v", dir, err)
		return
	}
	for _, e := range entries {
		t.Logf("[cache dir] %s  (isDir=%v)", filepath.Join(dir, e.Name()), e.IsDir())
		if e.IsDir() {
			sub, _ := os.ReadDir(filepath.Join(dir, e.Name()))
			for _, f := range sub {
				t.Logf("[cache dir]   %s", f.Name())
			}
		}
	}
}
