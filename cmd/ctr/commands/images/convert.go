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

package images

import (
	"errors"
	"fmt"

	"github.com/containerd/containerd/v2/cmd/ctr/commands"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/converter"
	"github.com/containerd/containerd/v2/core/images/converter/erofs"
	"github.com/containerd/containerd/v2/core/images/converter/uncompress"
	"github.com/containerd/platforms"
	"github.com/urfave/cli/v2"
)

// Default layer type for `ctr image convert`.  The canonical lazy-loading
// format is chunked + zstd EROFS with dm-verity on by default.
const defaultLayerType = images.MediaTypeErofsZstd

// Default chunk size for the chunked-EROFS converter, used when
// --erofs-chunk-size is not set.
//
// This is the target *compressed* frame size — the chunked builder
// estimates uncompressed input per chunk as roughly 3× this value
// (zstd compresses EROFS data well; see core/content/index/chunked
// /builder.go).  1.5 MiB compressed → ~4.5 MiB of uncompressed
// EROFS data per chunk: small enough that first-page container
// reads only pull what's actually touched, large enough that the
// chunk-index trailer stays compact (≈ 48 bytes per chunk) and the
// per-chunk zstd framing overhead doesn't dominate.
//
// A future revision may tier this by layer size (smaller chunks for
// small layers, larger for huge ones).  Keep this constant in sync
// with any such default-selection helper.
const defaultErofsChunkSize = 1536 * 1024 // 1.5 MiB compressed

// isErofsLayerType reports whether mt is one of the EROFS media types
// the convert pipeline knows how to produce.
func isErofsLayerType(mt string) bool {
	switch mt {
	case images.MediaTypeErofs, images.MediaTypeErofsZstd:
		return true
	}
	return false
}

