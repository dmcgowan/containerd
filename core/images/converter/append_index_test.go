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

package converter_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/converter"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	localcs "github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/platforms"
	"github.com/opencontainers/go-digest"
	ocispecs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// ── test ConvertFuncs ─────────────────────────────────────────────────────────

// doubleLayerConvertFunc replaces a layer with twice its bytes — a
// recognisable, invertible transformation that lets tests detect real
// conversion without requiring mkfs.erofs or zstd.
func doubleLayerConvertFunc(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
	if !images.IsLayerType(desc.MediaType) {
		return nil, nil
	}
	orig, err := content.ReadBlob(ctx, cs, desc)
	if err != nil {
		return nil, err
	}
	doubled := append(orig, orig...)
	dgst := digest.FromBytes(doubled)
	if dgst == desc.Digest {
		return nil, nil
	}
	newDesc := desc
	newDesc.Digest = dgst
	newDesc.Size = int64(len(doubled))
	cw, err := cs.Writer(ctx,
		content.WithRef("test-double-"+dgst.String()),
		content.WithDescriptor(newDesc),
	)
	if err != nil {
		return nil, err
	}
	cw.Write(doubled)
	if err := cw.Commit(ctx, int64(len(doubled)), dgst); err != nil {
		cw.Close()
		return nil, err
	}
	return &newDesc, nil
}

// noopLayerConvertFunc returns nil (no change) for all layers.
func noopLayerConvertFunc(_ context.Context, _ content.Store, _ ocispec.Descriptor) (*ocispec.Descriptor, error) {
	return nil, nil
}

// markErofsUpdateManifest is a minimal UpdateManifestFunc that adds
// os.features=["erofs"] to the platform descriptor.
func markErofsUpdateManifest(_ context.Context, _ content.Store, _, convertedDesc ocispec.Descriptor) (*ocispec.Descriptor, error) {
	d := convertedDesc
	if d.Platform == nil {
		d.Platform = &ocispec.Platform{}
	}
	d.Platform.OSFeatures = appendUniq(d.Platform.OSFeatures, "erofs")
	return &d, nil
}

func appendUniq(ss []string, s string) []string {
	for _, v := range ss {
		if v == s {
			return ss
		}
	}
	return append(ss, s)
}

// ── content-store helpers ─────────────────────────────────────────────────────

func writeTestBlob(t *testing.T, ctx context.Context, cs content.Store, data []byte, mediaType string) ocispec.Descriptor {
	t.Helper()
	dgst := digest.FromBytes(data)
	desc := ocispec.Descriptor{MediaType: mediaType, Digest: dgst, Size: int64(len(data))}
	if _, err := cs.Info(ctx, dgst); err == nil {
		return desc
	}
	cw, err := cs.Writer(ctx, content.WithRef("tw-"+dgst.String()), content.WithDescriptor(desc))
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	cw.Write(data)
	if err := cw.Commit(ctx, int64(len(data)), dgst); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return desc
}

func readIndex(t *testing.T, ctx context.Context, cs content.Store, desc ocispec.Descriptor) ocispec.Index {
	t.Helper()
	b, err := content.ReadBlob(ctx, cs, desc)
	if err != nil {
		t.Fatalf("read index %s: %v", desc.Digest, err)
	}
	var idx ocispec.Index
	if err := json.Unmarshal(b, &idx); err != nil {
		t.Fatalf("unmarshal index: %v", err)
	}
	return idx
}

