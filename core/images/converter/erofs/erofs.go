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

// chunkingThreshold is the minimum raw EROFS image size (in bytes) above
// which the +zstd path splits the blob into per-chunk zstd frames and appends
// a chunk index. Below this threshold the whole EROFS image is written as a
// single zstd frame, avoiding chunk-index overhead for very small images.
//
// Note: this threshold applies to the EROFS filesystem image size after
// conversion but before compression, which is typically larger than the source
// tar due to EROFS block-alignment and inode metadata.
const chunkingThreshold = 20 * 1024 * 1024 // 20 MiB

type ConvertOpt func(*convertOptions)

type convertOptions struct {
	blobCompression string
	dmVerity        bool
}

func WithBlobCompression(c string) ConvertOpt {
	return func(opts *convertOptions) {
		opts.blobCompression = c
	}
}

// WithDmVerity enables dm-verity merkle-tree generation. The merkle tree is
// appended directly after the EROFS filesystem image bytes (single-file
// dm-verity layout), and the org.erofs.dmverity.* annotations are set on
// the output descriptor. The tree occupies its own chunk in the +zstd blob
// so lazy-loading fetches do not mix filesystem data with integrity metadata.
func WithDmVerity() ConvertOpt {
	return func(opts *convertOptions) {
		opts.dmVerity = true
	}
}

// WithCompressors is kept for API compatibility but is a no-op in the
// tarconv-based implementation; EROFS-internal compression is set via
// go-erofs CreateOpt when building the writer.
func WithCompressors(_ string) ConvertOpt {
	return func(_ *convertOptions) {}
}

// WithMkfsOptions is kept for API compatibility but is a no-op in the
// tarconv-based implementation that uses the go-erofs writer directly.
func WithMkfsOptions(_ []string) ConvertOpt {
	return func(_ *convertOptions) {}
}