var convertCommand = &cli.Command{
	Name:      "convert",
	Usage:     "Convert an image",
	ArgsUsage: "[flags] <source_ref> <target_ref>",
	Description: `Convert an image to a different layer format.

Defaults (zero flags) produce a single-layer, lazy-loading, dm-verity-protected
EROFS image — the format optimised for fast container start.  Override the
defaults with the flags below.

Examples:

  # Default: merged single-layer EROFS+zstd+chunk-index+dm-verity.
  ctr image convert example.com/foo:orig example.com/foo:erofs

  # Keep per-layer structure (skip merge) but otherwise default settings.
  ctr image convert --erofs-keep-layers example.com/foo:orig example.com/foo:erofs-multilayer

  # Raw (uncompressed) EROFS instead of the default zstd+chunked.
  ctr image convert --layer-type application/vnd.erofs example.com/foo:orig example.com/foo:erofs-raw

  # Same as default but without dm-verity (smaller blob, no integrity at mount).
  ctr image convert --erofs-dmverity=false example.com/foo:orig example.com/foo:erofs-noverity

  # Just uncompress / repack as OCI; no EROFS conversion.
  ctr image convert --layer-type "" --uncompress --oci example.com/foo:orig example.com/foo:oci

Index behaviour:
  EROFS conversions append the new manifests to the existing image index
  by default, keeping the original tar-based manifests first so non-EROFS
  runtimes transparently fall back.  Pass --erofs-replace to produce an
  EROFS-only index.

Use --platform to define which platforms to convert.  When --all-platforms
is given, all images in a manifest list must be available.
`,
	Flags: []cli.Flag{
		// Layer-type selector.  Defaults to EROFS+zstd (chunked + verity).
		// Pass an empty string ("") to skip EROFS conversion entirely (the
		// command then only runs --uncompress / --oci, if set).
		&cli.StringFlag{
			Name:  "layer-type",
			Value: defaultLayerType,
			Usage: "Target layer media type. Defaults to '" + defaultLayerType + "'. " +
				"Supported EROFS values: '" + images.MediaTypeErofsZstd + "' (chunked + zstd, " +
				"the lazy-loading format) and '" + images.MediaTypeErofs + "' (raw EROFS). " +
				"Pass an empty string to skip EROFS conversion entirely.",
		},

		// Generic format flags (apply regardless of --layer-type).
		&cli.BoolFlag{
			Name:  "uncompress",
			Usage: "Convert tar.gz layers to uncompressed tar layers (applies before EROFS).",
		},
		&cli.BoolFlag{
			Name:  "oci",
			Usage: "Convert Docker media types to OCI media types.",
		},

		// EROFS knobs.  All --erofs-* flags only apply when --layer-type
		// selects an EROFS media type.

		// Merge IS the default for EROFS conversions; --erofs-keep-layers opts out.
		&cli.BoolFlag{
			Name: "erofs-keep-layers",
			Usage: "Preserve the image's per-layer structure. " +
				"Default behaviour collapses all layers into a single merged " +
				"EROFS image (with whiteouts/deletions applied) so the result " +
				"represents the final filesystem state.  --erofs-keep-layers " +
				"produces one EROFS layer per source layer instead.",
		},

		// dm-verity is ON by default.  --erofs-dmverity=false opts out.
		// This is a regular bool flag with Value=true; users disable it via
		// --erofs-dmverity=false (urfave/cli supports this).
		&cli.BoolFlag{
			Name:  "erofs-dmverity",
			Value: true,
			Usage: "Append a dm-verity merkle tree to each EROFS layer " +
				"(single-file dm-verity layout). Sets org.erofs.dmverity.* " +
				"annotations on the layer descriptor.  Pure-Go: no external " +
				"binary required.  Default on; pass --erofs-dmverity=false to " +
				"opt out.  Mounting the result requires a dm-verity-capable kernel.",
		},

		// Chunk size for the chunked converter (default 1.5 MiB compressed).
		// Only meaningful when --layer-type is the +zstd variant.
		&cli.IntFlag{
			Name:  "erofs-chunk-size",
			Value: defaultErofsChunkSize,
			Usage: "Target *compressed* zstd frame size in bytes for EROFS+zstd " +
				"chunks (default: 1.5 MiB).  Each chunk consumes roughly 3× " +
				"this value of uncompressed EROFS input (zstd compresses EROFS " +
				"data well).  Smaller values = finer-grained lazy loading at " +
				"the cost of a larger chunk index.  Only meaningful when " +
				"--layer-type is " + images.MediaTypeErofsZstd + ".",
		},

		// Split-data: the cross-image deduplication format.  Mutually
		// exclusive with the per-image --layer-type EROFS conversions.
		&cli.BoolFlag{
			Name: "erofs-split-data",
			Usage: "Convert each layer into a pair of regular EROFS layers laid out " +
				"for cross-image deduplication (erofs-image-spec §9.1): " +
				"(1) a data-bearing EROFS layer holding file payloads in a " +
				"content-hash-friendly order, and " +
				"(2) a consuming EROFS layer whose inodes reference the data-bearing " +
				"layer's blocks via multi-device addressing. " +
				"Both outputs use the canonical " + images.MediaTypeErofs + " media type. " +
				"Mutually exclusive with --layer-type EROFS variants and --erofs-keep-layers.",
		},
		&cli.IntFlag{
			Name:  "erofs-split-inline-threshold",
			Value: 4096,
			Usage: "Files whose (inode + content) size is below this threshold stay inline " +
				"in the metadata layer rather than going to the data layer (default: 4096 = one block). " +
				"Only meaningful with --erofs-split-data.",
		},

		// Index behaviour.
		&cli.BoolFlag{
			Name: "erofs-replace",
			Usage: "Replace the entire image index with the EROFS-only result. " +
				"By default EROFS conversions append the new manifests to the " +
				"existing index, keeping the original tar-based manifests first " +
				"so non-EROFS-aware runtimes transparently fall back to the tar variant.",
		},

		// Platform / concurrency.
		&cli.StringSliceFlag{
			Name:  "platform",
			Usage: "Pull content from a specific platform.",
			Value: cli.NewStringSlice(),
		},
		&cli.BoolFlag{
			Name:  "all-platforms",
			Usage: "Exports content from all platforms.",
		},
		&cli.IntFlag{
			Name:  "parallelism",
			Value: 0,
			Usage: "Maximum number of manifests to convert concurrently (0 = unbounded).",
		},
	},
	Action: func(cliContext *cli.Context) error {
		srcRef := cliContext.Args().Get(0)
		targetRef := cliContext.Args().Get(1)
		if srcRef == "" || targetRef == "" {
			return errors.New("src and target image need to be specified")
		}

		if err := validateConvertFlags(cliContext); err != nil {
			return err
		}

		layerType := cliContext.String("layer-type")
		erofsEnabled := isErofsLayerType(layerType)
		splitData := cliContext.Bool("erofs-split-data")

		var convertOpts []converter.Opt

		// --parallelism.
		if p := cliContext.Int("parallelism"); p > 0 {
			convertOpts = append(convertOpts, converter.WithParallelism(p))
		}

		// Platform selection.
		//
		// EROFS conversions default to all platforms (non-Linux are skipped
		// automatically by the converter's OS guard); other conversions
		// default to the local platform.
		//
		// Exception: when EROFS conversions REPLACE the index AND merge is in
		// effect (default), default to the local platform only — merging all
		// platforms concurrently is memory-intensive and replace mode is
		// typically used for single-platform images.  Use --all-platforms
		// or --platform to override.
		isEROFSConv := erofsEnabled || splitData
		merging := erofsEnabled && !cliContext.Bool("erofs-keep-layers")
		isMergeReplace := merging && cliContext.Bool("erofs-replace")
		if !cliContext.Bool("all-platforms") {
			if pss := cliContext.StringSlice("platform"); len(pss) > 0 {
				all, err := platforms.ParseAll(pss)
				if err != nil {
					return err
				}
				convertOpts = append(convertOpts, converter.WithPlatform(platforms.Ordered(all...)))
			} else if !isEROFSConv || isMergeReplace {
				convertOpts = append(convertOpts, converter.WithPlatform(platforms.DefaultStrict()))
			}
		}

		// --uncompress / --oci: orthogonal to --layer-type.
		if cliContext.Bool("uncompress") {
			convertOpts = append(convertOpts, converter.WithLayerConvertFunc(uncompress.LayerConvertFunc))
		}
		if cliContext.Bool("oci") {
			convertOpts = append(convertOpts, converter.WithDockerToOCI(true))
		}

		// --erofs-split-data: independent code path (multi-layer convert).
		if splitData {
			threshold := cliContext.Int("erofs-split-inline-threshold")
			var erofsOpts []erofs.ConvertOpt
			convertOpts = append(convertOpts,
				converter.WithMultiLayerConvertFunc(erofs.LayerConvertFuncSplitData(threshold, erofsOpts...)),
				converter.WithUpdateManifest(erofs.UpdateManifestPlatform),
			)
			if !cliContext.Bool("erofs-replace") {
				convertOpts = append(convertOpts, converter.WithAppendToIndex())
			}
		} else if erofsEnabled {
			// Per-layer or merge EROFS conversion.
			erofsOpts := erofsConvertOptions(cliContext, layerType)

			switch {
			case merging:
				// Merge: replace all source layers with a single merged
				// EROFS image via UpdateManifest.  No per-layer convert
				// func — MergeManifestFunc reads the original tar layers
				// directly.
				convertOpts = append(convertOpts, converter.WithUpdateManifest(erofs.MergeManifestFunc(erofsOpts...)))

			case layerType == images.MediaTypeErofsZstd:
				// Per-layer chunked+zstd EROFS (the canonical lazy format
				// in --erofs-keep-layers mode).
				chunkSize := cliContext.Int("erofs-chunk-size")
				convertOpts = append(convertOpts,
					converter.WithLayerConvertFunc(erofs.LayerConvertFuncChunked(nil, chunkSize, erofsOpts...)),
					converter.WithUpdateManifest(erofs.UpdateManifestPlatform),
				)

			default:
				// Per-layer raw EROFS (--layer-type vnd.erofs +
				// --erofs-keep-layers).
				convertOpts = append(convertOpts,
					converter.WithLayerConvertFunc(erofs.LayerConvertFunc(erofsOpts...)),
					converter.WithUpdateManifest(erofs.UpdateManifestPlatform),
				)
			}

			if !cliContext.Bool("erofs-replace") {
				convertOpts = append(convertOpts, converter.WithAppendToIndex())
			}
		}

		client, ctx, cancel, err := commands.NewClient(cliContext)
		if err != nil {
			return err
		}
		defer cancel()

		newImg, err := converter.Convert(ctx, client, targetRef, srcRef, convertOpts...)
		if err != nil {
			return err
		}
		fmt.Fprintln(cliContext.App.Writer, newImg.Target.Digest.String())
		return nil
	},
}