// buildSingleManifest creates a minimal manifest (with a layer and a config
// that embeds the given platform) in cs and returns its descriptor.
func buildSingleManifest(t *testing.T, ctx context.Context, cs content.Store, platform ocispec.Platform) ocispec.Descriptor {
	t.Helper()

	layerData := []byte("layer-for-" + platform.OS + "-" + platform.Architecture)
	layerDesc := writeTestBlob(t, ctx, cs, layerData, ocispec.MediaTypeImageLayer)

	cfg := ocispec.Image{
		Platform: platform,
		RootFS:   ocispec.RootFS{Type: "layers", DiffIDs: []digest.Digest{digest.FromBytes(layerData)}},
	}
	cfgBytes, _ := json.Marshal(cfg)
	cfgDesc := writeTestBlob(t, ctx, cs, cfgBytes, ocispec.MediaTypeImageConfig)

	mfst := ocispec.Manifest{
		Versioned: ocispecs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    cfgDesc,
		Layers:    []ocispec.Descriptor{layerDesc},
	}
	mfstBytes, _ := json.Marshal(mfst)
	dgst := digest.FromBytes(mfstBytes)
	desc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    dgst,
		Size:      int64(len(mfstBytes)),
		Platform:  &platform,
	}
	if _, err := cs.Info(ctx, dgst); err != nil {
		cw, err := cs.Writer(ctx, content.WithRef("mfst-"+dgst.String()), content.WithDescriptor(desc))
		if err != nil {
			t.Fatalf("open manifest writer: %v", err)
		}
		cw.Write(mfstBytes)
		if err := cw.Commit(ctx, int64(len(mfstBytes)), dgst); err != nil {
			t.Fatalf("commit manifest: %v", err)
		}
	}
	return desc
}

