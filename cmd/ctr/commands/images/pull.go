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
	"context"
	"fmt"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/cmd/ctr/commands"
	"github.com/containerd/containerd/cmd/ctr/commands/content"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/platforms"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/urfave/cli"
)

var pullCommand = cli.Command{
	Name:      "pull",
	Usage:     "pull an image from a remote",
	ArgsUsage: "[flags] <ref>",
	Description: `Fetch and prepare an image for use in containerd.

After pulling an image, it should be ready to use the same reference in a run
command. As part of this process, we do the following:

1. Fetch all resources into containerd.
2. Prepare the snapshot filesystem with the pulled resources.
3. Register metadata for the image.
`,
	Flags: append(append(commands.RegistryFlags, append(commands.SnapshotterFlags, commands.LabelFlag)...),
		cli.StringSliceFlag{
			Name:  "platform",
			Usage: "Pull content from a specific platform",
			Value: &cli.StringSlice{},
		},
		cli.BoolFlag{
			Name:  "all-platforms",
			Usage: "pull content from all platforms",
		},
	),
	Action: func(context *cli.Context) error {
		var (
			ref = context.Args().First()
		)
		if ref == "" {
			return fmt.Errorf("please provide an image reference to pull")
		}

		client, ctx, cancel, err := commands.NewClient(context)
		if err != nil {
			return err
		}
		defer cancel()

		ctx, done, err := client.WithLease(ctx)
		if err != nil {
			return err
		}
		defer done(ctx)

		config, err := content.NewFetchConfig(ctx, context)
		if err != nil {
			return err
		}

		// Multiple unpack on pull not yet supported
		if context.Bool("all-platforms") || len(config.Platforms) > 1 {
			img, err := content.Fetch(ctx, client, ref, config)
			if err != nil {
				return err
			}

			log.G(ctx).WithField("image", ref).Debug("unpacking")
			var p []ocispec.Platform
			if context.Bool("all-platforms") {
				p, err = images.Platforms(ctx, client.ContentStore(), img.Target)
				if err != nil {
					return errors.Wrap(err, "unable to resolve image platforms")
				}
			} else {
				for _, s := range context.StringSlice("platform") {
					ps, err := platforms.Parse(s)
					if err != nil {
						return errors.Wrapf(err, "unable to parse platform %s", s)
					}
					p = append(p, ps)
				}
			}
			if len(p) == 0 {
				p = append(p, platforms.DefaultSpec())
			}

			for _, platform := range p {
				fmt.Printf("unpacking %s %s...\n", platforms.Format(platform), img.Target.Digest)
				i := containerd.NewImageWithPlatform(client, img, platforms.Only(platform))
				err = i.Unpack(ctx, context.String("snapshotter"))
				if err != nil {
					return err
				}
			}
			fmt.Println("done")
			return nil
		}

		// TODO: Show unpack status
		_, err = Pull(ctx, client, ref, config)

		return err
	},
}

// Pull loads all resources into the content store and returns the image
func Pull(ctx context.Context, client *containerd.Client, ref string, config *content.FetchConfig) (containerd.Image, error) {
	ongoing := content.NewJobs(ref)

	pctx, stopProgress := context.WithCancel(ctx)
	progress := make(chan struct{})

	go func() {
		if config.ProgressOutput != nil {
			// no progress bar, because it hides some debug logs
			content.ShowProgress(pctx, ongoing, client.ContentStore(), config.ProgressOutput)
		}
		close(progress)
	}()

	h := images.HandlerFunc(func(ctx context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		if desc.MediaType != images.MediaTypeDockerSchema1Manifest {
			ongoing.Add(desc)
		}
		return nil, nil
	})

	log.G(pctx).WithField("image", ref).Debug("fetching")
	labels := commands.LabelArgs(config.Labels)
	opts := []containerd.RemoteOpt{
		containerd.WithPullLabels(labels),
		containerd.WithResolver(config.Resolver),
		containerd.WithImageHandler(h),
		containerd.WithSchema1Conversion,
		containerd.WithAppendDistributionSourceLabel(),
		containerd.WithPullUnpack,
	}
	if config.Snapshotter != "" {
		opts = append(opts, containerd.WithPullSnapshotter(config.Snapshotter))
	}

	for _, platform := range config.Platforms {
		opts = append(opts, containerd.WithPlatform(platform))
	}

	img, err := client.Pull(pctx, ref, opts...)
	stopProgress()
	if err != nil {
		return nil, err
	}

	<-progress
	return img, nil
}
