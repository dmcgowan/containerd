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
	"os"
	"strings"

	"github.com/containerd/containerd/v2/cmd/ctr/commands"
	"github.com/containerd/containerd/v2/core/content/index/local"
	"github.com/containerd/containerd/v2/core/images/converter"
	"github.com/containerd/containerd/v2/core/images/converter/erofs"
	"github.com/containerd/containerd/v2/core/images/converter/uncompress"
	"github.com/containerd/platforms"
	"github.com/urfave/cli/v2"
)

var convertCommand = &cli.Command{
	Name:      "convert",
	Usage:     "Convert an image",
	ArgsUsage: "[flags] <source_ref> <target_ref>",
	Description: `Convert an image format.

e.g., 'ctr image convert --uncompress --oci example.com/foo:orig example.com/foo:converted'
      'ctr image convert --erofs raw example.com/foo:orig example.com/foo:erofs'
      'ctr image convert --erofs zstd example.com/foo:orig example.com/foo:erofs-zstd'
      'ctr image convert --erofs-chunked example.com/foo:orig example.com/foo:erofs-chunked'

      # Keep the original tar manifests first and append EROFS variants after them.
      # The result is an OCI image index compatible with all runtimes: non-EROFS
      # runtimes pick the first matching manifest (the original tar-based one),
      # EROFS-capable runtimes prefer the entry with os.features=["erofs"].
      'ctr image convert --erofs-chunked --append-to-index \
          docker.io/library/alpine:latest docker.io/dmcgowan/alpine:erofs-chunked'

Use '--platform' to define which platforms to convert.
When '--all-platforms' is given, all images in a manifest list must be available.
When '--append-to-index' is given, the source manifests are kept unchanged and
the converted manifests are appended after them in a new OCI image index.
`,
	Flags: []cli.Flag{
		// generic flags
		&cli.BoolFlag{
			Name:  "uncompress",
			Usage: "Convert tar.gz layers to uncompressed tar layers",
		},
		&cli.BoolFlag{
			Name:  "oci",
			Usage: "Convert Docker media types to OCI media types",
		},
		// erofs flags
		&cli.StringFlag{
			Name:  "erofs",
			Usage: "Convert layers to EROFS format, must specify 'raw' or 'zstd' (e.g. --erofs raw, --erofs zstd)",
		},
		&cli.StringFlag{
			Name:  "erofs-compressors",
			Usage: "Specify compression algorithm list when converting EROFS layers",
		},
		&cli.StringFlag{
			Name:  "erofs-mkfs-options",
			Usage: "Extra mkfs options applied when converting EROFS layers. (e.g. '-Efragments,dedupe')",
		},
		// index-append flag: keeps originals, adds EROFS variants
		&cli.BoolFlag{
			Name: "append-to-index",
			Usage: "Build a dual-format OCI image index that contains both the original " +
				"manifests and the converted ones. Original (tar-based) manifests are " +
				"listed first for backward compatibility; converted EROFS manifests " +
				"follow with os.features=[\"erofs\"] on each platform descriptor. " +
				"Non-EROFS-aware runtimes automatically pick the first matching manifest. " +
				"Use with --erofs-chunked (or --erofs zstd).",
		},
		// erofs chunked index flags
		&cli.BoolFlag{
			Name: "erofs-chunked",
			Usage: "Convert layers to EROFS chunked+zstd format with an appended chunk index " +
				"(application/vnd.erofs.layer.v1+zstd with org.erofs.index.* annotations). " +
				"Produces a blob compatible with the EROFS image spec for lazy loading. " +
				"The whole blob is stored in the content store so it can be exported and pushed. " +
				"Use --erofs-chunk-size to control the uncompressed chunk size (default 4 MiB).",
		},
		&cli.IntFlag{
			Name:  "erofs-chunk-size",
			Value: 4 * 1024 * 1024,
			Usage: "Uncompressed chunk size in bytes for --erofs-chunked (default: 4 MiB)",
		},
		// platform flags
		&cli.StringSliceFlag{
			Name:  "platform",
			Usage: "Pull content from a specific platform",
			Value: cli.NewStringSlice(),
		},
		&cli.BoolFlag{
			Name:  "all-platforms",
			Usage: "Exports content from all platforms",
		},
	},
	Action: func(cliContext *cli.Context) error {
		var convertOpts []converter.Opt
		srcRef := cliContext.Args().Get(0)
		targetRef := cliContext.Args().Get(1)
		if srcRef == "" || targetRef == "" {
			return errors.New("src and target image need to be specified")
		}

		if !cliContext.Bool("all-platforms") {
			if pss := cliContext.StringSlice("platform"); len(pss) > 0 {
				all, err := platforms.ParseAll(pss)
				if err != nil {
					return err
				}
				convertOpts = append(convertOpts, converter.WithPlatform(platforms.Ordered(all...)))
			} else {
				convertOpts = append(convertOpts, converter.WithPlatform(platforms.DefaultStrict()))
			}
		}

		if cliContext.Bool("uncompress") {
			convertOpts = append(convertOpts, converter.WithLayerConvertFunc(uncompress.LayerConvertFunc))
		}

		if cliContext.IsSet("erofs") {
			var erofsOpts []erofs.ConvertOpt
			switch cliContext.String("erofs") {
			case "raw":
			case "zstd":
				erofsOpts = append(erofsOpts, erofs.WithBlobCompression("zstd"))
			default:
				return fmt.Errorf("unsupported erofs format %q, supported: raw, zstd", cliContext.String("erofs"))
			}
			if compressors := cliContext.String("erofs-compressors"); compressors != "" {
				erofsOpts = append(erofsOpts, erofs.WithCompressors(compressors))
			}
			if mkfsOptsStr := cliContext.String("erofs-mkfs-options"); mkfsOptsStr != "" {
				mkfsOpts := strings.Fields(mkfsOptsStr)
				erofsOpts = append(erofsOpts, erofs.WithMkfsOptions(mkfsOpts))
			}
			convertOpts = append(convertOpts, converter.WithLayerConvertFunc(erofs.LayerConvertFunc(erofsOpts...)))
			convertOpts = append(convertOpts, converter.WithUpdateManifest(erofs.UpdateManifestPlatform))
			if cliContext.Bool("append-to-index") {
				convertOpts = append(convertOpts, converter.WithAppendToIndex())
			}
		}

		if cliContext.Bool("erofs-chunked") {
			// Create a temporary indexed content store sidecar backed by the
			// daemon's content store.  The sidecar persists only for the
			// lifetime of this ctr invocation; the whole blobs remain in the
			// daemon's content store after ctr exits, so they can be exported
			// or pushed normally.
			//
			// Operators who want persistent indexed store integration should
			// use the daemon's "io.containerd.content.index.v1" plugin
			// directly.  This temporary store is sufficient for producing the
			// correct blob format and annotations.
			client, ctx, cancel, err := commands.NewClient(cliContext)
			if err != nil {
				return err
			}
			defer cancel()

			tmpDir, err := os.MkdirTemp("", "ctr-erofs-chunked-*")
			if err != nil {
				return fmt.Errorf("create temp dir for indexed store: %w", err)
			}
			defer os.RemoveAll(tmpDir)

			idxStore, err := local.NewStore(local.Config{
				Root:    tmpDir,
				Content: client.ContentStore(),
			})
			if err != nil {
				return fmt.Errorf("open temporary indexed store: %w", err)
			}
			defer idxStore.Close()

			chunkSize := cliContext.Int("erofs-chunk-size")

			var erofsOpts []erofs.ConvertOpt
			if compressors := cliContext.String("erofs-compressors"); compressors != "" {
				erofsOpts = append(erofsOpts, erofs.WithCompressors(compressors))
			}
			if mkfsOptsStr := cliContext.String("erofs-mkfs-options"); mkfsOptsStr != "" {
				mkfsOpts := strings.Fields(mkfsOptsStr)
				erofsOpts = append(erofsOpts, erofs.WithMkfsOptions(mkfsOpts))
			}

			convertOpts = append(convertOpts,
				converter.WithLayerConvertFunc(erofs.LayerConvertFuncChunked(idxStore, chunkSize, erofsOpts...)),
				converter.WithUpdateManifest(erofs.UpdateManifestPlatform),
			)
			if cliContext.Bool("append-to-index") {
				convertOpts = append(convertOpts, converter.WithAppendToIndex())
			}

			newImg, err := converter.Convert(ctx, client, targetRef, srcRef, convertOpts...)
			if err != nil {
				return err
			}
			fmt.Fprintln(cliContext.App.Writer, newImg.Target.Digest.String())
			return nil
		}

		if cliContext.Bool("oci") {
			convertOpts = append(convertOpts, converter.WithDockerToOCI(true))
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