// LayerConvertFunc converts an OCI tar layer into an EROFS layer using the
// continuity tarconv package, which calls the go-erofs writer directly
// without invoking mkfs.erofs as an external process.
//
// The resulting media type is application/vnd.erofs (raw) by default.
// Pass WithBlobCompression("zstd") to produce application/vnd.erofs+zstd.
func LayerConvertFunc(opts ...ConvertOpt) converter.ConvertFunc {
	return func(ctx context.Context, cs content.Store, desc ocispec.Descriptor) (*ocispec.Descriptor, error) {
		var convertOpts convertOptions
		for _, opt := range opts {
			opt(&convertOpts)
		}

		if !images.IsLayerType(desc.MediaType) || erofsutils.IsErofsMediaType(desc.MediaType) {
			return nil, nil
		}
		if images.IsNonDistributable(desc.MediaType) {
			return nil, nil
		}

		// Obtain the uncompressed tar stream.
		uncompressedDesc := &desc
		if !uncompress.IsUncompressedType(desc.MediaType) {
			var err error
			uncompressedDesc, err = uncompress.LayerConvertFunc(ctx, cs, desc)
			if err != nil {
				return nil, err
			}
			if uncompressedDesc == nil {
				return nil, fmt.Errorf("unexpectedly got the same blob after decompression (%s, %q)", desc.Digest, desc.MediaType)
			}
			log.G(ctx).Debugf("uncompressed %s into %s", desc.Digest, uncompressedDesc.Digest)
		}

		info, err := cs.Info(ctx, desc.Digest)
		if err != nil {
			return nil, fmt.Errorf("failed to get content info: %w", err)
		}
		labelz := info.Labels
		if labelz == nil {
			labelz = make(map[string]string)
		}

		ra, err := cs.ReaderAt(ctx, *uncompressedDesc)
		if err != nil {
			return nil, fmt.Errorf("failed to get reader: %w", err)
		}
		defer ra.Close()

		tarReader := io.NewSectionReader(ra, 0, uncompressedDesc.Size)

		// Build the EROFS image into a temp file using the go-erofs writer.
		// goerofs.Create requires an io.WriteSeeker; use a temp file for that.
		erofsFile, err := os.CreateTemp("", "layer-*.erofs")
		if err != nil {
			return nil, fmt.Errorf("failed to create temp file for EROFS: %w", err)
		}
		erofsPath := erofsFile.Name()
		defer func() {
			if err := os.Remove(erofsPath); err != nil && !os.IsNotExist(err) {
				log.G(ctx).WithError(err).Warnf("failed to remove temp file %s", erofsPath)
			}
		}()

		w := goerofs.Create(erofsFile)
		if err := tarconv.Apply(w, tarReader); err != nil {
			erofsFile.Close()
			return nil, fmt.Errorf("tarconv: build EROFS from tar: %w", err)
		}
		if err := w.Close(); err != nil {
			erofsFile.Close()
			return nil, fmt.Errorf("tarconv: finalize EROFS image: %w", err)
		}
		// Seek back to the start for reading.
		if _, err := erofsFile.Seek(0, io.SeekStart); err != nil {
			erofsFile.Close()
			return nil, fmt.Errorf("failed to seek EROFS file: %w", err)
		}
		fi, err := erofsFile.Stat()
		if err != nil {
			erofsFile.Close()
			return nil, fmt.Errorf("failed to stat EROFS file: %w", err)
		}
		log.G(ctx).Debugf("built EROFS image for %s (%d bytes)", desc.Digest, fi.Size())

		if convertOpts.blobCompression == "zstd" {
			erofsBytes, err := io.ReadAll(erofsFile)
			erofsFile.Close()
			if err != nil {
				return nil, fmt.Errorf("failed to read EROFS image: %w", err)
			}

			// Optionally append a dm-verity merkle tree. The tree is appended
			// directly after the EROFS filesystem image bytes (single-file
			// dm-verity layout). hash_offset = original EROFS image size.
			var verity *erofsutils.DmVerityResult
			if convertOpts.dmVerity {
				combined, v, err := erofsutils.AppendDmVerity(ctx, erofsBytes, 0)
				if err != nil {
					return nil, fmt.Errorf("dmverity: %w", err)
				}
				erofsBytes = combined
				verity = v
			}

			// DiffID is the SHA-256 of the (possibly extended) uncompressed bytes.
			diffID := digest.FromBytes(erofsBytes)
			labelz[labels.LabelUncompressed] = diffID.String()

			ref := fmt.Sprintf("convert-erofs-from-%s", desc.Digest)
			cw, err := content.OpenWriter(ctx, cs, content.WithRef(ref))
			if err != nil {
				return nil, fmt.Errorf("failed to open content writer: %w", err)
			}
			defer cw.Close()
			if err := cw.Truncate(0); err != nil {
				return nil, fmt.Errorf("failed to truncate writer: %w", err)
			}

			var newDesc ocispec.Descriptor

			// Forced chunk boundary: when dm-verity is present, the merkle tree
			// must start on its own chunk (erofs-image-spec §3.5).
			var forcedBoundaries []int64
			if verity != nil {
				forcedBoundaries = []int64{verity.HashOffset}
			}

			if int64(len(erofsBytes)) >= chunkingThreshold {
				// Large image: split into per-chunk zstd frames and append a chunk
				// index (erofs-image-spec §3.4) for lazy loading and per-chunk
				// content addressing.
				result, err := chunked.Build(
					bytes.NewReader(erofsBytes),
					int64(len(erofsBytes)),
					contentindex.MediaTypeEROFSZstd,
					chunked.DefaultChunkSize,
					forcedBoundaries...,
				)
				if err != nil {
					return nil, fmt.Errorf("failed to build chunked EROFS blob: %w", err)
				}
				if _, err := cw.Write(result.Blob); err != nil {
					return nil, fmt.Errorf("failed to write chunked blob: %w", err)
				}
				if err := cw.Commit(ctx, int64(len(result.Blob)), result.Descriptor.Digest, content.WithLabels(labelz)); err != nil && !errdefs.IsAlreadyExists(err) {
					return nil, fmt.Errorf("failed to commit chunked blob: %w", err)
				}
				newDesc = result.Descriptor
				if newDesc.Annotations == nil {
					newDesc.Annotations = make(map[string]string)
				}
				newDesc.Annotations[contentindex.AnnotationUncompressedDigest] = diffID.String()
				if verity != nil {
					newDesc.Annotations[contentindex.AnnotationDmVerityHashOffset] = fmt.Sprintf("%d", verity.HashOffset)
					newDesc.Annotations[contentindex.AnnotationDmVerityRootDigest] = verity.RootDigest
					if verity.BlockSize != contentindex.DefaultDmVerityBlockSize {
						newDesc.Annotations[contentindex.AnnotationDmVerityBlockSize] = fmt.Sprintf("%d", verity.BlockSize)
					}
				}
				log.G(ctx).Debugf("converted %s to EROFS+zstd with chunk index (%d chunks)", desc.Digest, len(result.Chunks))
			} else {
				// Small image: compress as a single zstd frame. The chunk-index
				// overhead is not worthwhile for images under the threshold.
				zw, err := compression.CompressStream(cw, compression.Zstd)
				if err != nil {
					return nil, fmt.Errorf("failed to create zstd compressor: %w", err)
				}
				if _, err := io.Copy(zw, bytes.NewReader(erofsBytes)); err != nil {
					zw.Close()
					return nil, fmt.Errorf("failed to compress EROFS blob: %w", err)
				}
				if err := zw.Close(); err != nil {
					return nil, fmt.Errorf("failed to finalize zstd stream: %w", err)
				}
				if err := cw.Commit(ctx, 0, cw.Digest(), content.WithLabels(labelz)); err != nil && !errdefs.IsAlreadyExists(err) {
					return nil, fmt.Errorf("failed to commit: %w", err)
				}
				cInfo, err := cs.Info(ctx, cw.Digest())
				if err != nil {
					return nil, fmt.Errorf("failed to get content info: %w", err)
				}
				newDesc = desc
				newDesc.MediaType = images.MediaTypeErofsZstd
				newDesc.Digest = cw.Digest()
				newDesc.Size = cInfo.Size
				newDesc.Annotations = map[string]string{
					contentindex.AnnotationUncompressedDigest: diffID.String(),
				}
				if verity != nil {
					newDesc.Annotations[contentindex.AnnotationDmVerityHashOffset] = fmt.Sprintf("%d", verity.HashOffset)
					newDesc.Annotations[contentindex.AnnotationDmVerityRootDigest] = verity.RootDigest
					if verity.BlockSize != contentindex.DefaultDmVerityBlockSize {
						newDesc.Annotations[contentindex.AnnotationDmVerityBlockSize] = fmt.Sprintf("%d", verity.BlockSize)
					}
				}
				log.G(ctx).Debugf("converted %s to EROFS+zstd (single frame, below %d MiB threshold)", desc.Digest, chunkingThreshold/1024/1024)
			}
			return &newDesc, nil
		}

		// Raw (uncompressed) EROFS: ingest the raw image bytes directly.
		ref := fmt.Sprintf("convert-erofs-from-%s", desc.Digest)
		cw, err := content.OpenWriter(ctx, cs, content.WithRef(ref))
		if err != nil {
			erofsFile.Close()
			return nil, fmt.Errorf("failed to open content writer: %w", err)
		}
		defer cw.Close()
		if err := cw.Truncate(0); err != nil {
			erofsFile.Close()
			return nil, fmt.Errorf("failed to truncate writer: %w", err)
		}
		if _, err := io.Copy(cw, erofsFile); err != nil {
			erofsFile.Close()
			return nil, fmt.Errorf("failed to copy EROFS blob: %w", err)
		}
		erofsFile.Close()
		labelz[labels.LabelUncompressed] = cw.Digest().String()
		if err := cw.Commit(ctx, 0, cw.Digest(), content.WithLabels(labelz)); err != nil && !errdefs.IsAlreadyExists(err) {
			return nil, fmt.Errorf("failed to commit: %w", err)
		}

		cInfo, err := cs.Info(ctx, cw.Digest())
		if err != nil {
			return nil, fmt.Errorf("failed to get content info: %w", err)
		}
		newDesc := desc
		newDesc.MediaType = images.MediaTypeErofs
		newDesc.Digest = cw.Digest()
		newDesc.Size = cInfo.Size
		return &newDesc, nil
	}
}