// erofsConvertOptions builds the []erofs.ConvertOpt list driven by --layer-type
// and the EROFS-specific flags.  Called only when erofsEnabled is true.
//
// --erofs-chunk-size is threaded uniformly via WithTargetFrameSize so that
// every chunked entry point (per-layer LayerConvertFunc, LayerConvertFuncChunked,
// and the default MergeManifestFunc path) honours the user's chunk-granularity
// choice.  This closes the gap where MergeManifestFunc previously hard-coded
// chunked.TargetFrameSize and silently ignored --erofs-chunk-size.
func erofsConvertOptions(cliContext *cli.Context, layerType string) []erofs.ConvertOpt {
	var opts []erofs.ConvertOpt
	if layerType == images.MediaTypeErofsZstd {
		opts = append(opts, erofs.WithBlobCompression("zstd"))
	}
	if cliContext.Bool("erofs-dmverity") {
		opts = append(opts, erofs.WithDmVerity())
	}
	if layerType == images.MediaTypeErofsZstd {
		opts = append(opts, erofs.WithTargetFrameSize(cliContext.Int("erofs-chunk-size")))
	}
	return opts
}

// validateConvertFlags rejects clearly-incompatible flag combinations before
// the converter runs.  All validation is local: no host probing, no kernel
// capability check (the convert pipeline is pure-Go).
func validateConvertFlags(cliContext *cli.Context) error {
	layerType := cliContext.String("layer-type")
	erofsEnabled := isErofsLayerType(layerType)
	splitData := cliContext.Bool("erofs-split-data")

	// Reject unknown --layer-type values up front.  Empty string is the
	// explicit "skip EROFS conversion" sentinel.
	if layerType != "" && !erofsEnabled {
		return fmt.Errorf("--layer-type %q is not supported; supported: %q, %q, or \"\" (skip)",
			layerType, images.MediaTypeErofsZstd, images.MediaTypeErofs)
	}

	// --erofs-split-data is its own format; it cannot be combined with the
	// per-image EROFS conversion driven by --layer-type, nor with merge.
	if splitData {
		if erofsEnabled && cliContext.IsSet("layer-type") {
			return fmt.Errorf("--erofs-split-data is mutually exclusive with --layer-type %q; pass --layer-type \"\" or omit --erofs-split-data",
				layerType)
		}
		if cliContext.IsSet("erofs-keep-layers") {
			return fmt.Errorf("--erofs-split-data is mutually exclusive with --erofs-keep-layers")
		}
	}

	// EROFS-specific flags only make sense when EROFS conversion is active.
	// Surface clearly-misused flags as errors so users notice early.
	if !erofsEnabled && !splitData {
		for _, f := range []string{"erofs-keep-layers", "erofs-chunk-size", "erofs-replace"} {
			if cliContext.IsSet(f) {
				return fmt.Errorf("--%s requires an EROFS --layer-type (got %q)", f, layerType)
			}
		}
		// --erofs-dmverity is technically harmless when EROFS is disabled
		// (it's a no-op), but if the user explicitly set it they probably
		// expect EROFS conversion.  Flag this too so the silence isn't
		// surprising.
		if cliContext.IsSet("erofs-dmverity") {
			return fmt.Errorf("--erofs-dmverity requires an EROFS --layer-type (got %q)", layerType)
		}
	}

	// --erofs-chunk-size only applies to the +zstd layer type.
	if cliContext.IsSet("erofs-chunk-size") && erofsEnabled && layerType != images.MediaTypeErofsZstd {
		return fmt.Errorf("--erofs-chunk-size requires --layer-type %s (got %q)",
			images.MediaTypeErofsZstd, layerType)
	}

	// Chunk-size sanity check.  This is a *compressed* target frame
	// size; below 4 KiB the zstd framing overhead dominates and the
	// chunk index grows unbounded.  Catch absurd values.
	if cs := cliContext.Int("erofs-chunk-size"); cs < 4096 {
		return fmt.Errorf("--erofs-chunk-size %d is below the minimum (4096 bytes)", cs)
	}

	// Nothing-to-do guard.  When EROFS is skipped and no other transformer
	// is engaged, the command would silently noop into a copy.
	if !erofsEnabled && !splitData &&
		!cliContext.Bool("uncompress") && !cliContext.Bool("oci") {
		return errors.New("no conversion requested: pass a --layer-type, --uncompress, --oci, or --erofs-split-data")
	}

	return nil
}
