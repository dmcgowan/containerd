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
	"flag"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/testutil"
	"github.com/containerd/log"
)

func TestMain(m *testing.M) {
	flag.Parse()

	// Short mode: skip daemon lifecycle — run only the fast, network-free tests.
	if testing.Short() {
		os.Exit(m.Run())
	}

	// Non-short tests need root on Unix (snapshotter, mount, etc.).
	testutil.RequiresRootM()

	var buf bytes.Buffer
	ctx, cancel := testContext(nil)
	defer cancel()

	if !noDaemon {
		// Remove any stale state from a prior run.
		_ = os.RemoveAll(defaultRoot)

		stdioFile, err := os.CreateTemp("", "containerd-erofs-stdio-")
		if err != nil {
			fmt.Fprintf(os.Stderr, "could not create stdio temp file: %v\n", err)
			os.Exit(1)
		}
		defer func() {
			stdioFile.Close()
			os.Remove(stdioFile.Name())
		}()
		ctrdStdioFilePath = stdioFile.Name()
		stdioWriter := io.MultiWriter(stdioFile, &buf)

		cfgPath, err := createConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "could not create containerd config: %v\n", err)
			os.Exit(1)
		}
		defer os.Remove(cfgPath)

		err = ctrd.start("containerd", address, []string{
			"--root", defaultRoot,
			"--state", defaultState,
			"--log-level", "debug",
			"--config", cfgPath,
		}, stdioWriter, stdioWriter)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to start containerd: %v\n%s\n", err, buf.String())
			os.Exit(1)
		}
	} else {
		ctrd.addr = address
	}

	waitCtx, waitCancel := context.WithTimeout(ctx, 30*time.Second)
	client, err := ctrd.waitForStart(waitCtx)
	waitCancel()
	if err != nil {
		ctrd.Kill()
		ctrd.Wait()
		fmt.Fprintf(os.Stderr, "containerd did not start in time: %v\n%s\n", err, buf.String())
		os.Exit(1)
	}

	v, err := client.Version(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get containerd version: %v\n", err)
		os.Exit(1)
	}
	log.G(ctx).WithFields(log.Fields{
		"version":  v.Version,
		"revision": v.Revision,
	}).Info("EROFS integration suite running against containerd")

	// Set the namespace's default snapshotter so pull+unpack tests
	// pick up the right one without extra flags.
	snapshotter := defaults.DefaultSnapshotter
	nsCtx := namespaces.WithNamespace(ctx, testNamespace)
	if err := client.NamespaceService().SetLabel(nsCtx, testNamespace,
		defaults.DefaultSnapshotterNSLabel, snapshotter); err != nil {
		// Namespace may not exist yet — that's fine.
		log.G(ctx).WithError(err).Debug("could not set default snapshotter label")
	}
	testSnapshotter = snapshotter

	client.Close()

	status := m.Run()

	if !noDaemon {
		if err := ctrd.Stop(); err != nil {
			if err2 := ctrd.Kill(); err2 != nil {
				fmt.Fprintln(os.Stderr, "failed to kill containerd:", err2)
			}
		}
		ctrd.Wait()
		_ = os.RemoveAll(defaultRoot)
	}

	os.Exit(status)
}

// newTestClient opens a new containerd client for use in a single test.
// It is the caller's responsibility to call Close().
func newTestClient(t testing.TB) *containerd.Client {
	t.Helper()
	c, err := containerd.New(address)
	if err != nil {
		t.Fatalf("failed to create containerd client: %v", err)
	}
	return c
}
