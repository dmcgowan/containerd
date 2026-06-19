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

package erofs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"

	goerofs "github.com/erofs/go-erofs"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/containerd/containerd/v2/core/content"
	contentindex "github.com/containerd/containerd/v2/core/content/index"
	"github.com/containerd/containerd/v2/core/content/index/chunked"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/converter"
	"github.com/containerd/containerd/v2/core/images/converter/uncompress"
	"github.com/containerd/containerd/v2/internal/erofsutils"
	"github.com/containerd/containerd/v2/pkg/archive/compression"
	"github.com/containerd/containerd/v2/pkg/labels"
	"github.com/containerd/continuity/tarconv"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/platforms"
)

// MergeManifestFunc returns an UpdateManifestFunc that collapses all layers of
// the *original* manifest into a single merged EROFS layer using
// tarconv.WithMerge().
//
// Each original tar layer is applied sequentially to a single go-erofs Writer
// with overlay-changeset semantics: whiteouts remove entries, opaque
// directories wipe their children, and the final EROFS image represents the
// fully resolved filesystem with no overlay metadata.
//
// The result replaces all layers in the converted manifest with a single
// application/vnd.erofs+zstd layer carrying a chunk index (when above the
// chunking threshold). Pass ConvertOpts to enable optional features such as
// dm-verity (WithDmVerity).
func MergeManifestFunc(opts ...ConvertOpt) converter.UpdateManifestFunc {
	var convertOpts convertOptions
	for _, opt := range opts {
		opt(&convertOpts)
	}
	return func(ctx context.Context, cs content.Store, originalDesc, convertedDesc ocispec.Descriptor) (*ocispec.Descriptor, error) {
		if !images.IsManifestType(convertedDesc.MediaType) {
			return nil, nil
		}

		// Read the original (tar-based) manifest to get the source layers.
		var origMfst ocispec.Manifest
		if _, err := converter.ReadJSON(ctx, cs, &origMfst, originalDesc); err != nil {
			return nil, fmt.Errorf("merge: read original manifest: %w", err)
		}

		// Read the converted manifest for config and GC label management.
		var convMfst ocispec.Manifest
		manifestLabels, err := converter.ReadJSON(ctx, cs, &convMfst, convertedDesc)
		if err != nil {
			return nil, fmt.Errorf("merge: read converted manifest: %w", err)
		}
		if manifestLabels == nil {
			manifestLabels = make(map[string]string)
		}

		log.G(ctx).Infof("merge: applying %d layers into single EROFS image", len(origMfst.Layers))

		// Build the merged EROFS image by replaying all original tar layers.
		erofsFile, err := os.CreateTemp("", "merge-erofs-*.img")
		if err != nil {
			return nil, fmt.Errorf("merge: create temp: %w", err)
		}
		defer os.Remove(erofsFile.Name())
		defer erofsFile.Close() // no-op if already closed; covers all early-return paths

		w := goerofs.Create(erofsFile)

		for i, layer := range origMfst.Layers {
			if layer.Size == 0 {
				continue
			}
			if !images.IsLayerType(layer.MediaType) {
				continue
			}

			ra, err := cs.ReaderAt(ctx, layer)
			if err != nil {
				erofsFile.Close()
				return nil, fmt.Errorf("merge: open layer %d: %w", i, err)
			}

			var tarReader io.Reader = content.NewReader(ra)
			if !uncompress.IsUncompressedType(layer.MediaType) {
				dec, err := compression.DecompressStream(tarReader)
				if err != nil {
					ra.Close()
					erofsFile.Close()
					return nil, fmt.Errorf("merge: decompress layer %d: %w", i, err)
				}
				tarReader = dec
			}

			if err := tarconv.Apply(w, tarReader, tarconv.WithMerge()); err != nil {
				ra.Close()
				erofsFile.Close()
				return nil, fmt.Errorf("merge: apply layer %d: %w", i, err)
			}
			ra.Close()
			log.G(ctx).Debugf("merge: applied layer %d/%d", i+1, len(origMfst.Layers))
		}

		if err := w.Close(); err != nil {
			return nil, fmt.Errorf("merge: finalize EROFS: %w", err)
		}

		erofsSize, err := erofsFile.Seek(0, io.SeekEnd)
		if err != nil {
			return nil, fmt.Errorf("merge: seek end: %w", err)
		}
		log.G(ctx).Infof("merge: raw EROFS %.1f MiB", float64(erofsSize)/1024/1024)

		// For dm-verity we need the full image in memory to compute and append
		// the Merkle tree (AppendDmVerity currently takes []byte).  For the
		// common non-dm-verity path we stream directly from the temp file to
		// avoid holding the entire merged EROFS in RAM.
		var verity *erofsutils.DmVerityResult
		var erofsRA io.ReaderAt = erofsFile // streaming source; may be replaced below
		var forcedBoundaries []int64
		var diffID digest.Digest

		if convertOpts.dmVerity {
			if _, err := erofsFile.Seek(0, io.SeekStart); err != nil {
				return nil, fmt.Errorf("merge: seek for dmverity: %w", err)
			}
			erofsBytes, err := io.ReadAll(erofsFile)
			if err != nil {
				return nil, fmt.Errorf("merge: read EROFS for dmverity: %w", err)
			}
			combined, v, err := erofsutils.AppendDmVerity(ctx, erofsBytes, 0)
			if err != nil {
				return nil, fmt.Errorf("merge: dmverity: %w", err)
			}
			verity = v
			erofsSize = int64(len(combined))
			diffID = digest.FromBytes(combined)
			forcedBoundaries = []int64{verity.HashOffset}
			// Switch to an in-memory reader for the combined image+tree.
			erofsRA = bytes.NewReader(combined)
		} else {
			// Non-dm-verity: compute diffID by streaming the EROFS file.
			if _, err := erofsFile.Seek(0, io.SeekStart); err != nil {
				return nil, fmt.Errorf("merge: seek for diffID: %w", err)
			}
			h := digest.SHA256.Digester()
			if _, err := io.Copy(h.Hash(), erofsFile); err != nil {
				return nil, fmt.Errorf("merge: hash for diffID: %w", err)
			}
			diffID = h.Digest()
			if _, err := erofsFile.Seek(0, io.SeekStart); err != nil {
				return nil, fmt.Errorf("merge: seek after diffID: %w", err)
			}
		}

		labelz := map[string]string{
			labels.LabelUncompressed: diffID.String(),
		}

		// Ingest and chunk the merged blob.
		ref := fmt.Sprintf("convert-erofs-merge-%s", originalDesc.Digest)
		cw, err := content.OpenWriter(ctx, cs, content.WithRef(ref))
		if err != nil {
			return nil, fmt.Errorf("merge: open writer: %w", err)
		}
		defer cw.Close()
		if err := cw.Truncate(0); err != nil {
			return nil, fmt.Errorf("merge: truncate: %w", err)
		}

		var mergedDesc ocispec.Descriptor
		if erofsSize >= chunkingThreshold {
			targetFrame := convertOpts.targetFrameSize
			if targetFrame <= 0 {
				targetFrame = chunked.TargetFrameSize
			}
			result, err := chunked.Build(
				erofsRA,
				erofsSize,
				cw,
				contentindex.MediaTypeEROFSZstd,
				targetFrame,
				forcedBoundaries...,
			)
			if err != nil {
				return nil, fmt.Errorf("merge: chunk: %w", err)
			}
			blobDigest := cw.Digest()
			if err := cw.Commit(ctx, result.Written, blobDigest, content.WithLabels(labelz)); err != nil && !errdefs.IsAlreadyExists(err) {
				return nil, fmt.Errorf("merge: commit: %w", err)
			}
			mergedDesc = result.Descriptor
			mergedDesc.Digest = blobDigest
			if mergedDesc.Annotations == nil {
				mergedDesc.Annotations = make(map[string]string)
			}
			mergedDesc.Annotations[contentindex.AnnotationUncompressedDigest] = diffID.String()
			stampDmVerityAnnotations(mergedDesc.Annotations, verity)
			log.G(ctx).Infof("merge: %d chunks, blob %.1f MiB", len(result.Chunks), float64(result.Written)/1024/1024)
		} else {
			zw, err := compression.CompressStream(cw, compression.Zstd)
			if err != nil {
				if !convertOpts.dmVerity {
					erofsFile.Close()
				}
				return nil, fmt.Errorf("merge: zstd: %w", err)
			}
			if _, err := io.Copy(zw, io.NewSectionReader(erofsRA, 0, erofsSize)); err != nil {
				zw.Close()
			return nil, fmt.Errorf("merge: compress: %w", err)
		}
		if err := zw.Close(); err != nil {
				return nil, fmt.Errorf("merge: close zstd: %w", err)
			}
			if err := cw.Commit(ctx, 0, cw.Digest(), content.WithLabels(labelz)); err != nil && !errdefs.IsAlreadyExists(err) {
				return nil, fmt.Errorf("merge: commit single: %w", err)
			}
			cInfo, err := cs.Info(ctx, cw.Digest())
			if err != nil {
				return nil, fmt.Errorf("merge: info: %w", err)
			}
			anns := map[string]string{
				contentindex.AnnotationUncompressedDigest: diffID.String(),
			}
			stampDmVerityAnnotations(anns, verity)
			mergedDesc = ocispec.Descriptor{
				MediaType:   images.MediaTypeErofsZstd,
				Digest:      cw.Digest(),
				Size:        cInfo.Size,
				Annotations: anns,
			}
		}

		// Replace all layers in the converted manifest with the single merged layer.
		for k := range manifestLabels {
			if len(k) > 30 && k[:30] == "containerd.io/gc.ref.content.l" {
				delete(manifestLabels, k)
			}
		}
		manifestLabels["containerd.io/gc.ref.content.l.0"] = mergedDesc.Digest.String()
		convMfst.Layers = []ocispec.Descriptor{mergedDesc}

		// Update the config: single DiffID + os.features=["erofs"].
		var cfg converter.DualConfig
		configLabels, err := converter.ReadJSON(ctx, cs, &cfg, convMfst.Config)
		if err != nil {
			return nil, fmt.Errorf("merge: read config: %w", err)
		}
		newRootFS := ocispec.RootFS{Type: "layers", DiffIDs: []digest.Digest{diffID}}
		rootfsB, _ := json.Marshal(newRootFS)
		cfg["rootfs"] = (*json.RawMessage)(&rootfsB)

		var cfgAsOCI ocispec.Image
		if _, err := converter.ReadJSON(ctx, cs, &cfgAsOCI, convMfst.Config); err == nil {
			normalized := platforms.Normalize(cfgAsOCI.Platform)
			if !slices.Contains(normalized.OSFeatures, "erofs") {
				normalized.OSFeatures = append(normalized.OSFeatures, "erofs")
			}
			b, _ := json.Marshal(normalized.OSFeatures)
			cfg["os.features"] = (*json.RawMessage)(&b)
		}
		newConfig, err := converter.WriteJSON(ctx, cs, &cfg, convMfst.Config, configLabels)
		if err != nil {
			return nil, fmt.Errorf("merge: write config: %w", err)
		}
		manifestLabels["containerd.io/gc.ref.content.config"] = newConfig.Digest.String()
		convMfst.Config = *newConfig

		newManifestDesc, err := converter.WriteJSON(ctx, cs, &convMfst, convertedDesc, manifestLabels)
		if err != nil {
			return nil, fmt.Errorf("merge: write manifest: %w", err)
		}
		if originalDesc.Platform != nil {
			p := *originalDesc.Platform
			if !slices.Contains(p.OSFeatures, "erofs") {
				p.OSFeatures = append(p.OSFeatures, "erofs")
			}
			newManifestDesc.Platform = &p
		}
		return newManifestDesc, nil
	}
}
