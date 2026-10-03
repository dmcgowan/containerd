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

package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"

	bootapi "github.com/containerd/containerd/api/runtime/bootstrap/v1"
	"github.com/containerd/errdefs"
	"github.com/containerd/log"
	"github.com/containerd/otelttrpc"
	"github.com/containerd/ttrpc"

	"github.com/containerd/containerd/v2/core/runtime"
	"github.com/containerd/containerd/v2/pkg/atomicfile"
	"github.com/containerd/containerd/v2/pkg/dialer"
	"github.com/containerd/containerd/v2/pkg/protobuf/proto"
	client "github.com/containerd/containerd/v2/pkg/shim"
	"github.com/containerd/containerd/v2/pkg/timeout"
)

const (
	loadTimeout     = "io.containerd.timeout.shim.load"
	cleanupTimeout  = "io.containerd.timeout.shim.cleanup"
	shutdownTimeout = "io.containerd.timeout.shim.shutdown"
)

func init() {
	timeout.Set(loadTimeout, 5*time.Second)
	timeout.Set(cleanupTimeout, 5*time.Second)
	timeout.Set(shutdownTimeout, 3*time.Second)
}

func loadShim(ctx context.Context, bundle *Bundle, onClose func()) (_ ShimInstance, retErr error) {
	shimCtx, cancelShimLog := context.WithCancel(ctx)
	defer func() {
		if retErr != nil {
			cancelShimLog()
		}
	}()
	f, err := openShimLog(shimCtx, bundle, client.AnonReconnectDialer)
	if err != nil {
		return nil, fmt.Errorf("open shim log pipe when reload: %w", err)
	}
	defer func() {
		if retErr != nil {
			f.Close()
		}
	}()
	// open the log pipe and block until the writer is ready
	// this helps with synchronization of the shim
	// copy the shim's logs to containerd's output
	go func() {
		defer f.Close()
		_, err := io.Copy(os.Stderr, f)
		// To prevent flood of error messages, the expected error
		// should be reset, like os.ErrClosed or os.ErrNotExist, which
		// depends on platform.
		err = checkCopyShimLogError(shimCtx, err)
		if err != nil {
			log.G(shimCtx).WithError(err).Error("copy shim log after reload")
		}
	}()
	onCloseWithShimLog := func() {
		onClose()
		cancelShimLog()
		f.Close()
	}

	params, err := restoreBootstrapParams(bundle.Path)
	if err != nil {
		return nil, fmt.Errorf("failed to read bootstrap.json when restoring bundle %q: %w", bundle.ID, err)
	}

	conn, err := makeConnection(ctx, bundle.ID, params, onCloseWithShimLog, client.AnonReconnectDialer)
	if err != nil {
		return nil, fmt.Errorf("unable to make connection: %w", err)
	}

	defer func() {
		if retErr != nil {
			conn.Close()
		}
	}()

	// The address is in the form like ttrpc+unix://<uds-path> or grpc+vsock://<cid>:<port>
	address := fmt.Sprintf("%s+%s", params.Protocol, params.Address)

	shim := &shim{
		bundle:    bundle,
		client:    conn,
		address:   address,
		version:   int(params.Version),
		bootstrap: params,
	}

	return shim, nil
}

// CurrentShimVersion is the latest shim version supported by containerd (e.g. TaskService v3).
const CurrentShimVersion = 3

// ShimInstance represents running shim process managed by ShimManager.
type ShimInstance interface {
	io.Closer

	// ID of the shim.
	ID() string
	// Namespace of this shim.
	Namespace() string
	// Bundle is a file system path to shim's bundle.
	Bundle() string
	// Client returns the underlying TTRPC or GRPC client object for this shim.
	// The underlying object can be either *ttrpc.Client or grpc.ClientConnInterface.
	Client() any
	// Delete will close the client and remove bundle from disk.
	Delete(ctx context.Context) error
	// Endpoint returns shim's endpoint information,
	// including address and version.
	Endpoint() (string, int)
}

// ShimCapabilities is implemented by shim instances that retain what the shim
// advertised when it started.
//
// This is a separate interface rather than a method on [ShimInstance] so that
// external implementations of ShimInstance keep compiling; a shim instance
// that does not implement it is treated as having advertised nothing.
type ShimCapabilities interface {
	// BootstrapResult returns the result the shim returned when it started,
	// carrying the extensions it advertised. It may be nil.
	BootstrapResult() *bootapi.BootstrapResult
}

