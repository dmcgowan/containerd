package main

import (
	"github.com/boltdb/bolt"
	"github.com/pkg/errors"
	"github.com/urfave/cli"
)

var snapshotCommand = cli.Command{
	Name:        "snapshot",
	Description: "snapshot database debugging",
	Subcommands: cli.Commands{
		dumpCommand,
	},
}

var dumpCommand = cli.Command{
	Name:        "dump",
	ArgsUsage:   "[flags] metadata-db-file",
	Description: `Dump snapshot storage database`,
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
					buckets: map[string]Dumper{
						"snapshots": dumper{
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
						"parents": dumper{
							key:   parentKey,
							value: strval,
						},
					},
				},
			},
		}

		return Dump(db, root)
	},
}
