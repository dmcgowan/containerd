package main

import (
	"github.com/boltdb/bolt"
	"github.com/pkg/errors"
	"github.com/urfave/cli"
)

var metadataCommand = cli.Command{
	Name:        "metadata",
	Description: "metadata database debugging",
	Subcommands: cli.Commands{
		metadataDumpCommand,
	},
}

var metadataDumpCommand = cli.Command{
	Name:        "dump",
	ArgsUsage:   "[flags] metadata-db-file",
	Description: `Dump metadata database`,
	Flags:       []cli.Flag{},
	Action: func(context *cli.Context) error {
		if context.NArg() != 1 {
			return errors.Errorf("unexpected number of arguments: %d", context.NArg())
		}

		storageFile := context.Args().Get(0)

		db, err := bolt.Open(storageFile, 0600, nil)
		if err != nil {
			return errors.Wrap(err, "failed to open database file")
		}

		root := dumper{
			buckets: map[string]Dumper{
				"v1": dumper{
					any: dumper{
						buckets: map[string]Dumper{
							"content": dumper{
								buckets: map[string]Dumper{
									"blob": dumper{
										any: dumper{
											keys: map[string]valueFn{
												"createdat": timeval,
												"updatedat": timeval,
												"size":      intval,
											},
										},
									},
									"ingest": dumper{},
								},
							},
							"images": dumper{
								any: dumper{
									buckets: map[string]Dumper{
										"target": dumper{
											keys: map[string]valueFn{
												"digest":    strval,
												"mediatype": strval,
												"size":      intval,
											},
										},
									},
									keys: map[string]valueFn{
										"createdat": timeval,
										"updatedat": timeval,
									},
								},
							},
							"snapshots": dumper{
								any: dumper{
									any: dumper{
										any: dumper{
											keys: map[string]valueFn{
												"parent":    strval,
												"createdat": timeval,
												"updatedat": timeval,
												"id":        uintval,
												"inodes":    intval,
												"size":      intval,
											},
										},
									},
								},
							},
						},
					},
				},
			},
		}

		return Dump(db, root)
	},
}