// ClientVersionDowngrader is implemented by shim instances that can lower their
// task client API version.
type ClientVersionDowngrader interface {
	// Downgrade is to lower shim's client version.
	//
	// Assume there is a running pod created by containerd-shim-runc-v2 from v1.7.x.
	// After upgrading to v2.x, the containerd-shim-runc-v2 binary will support the
	// sandbox API, and calling `shim start` for the existing running pod will return
	// a version=3 address. However, that pod shim does not support the streaming IO API,
	// so we should downgrade the shim version.
	//
	// Additionally, if a container record was created with v1.7.x, it will not have
	// a SandboxID field in the metadata store. In the CRI case, this will cause
	// the new shim client to use the v3 protocol to send requests to a running shim
	// that still uses the v2 protocol, resulting in a failure to start.
	// In this case, we should also downgrade the shim version and retry.
	Downgrade() error
}

// DeleteResultRecorder is implemented by shim instances that cache the result of
// a successful delete, so that a Delete retried after a failed shutdown can
// still return the original exit. It is used by task-oriented callers; a shim
// instance that does not record a result returns nil from DeleteResult.
type DeleteResultRecorder interface {
	RecordDeleteResult(*runtime.Exit)
	DeleteResult() *runtime.Exit
}

func parseStartResponse(response []byte) (*bootapi.BootstrapResult, error) {
	var result bootapi.BootstrapResult

	if err := proto.Unmarshal(response, &result); err == nil {
		return &result, nil
	}

	// Fallback to legacy parsing for backward compatibility with legacy shims that return the address as a plain string or JSON.
	response = bytes.TrimSpace(response)

	// Decode into the whole message rather than a subset of its fields. A
	// bundle's bootstrap.json is written with encoding/json and read back
	// through here, so a field that is not decoded is silently lost on reload.
	params := &bootapi.BootstrapResult{}
	if err := json.Unmarshal(response, params); err != nil || params.Version < 2 {
		// Use TTRPC for legacy shims
		params = &bootapi.BootstrapResult{
			Address:  string(response),
			Protocol: "ttrpc",
			Version:  2,
		}
	}

	if params.Version > CurrentShimVersion {
		return nil, fmt.Errorf("unsupported shim version (%d): %w", params.Version, errdefs.ErrNotImplemented)
	}

	return params, nil
}

// writeBootstrapParams writes shim's bootstrap configuration (e.g. how to connect, version, etc).
func writeBootstrapParams(path string, params *bootapi.BootstrapResult) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}

	data, err := json.Marshal(&params)
	if err != nil {
		return err
	}

	f, err := atomicfile.New(path, 0o644)
	if err != nil {
		return err
	}

	_, err = f.Write(data)
	if err != nil {
		f.Cancel()
		return err
	}

	return f.Close()
}

func readBootstrapParams(path string) (*bootapi.BootstrapResult, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	return parseStartResponse(data)
}

// makeConnection creates a new TTRPC or GRPC connection using the address and
// protocol from params. Legacy plain-string or JSON bootstrap responses are
// normalized by parseStartResponse before calling this function.
// The dialer parameter controls connection behavior: use AnonDialer for newly
// started shims (retries if pipe doesn't exist yet) or AnonReconnectDialer for
// reconnecting to already-running shims (fails fast if pipe is missing).
func makeConnection(ctx context.Context, id string, params *bootapi.BootstrapResult, onClose func(), dialer func(string, time.Duration) (net.Conn, error)) (_ io.Closer, retErr error) {
	log.G(ctx).WithFields(log.Fields{
		"address":  params.Address,
		"protocol": params.Protocol,
		"version":  params.Version,
	}).Infof("connecting to shim %s", id)

	switch strings.ToLower(params.Protocol) {
	case "ttrpc":
		conn, err := client.Connect(params.Address, dialer)
		if err != nil {
			return nil, fmt.Errorf("failed to create TTRPC connection: %w", err)
		}
		defer func() {
			if retErr != nil {
				conn.Close()
			}
		}()

		return ttrpc.NewClient(
			conn,
			ttrpc.WithOnClose(onClose),
			ttrpc.WithUnaryClientInterceptor(otelttrpc.UnaryClientInterceptor()),
		), nil
	case "grpc":
		gopts := []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		}
		return grpcDialContext(params.Address, onClose, gopts...)
	default:
		return nil, fmt.Errorf("unexpected protocol: %q", params.Protocol)
	}
}

