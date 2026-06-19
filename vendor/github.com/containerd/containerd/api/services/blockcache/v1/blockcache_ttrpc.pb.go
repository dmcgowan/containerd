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

// Hand-written ttrpc service stubs for the BlockCache service.
// Not auto-generated; avoids protoc-gen-go-ttrpc toolchain dependency.

package blockcache

import (
	"context"

	"github.com/containerd/ttrpc"
)

// ── Service interface (server side) ──────────────────────────────────────────

// TTRPCBlockCacheService is the server-side interface for BlockCache.
type TTRPCBlockCacheService interface {
	Fill(context.Context, TTRPCBlockCache_FillServer) error
}

// TTRPCBlockCache_FillServer is the server-side stream for the Fill RPC.
type TTRPCBlockCache_FillServer interface {
	Send(*FillMessage) error
	Recv() (*FillMessage, error)
	ttrpc.StreamServer
}

type ttrpcblockcacheFillServer struct {
	ttrpc.StreamServer
}

func (x *ttrpcblockcacheFillServer) Send(m *FillMessage) error {
	return x.StreamServer.SendMsg(m)
}

func (x *ttrpcblockcacheFillServer) Recv() (*FillMessage, error) {
	m := new(FillMessage)
	if err := x.StreamServer.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}

// RegisterTTRPCBlockCacheService registers svc with the ttrpc server.
func RegisterTTRPCBlockCacheService(srv *ttrpc.Server, svc TTRPCBlockCacheService) {
	srv.RegisterService("containerd.services.blockcache.v1.BlockCache", &ttrpc.ServiceDesc{
		Streams: map[string]ttrpc.Stream{
			"Fill": {
				Handler: func(ctx context.Context, stream ttrpc.StreamServer) (interface{}, error) {
					return nil, svc.Fill(ctx, &ttrpcblockcacheFillServer{stream})
				},
				StreamingClient: true,
				StreamingServer: true,
			},
		},
	})
}

// ── Client interface ──────────────────────────────────────────────────────────

// TTRPCBlockCacheClient is the client-side interface for BlockCache.
type TTRPCBlockCacheClient interface {
	Fill(context.Context) (TTRPCBlockCache_FillClient, error)
}

// TTRPCBlockCache_FillClient is the client-side stream for the Fill RPC.
type TTRPCBlockCache_FillClient interface {
	Send(*FillMessage) error
	Recv() (*FillMessage, error)
	ttrpc.ClientStream
}

type ttrpcblockcacheClient struct {
	client *ttrpc.Client
}

// NewTTRPCBlockCacheClient returns a new client for the BlockCache service.
func NewTTRPCBlockCacheClient(client *ttrpc.Client) TTRPCBlockCacheClient {
	return &ttrpcblockcacheClient{client: client}
}

func (c *ttrpcblockcacheClient) Fill(ctx context.Context) (TTRPCBlockCache_FillClient, error) {
	stream, err := c.client.NewStream(ctx, &ttrpc.StreamDesc{
		StreamingClient: true,
		StreamingServer: true,
	}, "containerd.services.blockcache.v1.BlockCache", "Fill", nil)
	if err != nil {
		return nil, err
	}
	return &ttrpcblockcacheFillClient{stream}, nil
}

type ttrpcblockcacheFillClient struct {
	ttrpc.ClientStream
}

func (x *ttrpcblockcacheFillClient) Send(m *FillMessage) error {
	return x.ClientStream.SendMsg(m)
}

func (x *ttrpcblockcacheFillClient) Recv() (*FillMessage, error) {
	m := new(FillMessage)
	if err := x.ClientStream.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}