func UpdateManifestPlatform(ctx context.Context, cs content.Store, originalDesc, convertedDesc ocispec.Descriptor) (*ocispec.Descriptor, error) {
	if !images.IsManifestType(convertedDesc.MediaType) {
		return nil, nil
	}

	var manifest ocispec.Manifest
	manifestLabels, err := converter.ReadJSON(ctx, cs, &manifest, convertedDesc)
	if err != nil {
		return nil, err
	}

	var platform ocispec.Platform
	if originalDesc.Platform != nil {
		platform = *originalDesc.Platform
	} else {
		configPlatform, err := images.ConfigPlatform(ctx, cs, manifest.Config)
		if err != nil {
			return nil, err
		}
		platform = configPlatform
	}

	normalized := platforms.Normalize(platform)
	if !slices.Contains(normalized.OSFeatures, "erofs") {
		normalized.OSFeatures = append(normalized.OSFeatures, "erofs")
		normalized = platforms.Normalize(normalized)
	}

	var cfg converter.DualConfig
	configLabels, err := converter.ReadJSON(ctx, cs, &cfg, manifest.Config)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(normalized.OSFeatures)
	if err != nil {
		return nil, err
	}
	cfg["os.features"] = (*json.RawMessage)(&b)
	newConfig, err := converter.WriteJSON(ctx, cs, &cfg, manifest.Config, configLabels)
	if err != nil {
		return nil, err
	}

	if manifestLabels == nil {
		manifestLabels = make(map[string]string)
	}
	converter.ClearGCLabels(manifestLabels, manifest.Config.Digest)
	manifestLabels["containerd.io/gc.ref.content.config"] = newConfig.Digest.String()
	manifest.Config = *newConfig

	newManifestDesc, err := converter.WriteJSON(ctx, cs, &manifest, convertedDesc, manifestLabels)
	if err != nil {
		return nil, err
	}
	newManifestDesc.Platform = &normalized
	return newManifestDesc, nil
}
