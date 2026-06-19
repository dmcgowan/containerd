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

	// targetFrameSize, when > 0, overrides chunked.TargetFrameSize for any
	// converter entry point that produces chunked output (LayerConvertFunc,
	// LayerConvertFuncChunked, MergeManifestFunc).  Lets callers drive the
	// chunk-granularity knob through the same ConvertOpt list as the other
	// per-conversion settings.  Zero = use chunked.TargetFrameSize default.
	targetFrameSize int
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

// WithTargetFrameSize sets the target *compressed* zstd frame size in bytes
// for chunked output.  Applies to LayerConvertFunc (the auto-chunk +zstd
// path), LayerConvertFuncChunked, and MergeManifestFunc.  Pass 0 or omit to
// use chunked.TargetFrameSize (4.5 MiB).  The actual uncompressed input per
// chunk is roughly 3× this value (zstd compresses EROFS data well).
//
// For LayerConvertFuncChunked, the explicit targetFrameSize argument takes
// precedence over WithTargetFrameSize when both are set; this preserves
// existing call sites.
func WithTargetFrameSize(n int) ConvertOpt {
	return func(opts *convertOptions) {
		opts.targetFrameSize = n
	}
}

// LayerConvertFunc converts an OCI tar layer into an EROFS layer using the
// continuity tarconv package, which calls the go-erofs writer directly
// using the pure-Go go-erofs + continuity/tarconv stack.
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

		// Decompress the source layer on-the-fly — no intermediate
		// uncompressed blob is written to the content store.
		ra, err := cs.ReaderAt(ctx, desc)
		if err != nil {
			return nil, fmt.Errorf("failed to get reader: %w", err)
		}
		defer ra.Close()

		var tarReader io.Reader
		if uncompress.IsUncompressedType(desc.MediaType) {
			tarReader = io.NewSectionReader(ra, 0, ra.Size())
		} else {
			decomp, err := compression.DecompressStream(io.NewSectionReader(ra, 0, ra.Size()))
			if err != nil {
				return nil, fmt.Errorf("failed to decompress stream: %w", err)
			}
			defer decomp.Close()
			tarReader = decomp
		}

		info, err := cs.Info(ctx, desc.Digest)
		if err != nil {
			return nil, fmt.Errorf("failed to get content info: %w", err)
		}
		labelz := info.Labels
		if labelz == nil {
			labelz = make(map[string]string)
		}

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
			erofsSize := fi.Size()

			// Resolve target compressed-frame size for the chunked
			// branch.  WithTargetFrameSize overrides; otherwise fall
			// back to chunked.DefaultChunkSize so existing behaviour
			// is preserved when the caller didn't set it.
			effectiveFrameSize := convertOpts.targetFrameSize
			if effectiveFrameSize <= 0 {
				effectiveFrameSize = chunked.DefaultChunkSize
			}

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

			var newDesc ocispec.Descriptor

			if convertOpts.dmVerity {
				// dm-verity requires the full image in memory to append the
				// merkle tree before compression.  This is intentionally an
				// in-memory path; dm-verity is a rare option.
				erofsBytes, err := io.ReadAll(erofsFile)
				erofsFile.Close()
				if err != nil {
					return nil, fmt.Errorf("failed to read EROFS image for dm-verity: %w", err)
				}
				combined, verity, err := erofsutils.AppendDmVerity(ctx, erofsBytes, 0)
				if err != nil {
					return nil, fmt.Errorf("dmverity: %w", err)
				}
				diffID := digest.FromBytes(combined)
				labelz[labels.LabelUncompressed] = diffID.String()

				var forcedBoundaries []int64
				if verity != nil {
					forcedBoundaries = []int64{verity.HashOffset}
				}

				combinedRA := bytes.NewReader(combined)
				if int64(len(combined)) >= chunkingThreshold {
					result, err := chunked.Build(combinedRA, int64(len(combined)), cw,
						contentindex.MediaTypeEROFSZstd, effectiveFrameSize,
						forcedBoundaries...)
					if err != nil {
						return nil, fmt.Errorf("failed to build chunked EROFS blob: %w", err)
					}
					blobDigest := cw.Digest()
					if cerr := cw.Commit(ctx, result.Written, blobDigest, content.WithLabels(labelz)); cerr != nil && !errdefs.IsAlreadyExists(cerr) {
						return nil, fmt.Errorf("failed to commit chunked blob: %w", cerr)
					}
					newDesc = result.Descriptor
					newDesc.Digest = blobDigest
					if newDesc.Annotations == nil {
						newDesc.Annotations = make(map[string]string)
					}
					newDesc.Annotations[contentindex.AnnotationUncompressedDigest] = diffID.String()
					stampDmVerityAnnotations(newDesc.Annotations, verity)
					log.G(ctx).Debugf("converted %s to EROFS+zstd+dmverity with chunk index (%d chunks)", desc.Digest, len(result.Chunks))
				} else {
					zw, err := compression.CompressStream(cw, compression.Zstd)
					if err != nil {
						return nil, fmt.Errorf("failed to create zstd compressor: %w", err)
					}
					if _, err := io.Copy(zw, combinedRA); err != nil {
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
					stampDmVerityAnnotations(newDesc.Annotations, verity)
					log.G(ctx).Debugf("converted %s to EROFS+zstd+dmverity (single frame)", desc.Digest)
				}
			} else if erofsSize >= chunkingThreshold {
				// Large image, no dm-verity: stream EROFS file → chunked +zstd.
				// chunked.Build reads per-chunk windows from erofsFile; no
				// full-image copy is held in RAM.  DiffID is hashed in-stream.
				result, err := chunked.Build(erofsFile, erofsSize, cw,
					contentindex.MediaTypeEROFSZstd, effectiveFrameSize)
				erofsFile.Close()
				if err != nil {
					return nil, fmt.Errorf("failed to build chunked EROFS blob: %w", err)
				}
				blobDigest := cw.Digest()
				labelz[labels.LabelUncompressed] = result.DiffID.String()
				if cerr := cw.Commit(ctx, result.Written, blobDigest, content.WithLabels(labelz)); cerr != nil && !errdefs.IsAlreadyExists(cerr) {
					return nil, fmt.Errorf("failed to commit chunked blob: %w", cerr)
				}
				newDesc = result.Descriptor
				newDesc.Digest = blobDigest
				if newDesc.Annotations == nil {
					newDesc.Annotations = make(map[string]string)
				}
				newDesc.Annotations[contentindex.AnnotationUncompressedDigest] = result.DiffID.String()
				log.G(ctx).Debugf("converted %s to EROFS+zstd with chunk index (%d chunks)", desc.Digest, len(result.Chunks))
			} else {
				// Small image, no dm-verity: single zstd frame streamed from file.
				// Compute diffID simultaneously via TeeReader.
				diffIDHasher := digest.SHA256.Digester()
				zw, err := compression.CompressStream(cw, compression.Zstd)
				if err != nil {
					erofsFile.Close()
					return nil, fmt.Errorf("failed to create zstd compressor: %w", err)
				}
				if _, err := io.Copy(zw, io.TeeReader(erofsFile, diffIDHasher.Hash())); err != nil {
					erofsFile.Close()
					zw.Close()
					return nil, fmt.Errorf("failed to compress EROFS blob: %w", err)
				}
				erofsFile.Close()
				if err := zw.Close(); err != nil {
					return nil, fmt.Errorf("failed to finalize zstd stream: %w", err)
				}
				diffID := diffIDHasher.Digest()
				labelz[labels.LabelUncompressed] = diffID.String()
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
				log.G(ctx).Debugf("converted %s to EROFS+zstd (single frame, below %d MiB threshold)", desc.Digest, chunkingThreshold/1024/1024)
			}
			return &newDesc, nil
		}

		// Raw (uncompressed) EROFS: ingest the raw image bytes directly.
		// When dm-verity is requested, the merkle tree is appended after the
		// EROFS bytes ([raw EROFS][verity SB][merkle tree]).  No zstd is
		// involved; verity operates purely on byte offsets via annotations.
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

		var verity *erofsutils.DmVerityResult
		if convertOpts.dmVerity {
			// Stream EROFS → temp file → AppendDmVerityStream → cw.
			// The EROFS image is already on disk in erofsFile; we copy it
			// to a fresh file, append SB+tree, then ingest the combined
			// result.  Memory: O(leaf hashes) ≈ size/blockSize * 32 bytes.
			combinedFile, vres, err := streamAppendDmVerity(ctx, erofsFile, fi.Size())
			erofsFile.Close()
			if err != nil {
				return nil, fmt.Errorf("raw dm-verity: %w", err)
			}
			defer os.Remove(combinedFile.Name())
			defer combinedFile.Close()
			if _, err := combinedFile.Seek(0, io.SeekStart); err != nil {
				return nil, fmt.Errorf("seek combined: %w", err)
			}
			if _, err := io.Copy(cw, combinedFile); err != nil {
				return nil, fmt.Errorf("copy combined to writer: %w", err)
			}
			verity = vres
		} else {
			if _, err := io.Copy(cw, erofsFile); err != nil {
				erofsFile.Close()
				return nil, fmt.Errorf("failed to copy EROFS blob: %w", err)
			}
			erofsFile.Close()
		}

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
		if verity != nil {
			newDesc.Annotations = map[string]string{}
			stampDmVerityAnnotations(newDesc.Annotations, verity)
		}
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
