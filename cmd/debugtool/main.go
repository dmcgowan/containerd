package main

import (
	"fmt"
	"os"

	"github.com/containerd/containerd/version"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

func init() {
	cli.VersionPrinter = func(c *cli.Context) {
		fmt.Println(c.App.Name, version.Package, c.App.Version)
	}
}

func main() {
	app := cli.NewApp()
	app.Name = "debugtool"
	app.Version = version.Version
	app.Usage = `containerd debug tool`
	app.Flags = []cli.Flag{
		cli.BoolFlag{
			Name:  "debug",
			Usage: "enable debug output in logs",
		},
	}
	app.Commands = []cli.Command{
		snapshotCommand,
		metadataCommand,
		gcCommand,
	}
	app.Before = func(context *cli.Context) error {
		if context.GlobalBool("debug") {
			logrus.SetLevel(logrus.DebugLevel)
		}
		return nil
	}
	AttachProfiler(app)
	if err := app.Run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "debugtool: %s\n", err)
		os.Exit(1)
	}
}
