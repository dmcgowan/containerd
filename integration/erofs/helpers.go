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

// Package erofs provides an integration test suite for the EROFS image format
// and the EROFS snapshotter.
//
// The suite mirrors the structure of integration/client/ but is dedicated to
// EROFS-specific behaviour.  It covers:
//
//   - Media-type constant correctness (all platforms)
//   - Image conversion via converter/erofs (Linux)
//   - OCI archive round-trips (Linux)
//   - Snapshotter unpack and mount operations (Linux, requires root + kernel module)
//
// To run the full suite against a freshly started daemon:
//
//	go test ./integration/erofs/... -v
//
// To run against an already-running daemon (skip daemon start):
//
//	go test ./integration/erofs/... -no-daemon -address /run/containerd/containerd.sock
//
// EROFS snapshotter tests additionally require:
//   - Running as root (-test.root)
//   - EROFS kernel module loaded (modprobe erofs)
package erofs

import (
	"context"
	"flag"
	"fmt"
	"os"
	"testing"

	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/log/logtest"
)

const testNamespace = "erofs-testing"

var (
	address           string
	ctrdStdioFilePath string
	testSnapshotter   = defaults.DefaultSnapshotter
	ctrd              = &daemon{}
	noDaemon          bool
)

func init() {
	flag.StringVar(&address, "address", defaultAddress,
		"containerd socket address for EROFS integration tests")
	flag.BoolVar(&noDaemon, "no-daemon", false,
		"connect to an already-running containerd instead of starting one")
}

// testContext returns a context with the erofs testing namespace and a
// per-test logger attached.
func testContext(t testing.TB) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background()) //nolint:all
	ctx = namespaces.WithNamespace(ctx, testNamespace)
	if t != nil {
		ctx = logtest.WithT(ctx, t)
	}
	return ctx, cancel
}

// isNetworkError returns true when the error looks like a transient network
// problem (dial failure, DNS lookup failure, etc.).  Used to skip tests that
// require a remote registry in offline environments.
func isNetworkError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, frag := range []string{
		"dial tcp", "connection refused", "no such host",
		"network unreachable", "timeout", "EOF",
		"TLS handshake timeout",
	} {
		if containsFrag(msg, frag) {
			return true
		}
	}
	return false
}

func containsFrag(s, sub string) bool {
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// isErofsMediaTypePrefix returns true for any media type starting with
// "application/vnd.erofs".  This is an inline copy of
// erofsutils.IsErofsMediaType that avoids the Linux-only dm-verity transitive
// dependency, keeping this file compilable on all platforms.
func isErofsMediaTypePrefix(mt string) bool {
	const prefix = "application/vnd.erofs"
	return len(mt) >= len(prefix) && mt[:len(prefix)] == prefix
}

// createConfig writes a minimal containerd config to a temp file and returns
// its path.  The caller is responsible for removing it.
func createConfig() (string, error) {
	f, err := os.CreateTemp("", "containerd-erofs-config-")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "version = 2\n"); err != nil {
		return "", err
	}
	return f.Name(), nil
}