// grpcDialContext and the underlying grpcConn type exist solely
// so we can have something similar to ttrpc.WithOnClose to have
// a callback run when the connection is severed or explicitly closed.
func grpcDialContext(
	address string,
	onClose func(),
	gopts ...grpc.DialOption,
) (*grpcConn, error) {
	// If grpc.WithBlock is specified in gopts this causes the connection to block waiting for
	// a connection regardless of if the socket exists or has a listener when Dial begins. This
	// specific behavior of WithBlock is mostly undesirable for shims, as if the socket isn't
	// there when we go to load/connect there's likely an issue. However, getting rid of WithBlock is
	// also undesirable as we don't want the background connection behavior, we want to ensure
	// a connection before moving on. To bring this in line with the ttrpc connection behavior
	// lets do an initial dial to ensure the shims socket is actually available. stat wouldn't suffice
	// here as if the shim exited unexpectedly its socket may still be on the filesystem, but it'd return
	// ECONNREFUSED which grpc.DialContext will happily trudge along through for the full timeout.
	//
	// This is especially helpful on restart of containerd as if the shim died while containerd
	// was down, we end up waiting the full timeout.
	conn, err := net.DialTimeout("unix", address, time.Second*10)
	if err != nil {
		return nil, err
	}
	conn.Close()

	target := dialer.DialAddress(address)
	client, err := grpc.NewClient(target, gopts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create GRPC connection: %w", err)
	}

	done := make(chan struct{})
	go func() {
		gctx := context.Background()
		sourceState := connectivity.Ready
		for {
			if client.WaitForStateChange(gctx, sourceState) {
				state := client.GetState()
				if state == connectivity.Idle || state == connectivity.Shutdown {
					break
				}
				// Could be transient failure. Lets see if we can get back to a working
				// state.
				log.G(gctx).WithFields(log.Fields{
					"state": state,
					"addr":  target,
				}).Warn("shim grpc connection unexpected state")
				sourceState = state
			}
		}
		onClose()
		close(done)
	}()

	return &grpcConn{
		ClientConn:  client,
		onCloseDone: done,
	}, nil
}

type grpcConn struct {
	*grpc.ClientConn
	onCloseDone chan struct{}
}

func (gc *grpcConn) UserOnCloseWait(ctx context.Context) error {
	select {
	case <-gc.onCloseDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type shim struct {
	bundle  *Bundle
	client  any
	address string
	version int
	// bootstrap is what the shim advertised when it started. Retained whole
	// rather than decoded into fields here so that a new capability needs no
	// further plumbing through the shim instance.
	bootstrap *bootapi.BootstrapResult

	deleteExit atomic.Pointer[runtime.Exit]
}

var _ ShimInstance = (*shim)(nil)
var _ ClientVersionDowngrader = (*shim)(nil)
var _ ShimCapabilities = (*shim)(nil)
var _ DeleteResultRecorder = (*shim)(nil)

// BootstrapResult returns what the shim advertised when it started.
func (s *shim) BootstrapResult() *bootapi.BootstrapResult {
	return s.bootstrap
}

// ID of the shim/task
func (s *shim) ID() string {
	return s.bundle.ID
}

func (s *shim) Endpoint() (string, int) {
	return s.address, s.version
}

func (s *shim) Downgrade() error {
	if s.version >= CurrentShimVersion {
		s.version--
		return nil
	}
	return fmt.Errorf("unable to downgrade because shim version (%d) is lower than CurrentShimVersion (%d)",
		s.version, CurrentShimVersion)
}

func (s *shim) Namespace() string {
	return s.bundle.Namespace
}

func (s *shim) Bundle() string {
	return s.bundle.Path
}

func (s *shim) Client() any {
	return s.client
}

// Close closes the underlying client connection.
func (s *shim) Close() error {
	if ttrpcClient, ok := s.client.(*ttrpc.Client); ok {
		return ttrpcClient.Close()
	}

	if grpcClient, ok := s.client.(*grpcConn); ok {
		return grpcClient.Close()
	}

	return nil
}

func (s *shim) Delete(ctx context.Context) error {
	var result []error

	if ttrpcClient, ok := s.client.(*ttrpc.Client); ok {
		if err := ttrpcClient.Close(); err != nil {
			result = append(result, fmt.Errorf("failed to close ttrpc client: %w", err))
		}

		if err := ttrpcClient.UserOnCloseWait(ctx); err != nil {
			result = append(result, fmt.Errorf("close wait error: %w", err))
		}
	}

	if grpcClient, ok := s.client.(*grpcConn); ok {
		if err := grpcClient.Close(); err != nil {
			result = append(result, fmt.Errorf("failed to close grpc client: %w", err))
		}

		if err := grpcClient.UserOnCloseWait(ctx); err != nil {
			result = append(result, fmt.Errorf("close wait error: %w", err))
		}
	}

	if err := s.bundle.Delete(); err != nil {
		log.G(ctx).WithField("id", s.ID()).WithError(err).Error("failed to delete bundle")
		result = append(result, fmt.Errorf("failed to delete bundle: %w", err))
	}

	return errors.Join(result...)
}

// RecordDeleteResult caches the result of a successful delete. The value is
// copied both in and out so that neither the caller that recorded it nor a later
// retry can mutate the cached result.
func (s *shim) RecordDeleteResult(exit *runtime.Exit) {
	cached := *exit
	s.deleteExit.Store(&cached)
}

// DeleteResult returns a copy of the cached delete result, or nil if this
// containerd process has never deleted the shim successfully.
func (s *shim) DeleteResult() *runtime.Exit {
	cached := s.deleteExit.Load()
	if cached == nil {
		return nil
	}
	exit := *cached
	return &exit
}
