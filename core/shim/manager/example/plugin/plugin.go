//go:build linux

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

// Package plugin implements an example managed proxy plugin shim: a gRPC
// snapshotter served through the shim bootstrap protocol. containerd starts it
// via a [proxy_plugins] entry whose "shim" field names this binary, connects to
// the gRPC address it advertises, and registers it as a snapshotter.
//
// It demonstrates what a managed proxy plugin shim must do:
//
//   - On "start": read BootstrapParams from stdin, daemonize a gRPC server on a
//     unix socket in the bundle, and write a BootstrapResult (protocol "grpc",
//     the socket address, and an optional containerd.types.PluginInfo extension)
//     to stdout.
//   - On "delete": tear down the bundle's socket and write a DeleteResponse.
//   - With no action (the daemonized child): serve gRPC until signaled.
//
// It is intentionally small; a production plugin would add logging, readiness
// signaling and graceful shutdown.
package plugin

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	bootapi "github.com/containerd/containerd/api/runtime/bootstrap/v1"
	task "github.com/containerd/containerd/api/runtime/task/v3"
	snapshotsapi "github.com/containerd/containerd/api/services/snapshots/v1"
	"github.com/containerd/containerd/api/types"

	"github.com/containerd/containerd/v2/contrib/snapshotservice"
	"github.com/containerd/containerd/v2/pkg/protobuf"
	"github.com/containerd/containerd/v2/plugins/snapshots/native"
)

const socketName = "plugin.sock"

// Run is the entry point for the example plugin shim binary. It dispatches on
// the bootstrap action passed as the final argument ("start", "delete", or empty
// for the long-running server).
func Run() {
	flag.String("namespace", "", "namespace (ignored)")
	flag.String("id", "", "plugin id")
	flag.String("address", "", "containerd address (ignored)")
	flag.String("publish-binary", "", "publish binary (ignored)")
	flag.Bool("debug", false, "debug (ignored)")
	flag.Parse()
	action := flag.Arg(0)

	var err error
	switch action {
	case "start":
		err = runStart()
	case "delete":
		err = runDelete()
	default:
		err = runServe()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// runStart daemonizes the server and reports how to reach it.
func runStart() error {
	// Drain and ignore the BootstrapParams; this example needs nothing from it.
	// A real shim would decode it for configuration and extensions.
	_, _ = os.Stdin.Read(make([]byte, 0))

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}

	cmd := exec.Command(self)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start plugin daemon: %w", err)
	}
	// Detach the daemon from this short-lived helper.
	_ = cmd.Process.Release()

	address := filepath.Join(cwd, socketName)

	// Wait for the daemon to create its socket before telling containerd where
	// to connect, so the first dial does not race the server coming up.
	if err := awaitSocket(address); err != nil {
		return err
	}

	result := &bootapi.BootstrapResult{
		Version:  3,
		Address:  address,
		Protocol: "grpc",
	}
	// Advertise what this plugin can do, so containerd registers it with the
	// right exports/capabilities without them being set in configuration.
	info := &types.PluginInfo{
		Exports:      map[string]string{"example": "true"},
		Capabilities: []string{"example-snapshotter"},
	}
	if err := result.AddExtension(info); err != nil {
		return err
	}

	data, err := proto.Marshal(result)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(data)
	return err
}

// runDelete removes the bundle's socket and reports a (zero) exit.
func runDelete() error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(cwd, socketName))

	data, err := proto.Marshal(&task.DeleteResponse{
		ExitedAt: protobuf.ToTimestamp(time.Now()),
	})
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(data)
	return err
}

// runServe is the long-running daemon: it serves the snapshotter gRPC API on the
// bundle socket until it receives SIGTERM/SIGINT.
func runServe() error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	address := filepath.Join(cwd, socketName)
	_ = os.Remove(address)

	sn, err := native.NewSnapshotter(filepath.Join(cwd, "snapshots"))
	if err != nil {
		return err
	}

	l, err := net.Listen("unix", address)
	if err != nil {
		return err
	}

	srv := grpc.NewServer()
	snapshotsapi.RegisterSnapshotsServer(srv, snapshotservice.FromSnapshotter(sn))

	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		<-ch
		srv.GracefulStop()
	}()

	return serve(context.Background(), srv, l)
}

func serve(_ context.Context, srv *grpc.Server, l net.Listener) error {
	defer l.Close()
	return srv.Serve(l)
}

// awaitSocket blocks until address is connectable or a short deadline elapses.
func awaitSocket(address string) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", address, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("plugin socket %q not ready", address)
}
