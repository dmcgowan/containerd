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

//go:build linux

package blockmount

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	blockcachev1 "github.com/containerd/containerd/api/services/blockcache/v1"
	"github.com/containerd/ttrpc"
)

const dialTimeout = 10 * time.Second

// newBlockCacheClient dials the daemon's ttrpc socket and returns a
// TTRPCBlockCacheClient.  The socket address format mirrors what
// pkg/ttrpcutil.NewClient uses: typically "unix:///run/containerd/...".
func newBlockCacheClient(ctx context.Context, address string) (blockcachev1.TTRPCBlockCacheClient, error) {
	conn, err := dialTTRPC(ctx, address)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", address, err)
	}
	client := ttrpc.NewClient(conn)
	return blockcachev1.NewTTRPCBlockCacheClient(client), nil
}

func dialTTRPC(ctx context.Context, address string) (net.Conn, error) {
	// Strip scheme if present (e.g. "unix:///path" or "/path").
	path := address
	if s, ok := strings.CutPrefix(address, "unix://"); ok {
		path = s
	} else if s, ok := strings.CutPrefix(address, "unix:"); ok {
		path = s
	}

	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	d := net.Dialer{}
	return d.DialContext(dialCtx, "unix", path)
}