func buildIndex(t *testing.T, ctx context.Context, cs content.Store, manifests ...ocispec.Descriptor) ocispec.Descriptor {
	t.Helper()
	idx := ocispec.Index{
		Versioned: ocispecs.Versioned{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: manifests,
	}
	b, _ := json.Marshal(idx)
	dgst := digest.FromBytes(b)
	desc := ocispec.Descriptor{MediaType: ocispec.MediaTypeImageIndex, Digest: dgst, Size: int64(len(b))}
	if _, err := cs.Info(ctx, dgst); err != nil {
		cw, err := cs.Writer(ctx, content.WithRef("idx-"+dgst.String()), content.WithDescriptor(desc))
		if err != nil {
			t.Fatalf("open index writer: %v", err)
		}
		cw.Write(b)
		if err := cw.Commit(ctx, int64(len(b)), dgst); err != nil {
			t.Fatalf("commit index: %v", err)
		}
	}
	return desc
}

// ── Tests ─────────────────────────────────────────────────────────────────────

// TestAppendIndexConvertFunc_PromotesSingleManifest verifies that a single
// manifest source is wrapped in a new OCI image index with the original listed
// first and the converted variant listed second with os.features=["erofs"].
func TestAppendIndexConvertFunc_PromotesSingleManifest(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")

	cs, err := localcs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	origDesc := buildSingleManifest(t, ctx, cs, ocispec.Platform{OS: "linux", Architecture: "amd64"})

	fn := converter.AppendIndexConvertFunc(
		doubleLayerConvertFunc,
		false,
		platforms.All,
		markErofsUpdateManifest,
	)
	resultDesc, err := fn(ctx, cs, origDesc)
	if err != nil {
		t.Fatalf("AppendIndexConvertFunc: %v", err)
	}
	if resultDesc == nil {
		t.Fatal("expected a new descriptor, got nil")
	}
	if resultDesc.MediaType != ocispec.MediaTypeImageIndex {
		t.Fatalf("result media type: got %q want OCI image index", resultDesc.MediaType)
	}

	idx := readIndex(t, ctx, cs, *resultDesc)
	if got := len(idx.Manifests); got != 2 {
		t.Fatalf("want 2 manifests, got %d", got)
	}
	// [0] = original, [1] = converted.
	if idx.Manifests[0].Digest != origDesc.Digest {
		t.Errorf("manifests[0] should be the original %s, got %s", origDesc.Digest, idx.Manifests[0].Digest)
	}
	if idx.Manifests[1].Digest == origDesc.Digest {
		t.Error("manifests[1] should differ from the original")
	}
	if !hasOSFeature(idx.Manifests[1], "erofs") {
		t.Errorf("manifests[1] should have os.features=[erofs], platform: %+v", idx.Manifests[1].Platform)
	}
	t.Logf("single manifest → index: [%s (orig), %s (erofs)] ✓",
		idx.Manifests[0].Digest, idx.Manifests[1].Digest)
}

// TestAppendIndexConvertFunc_AppendsToExistingIndex verifies that a
// multi-platform index source retains all original manifests first and
// appends converted EROFS variants after them.
func TestAppendIndexConvertFunc_AppendsToExistingIndex(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")

	cs, err := localcs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	amd64 := buildSingleManifest(t, ctx, cs, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	arm64 := buildSingleManifest(t, ctx, cs, ocispec.Platform{OS: "linux", Architecture: "arm64"})
	origIdxDesc := buildIndex(t, ctx, cs, amd64, arm64)

	fn := converter.AppendIndexConvertFunc(
		doubleLayerConvertFunc,
		false,
		platforms.All,
		markErofsUpdateManifest,
	)
	resultDesc, err := fn(ctx, cs, origIdxDesc)
	if err != nil {
		t.Fatalf("AppendIndexConvertFunc: %v", err)
	}
	if resultDesc == nil {
		t.Fatal("expected a new index")
	}

	idx := readIndex(t, ctx, cs, *resultDesc)
	if got := len(idx.Manifests); got != 4 {
		t.Fatalf("want 4 manifests (2 orig + 2 converted), got %d", got)
	}

	// First two are originals, unchanged.
	if idx.Manifests[0].Digest != amd64.Digest {
		t.Errorf("manifests[0] should be original amd64")
	}
	if idx.Manifests[1].Digest != arm64.Digest {
		t.Errorf("manifests[1] should be original arm64")
	}

	// Last two are converted EROFS variants.
	for i := 2; i < 4; i++ {
		if idx.Manifests[i].Digest == amd64.Digest || idx.Manifests[i].Digest == arm64.Digest {
			t.Errorf("manifests[%d] should be a converted manifest", i)
		}
		if !hasOSFeature(idx.Manifests[i], "erofs") {
			t.Errorf("manifests[%d] missing os.features=erofs; platform=%+v", i, idx.Manifests[i].Platform)
		}
	}
	t.Logf("2 originals + 2 EROFS = 4 manifests in result index ✓")
}

// TestAppendIndexConvertFunc_NoOpWhenUnchanged verifies that when no layer
// is actually converted (noop), nil is returned (no new index created).
func TestAppendIndexConvertFunc_NoOpWhenUnchanged(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")

	cs, err := localcs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	origDesc := buildSingleManifest(t, ctx, cs, ocispec.Platform{OS: "linux", Architecture: "amd64"})

	fn := converter.AppendIndexConvertFunc(noopLayerConvertFunc, false, platforms.All, nil)
	resultDesc, err := fn(ctx, cs, origDesc)
	if err != nil {
		t.Fatalf("AppendIndexConvertFunc: %v", err)
	}
	if resultDesc != nil {
		t.Errorf("expected nil (no conversion), got %s", resultDesc.Digest)
	}
	t.Log("no-op → nil result (no unnecessary index created) ✓")
}

// TestAppendIndexConvertFunc_PlatformFilter verifies that only platforms
// matching the filter are converted; all original manifests are kept.
func TestAppendIndexConvertFunc_PlatformFilter(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")

	cs, err := localcs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	amd64p := ocispec.Platform{OS: "linux", Architecture: "amd64"}
	arm64p := ocispec.Platform{OS: "linux", Architecture: "arm64"}
	amd64Desc := buildSingleManifest(t, ctx, cs, amd64p)
	arm64Desc := buildSingleManifest(t, ctx, cs, arm64p)
	origIdxDesc := buildIndex(t, ctx, cs, amd64Desc, arm64Desc)

	// Convert only amd64.
	fn := converter.AppendIndexConvertFunc(
		doubleLayerConvertFunc,
		false,
		platforms.Only(amd64p),
		markErofsUpdateManifest,
	)
	resultDesc, err := fn(ctx, cs, origIdxDesc)
	if err != nil {
		t.Fatalf("AppendIndexConvertFunc: %v", err)
	}
	if resultDesc == nil {
		t.Fatal("expected new index")
	}

	idx := readIndex(t, ctx, cs, *resultDesc)
	// 2 originals + 1 converted (only amd64) = 3.
	if got := len(idx.Manifests); got != 3 {
		t.Fatalf("want 3 (2 orig + 1 converted amd64), got %d", got)
	}
	// Originals first.
	if idx.Manifests[0].Digest != amd64Desc.Digest {
		t.Error("manifests[0] should be original amd64")
	}
	if idx.Manifests[1].Digest != arm64Desc.Digest {
		t.Error("manifests[1] should be original arm64")
	}
	// Only amd64 was converted.
	if idx.Manifests[2].Platform == nil || idx.Manifests[2].Platform.Architecture != "amd64" {
		t.Errorf("converted entry should be amd64; got %+v", idx.Manifests[2].Platform)
	}
	if !hasOSFeature(idx.Manifests[2], "erofs") {
		t.Errorf("converted amd64 entry missing os.features=erofs")
	}
	t.Logf("platform filter: 2 originals kept, 1 amd64 EROFS appended ✓")
}

// TestWithAppendToIndex_Integration tests the full converter.Convert() pipeline
// with converter.WithAppendToIndex() using a minimal in-process mock client.
func TestWithAppendToIndex_Integration(t *testing.T) {
	ctx := namespaces.WithNamespace(context.Background(), "test")

	cs, err := localcs.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	origDesc := buildSingleManifest(t, ctx, cs, ocispec.Platform{OS: "linux", Architecture: "amd64"})
	imgStore := newMockImageStore(images.Image{Name: "test/img:latest", Target: origDesc})
	client := &mockClient{cs: cs, is: imgStore}

	_, err = converter.Convert(ctx, client, "test/img:dual", "test/img:latest",
		converter.WithLayerConvertFunc(doubleLayerConvertFunc),
		converter.WithUpdateManifest(markErofsUpdateManifest),
		converter.WithAppendToIndex(),
	)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	dstImg, err := imgStore.Get(ctx, "test/img:dual")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if dstImg.Target.MediaType != ocispec.MediaTypeImageIndex {
		t.Fatalf("expected OCI image index, got %q", dstImg.Target.MediaType)
	}

	idx := readIndex(t, ctx, cs, dstImg.Target)
	if got := len(idx.Manifests); got != 2 {
		t.Fatalf("want 2 manifests, got %d", got)
	}
	if idx.Manifests[0].Digest != origDesc.Digest {
		t.Error("manifests[0] should be the original")
	}
	if !hasOSFeature(idx.Manifests[1], "erofs") {
		t.Error("manifests[1] should have os.features=erofs")
	}
	t.Logf("integration: target index %s, 2 manifests [orig, erofs] ✓", dstImg.Target.Digest)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func hasOSFeature(desc ocispec.Descriptor, feature string) bool {
	if desc.Platform == nil {
		return false
	}
	for _, f := range desc.Platform.OSFeatures {
		if f == feature {
			return true
		}
	}
	return false
}

// ── mock client ───────────────────────────────────────────────────────────────

type mockClient struct {
	cs content.Store
	is images.Store
}

func (c *mockClient) WithLease(ctx context.Context, _ ...leases.Opt) (context.Context, func(context.Context) error, error) {
	return ctx, func(context.Context) error { return nil }, nil
}
func (c *mockClient) ContentStore() content.Store { return c.cs }
func (c *mockClient) ImageService() images.Store  { return c.is }

// compile-time check
var _ converter.Client = (*mockClient)(nil)

// ── mock image store ──────────────────────────────────────────────────────────

type mockImageStore struct {
	imgs map[string]images.Image
}

func newMockImageStore(seed ...images.Image) *mockImageStore {
	m := &mockImageStore{imgs: make(map[string]images.Image)}
	for _, img := range seed {
		m.imgs[img.Name] = img
	}
	return m
}

func (s *mockImageStore) Get(_ context.Context, name string) (images.Image, error) {
	img, ok := s.imgs[name]
	if !ok {
		return images.Image{}, fmt.Errorf("image %q not found", name)
	}
	return img, nil
}
func (s *mockImageStore) List(_ context.Context, _ ...string) ([]images.Image, error) {
	var l []images.Image
	for _, img := range s.imgs {
		l = append(l, img)
	}
	return l, nil
}
func (s *mockImageStore) Create(_ context.Context, img images.Image) (images.Image, error) {
	s.imgs[img.Name] = img
	return img, nil
}
func (s *mockImageStore) Update(_ context.Context, img images.Image, _ ...string) (images.Image, error) {
	s.imgs[img.Name] = img
	return img, nil
}
func (s *mockImageStore) Delete(_ context.Context, name string, _ ...images.DeleteOpt) error {
	delete(s.imgs, name)
	return nil
}

var _ images.Store = (*mockImageStore)(nil)
