package main

import (
	"context"
	"encoding/base32"
	"fmt"
	"math/rand"
	"time"

	"github.com/boltdb/bolt"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/errdefs"
	gcmetadata "github.com/containerd/containerd/gc/metadata"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/metadata"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/snapshot"
	"github.com/gogo/protobuf/types"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

var gcCommand = cli.Command{
	Name:        "gc",
	Description: "GC is used to debug garbage collection on metadata",
	Subcommands: cli.Commands{
		populateCommand,
		metadataGCCommand,
	},
}

var populateCommand = cli.Command{
	Name:        "populate",
	ArgsUsage:   "[flags] metadata-db-file",
	Description: `Populates random values in metadata db`,
	Flags: []cli.Flag{
		cli.IntFlag{
			Name:  "n",
			Usage: "Number of item sets to add to the database (10 entries per set)",
			Value: 10,
		},
		cli.StringFlag{
			Name:  "namespace",
			Usage: "Namespace to populate",
			Value: "debugtool",
		},
	},
	Action: func(clicontext *cli.Context) error {
		if clicontext.NArg() != 1 {
			return errors.Errorf("unexpected number of arguments: %d", clicontext.NArg())
		}

		storageFile := clicontext.Args().Get(0)

		db, err := bolt.Open(storageFile, 0600, nil)
		if err != nil {
			return errors.Wrap(err, "failed to open database file")
		}
		defer db.Close()

		cs := metadata.NewContentStore(db, noopContentStore{})
		sn := metadata.NewSnapshotter(db, "noop", noopSnapshotter{})
		ctx := namespaces.WithNamespace(context.Background(), "debug") //clicontext.String("namespace"))

		return db.Update(func(tx *bolt.Tx) error {
			ctx := metadata.WithTransactionContext(ctx, tx)
			total := clicontext.Int("n")
			var buf [256]byte

			r := rand.New(rand.NewSource(time.Now().UnixNano()))
			for i := 0; i < total; i++ {
				r.Read(buf[:5])
				id := base32.HexEncoding.WithPadding(base32.NoPadding).EncodeToString(buf[:6])

				// Add content blob
				data := buf[:]
				r.Read(data)
				expected := digest.FromBytes(data)

				w, err := cs.Writer(ctx, "test-ref", int64(len(data)), expected)
				if err != nil {
					return errors.Wrap(err, "failed to create writer")
				}
				if _, err := w.Write(data); err != nil {
					return errors.Wrap(err, "write blob failed")
				}
				if err := w.Commit(ctx, int64(len(data)), expected); err != nil {
					return errors.Wrap(err, "failed to commit blob")
				}

				// Add image pointing to blob
				_, err = metadata.NewImageStore(tx).Create(ctx, images.Image{
					Name: fmt.Sprintf("image-%s", id),
					Target: ocispec.Descriptor{
						Digest: expected,
						Size:   int64(len(data)),
					},
				})
				if err != nil {
					return err
				}

				lastSnapshot := 7
				var key string
				for j := 0; j <= lastSnapshot; j++ {
					parent := key
					key = fmt.Sprintf("snapshot-id-%d", id, j)

					active := fmt.Sprintf("active %s", key)
					if _, err := sn.Prepare(ctx, active, parent); err != nil {
						return err
					}
					if err := sn.Commit(ctx, key, active); err != nil {
						return err
					}
				}

				container := containers.Container{
					ID:          fmt.Sprintf("container-%s", id),
					SnapshotKey: key,
					Snapshotter: "noop",
					//Labels:      map[string]string{},

					Runtime: containers.RuntimeInfo{
						Name: "testruntime",
					},
					Spec: &types.Any{},
				}
				_, err = metadata.NewContainerStore(tx).Create(ctx, container)
				if err != nil {
					return err
				}
			}

			return nil
		})
	},
}

var metadataGCCommand = cli.Command{
	Name:        "metadata",
	ArgsUsage:   "[flags] metadata-db-file",
	Description: `Garbage collects the metadata database`,
	Flags:       []cli.Flag{},
	Action: func(clicontext *cli.Context) error {
		if clicontext.NArg() != 1 {
			return errors.Errorf("unexpected number of arguments: %d", clicontext.NArg())
		}

		storageFile := clicontext.Args().Get(0)

		db, err := bolt.Open(storageFile, 0600, nil)
		if err != nil {
			return errors.Wrap(err, "failed to open database file")
		}
		defer db.Close()

		gc, err := gcmetadata.NewMetadataCollector(db)
		if err != nil {
			return err
		}

		start := time.Now()
		if err := gc.Trigger(context.Background()); err != nil {
			return err
		}
		duration := time.Now().Sub(start)

		logrus.Infof("Garbage collection completed in: %s", duration)

		return nil
	},
}

type noopContentStore struct{}

type noopContentWriter struct {
	ref    string
	offset int64
	size   int64
	dgstr  digest.Digester
}

func (noopContentStore) Info(ctx context.Context, dgst digest.Digest) (content.Info, error) {
	return content.Info{}, errdefs.ErrNotFound
}

func (noopContentStore) Update(ctx context.Context, info content.Info, fieldpaths ...string) (content.Info, error) {
	return content.Info{}, errdefs.ErrNotFound
}

func (noopContentStore) Walk(ctx context.Context, fn content.WalkFunc, filters ...string) error {
	return nil
}
func (noopContentStore) Delete(ctx context.Context, dgst digest.Digest) error {
	return errdefs.ErrNotFound
}
func (noopContentStore) Status(ctx context.Context, ref string) (content.Status, error) {
	return content.Status{}, errdefs.ErrNotFound
}

func (noopContentStore) ListStatuses(ctx context.Context, filters ...string) ([]content.Status, error) {
	return nil, nil
}

func (noopContentStore) Abort(ctx context.Context, ref string) error {
	return errdefs.ErrNotFound
}

func (noopContentStore) ReaderAt(ctx context.Context, dgst digest.Digest) (content.ReaderAt, error) {
	return nil, errdefs.ErrNotFound
}
func (noopContentStore) Writer(ctx context.Context, ref string, size int64, expected digest.Digest) (content.Writer, error) {
	return &noopContentWriter{
		ref:   ref,
		size:  size,
		dgstr: digest.SHA256.Digester(),
	}, nil
}

func (w *noopContentWriter) Write(p []byte) (int, error) {
	w.offset += int64(len(p))
	return w.dgstr.Hash().Write(p)
}

func (w *noopContentWriter) Close() error {
	return nil
}

func (w *noopContentWriter) Digest() digest.Digest {
	return w.dgstr.Digest()
}

func (w *noopContentWriter) Commit(ctx context.Context, size int64, expected digest.Digest, opts ...content.Opt) error {
	return nil
}

func (w *noopContentWriter) Status() (content.Status, error) {
	return content.Status{
		Ref:    w.ref,
		Offset: w.offset,
		Total:  w.size,
	}, nil
}

func (w *noopContentWriter) Truncate(size int64) error {
	if size != 0 {
		return errors.New("cannot truncate to size")
	}
	w.dgstr = digest.SHA256.Digester()
	w.offset = 0
	return nil
}

type noopSnapshotter struct{}

func (noopSnapshotter) Stat(ctx context.Context, key string) (snapshot.Info, error) {
	return snapshot.Info{}, errdefs.ErrNotFound
}

func (noopSnapshotter) Update(ctx context.Context, info snapshot.Info, fieldpaths ...string) (snapshot.Info, error) {
	return snapshot.Info{}, errdefs.ErrNotFound
}

func (noopSnapshotter) Usage(ctx context.Context, key string) (snapshot.Usage, error) {
	return snapshot.Usage{}, errdefs.ErrNotFound
}

func (noopSnapshotter) Mounts(ctx context.Context, key string) ([]mount.Mount, error) {
	return []mount.Mount{}, nil
}

func (noopSnapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshot.Opt) ([]mount.Mount, error) {
	return []mount.Mount{}, nil
}

func (noopSnapshotter) View(ctx context.Context, key, parent string, opts ...snapshot.Opt) ([]mount.Mount, error) {
	return []mount.Mount{}, nil
}

func (noopSnapshotter) Commit(ctx context.Context, name, key string, opts ...snapshot.Opt) error {
	return nil
}

func (noopSnapshotter) Remove(ctx context.Context, key string) error {
	return nil
}
func (noopSnapshotter) Walk(ctx context.Context, fn func(context.Context, snapshot.Info) error) error {
	return nil
}
