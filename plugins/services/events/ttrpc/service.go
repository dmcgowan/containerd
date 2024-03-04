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

package eventsttrpc

import (
	"context"
	"fmt"

	api "github.com/containerd/containerd/v2/api/services/events/v1"
	"github.com/containerd/containerd/v2/api/types"
	"github.com/containerd/containerd/v2/core/events"
	"github.com/containerd/containerd/v2/core/events/exchange"
	"github.com/containerd/containerd/v2/plugins"
	"github.com/containerd/containerd/v2/protobuf"
	ptypes "github.com/containerd/containerd/v2/protobuf/types"
	"github.com/containerd/errdefs"
	"github.com/containerd/plugin"
	"github.com/containerd/plugin/registry"
	"github.com/containerd/ttrpc"
)

func init() {
	registry.Register(&plugin.Registration{
		Type: plugins.TTRPCPlugin,
		ID:   "events",
		Requires: []plugin.Type{
			plugins.EventPlugin,
		},
		InitFn: func(ic *plugin.InitContext) (interface{}, error) {
			ep, err := ic.GetByID(plugins.EventPlugin, "exchange")
			if err != nil {
				return nil, err
			}
			return NewService(ep.(*exchange.Exchange)), nil
		},
	})
}

type service struct {
	events *exchange.Exchange
}

// NewService returns the TTRPC events server
func NewService(events *exchange.Exchange) interface{} {
	return &service{
		events: events,
	}
}

func (s *service) RegisterTTRPC(server *ttrpc.Server) error {
	api.RegisterTTRPCEventsService(server, s)
	return nil
}

func (s *service) Publish(ctx context.Context, r *api.PublishRequest) (*ptypes.Empty, error) {
	if err := s.events.Publish(ctx, r.Topic, r.Event); err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	return &ptypes.Empty{}, nil
}

func (s *service) Forward(ctx context.Context, r *api.ForwardRequest) (*ptypes.Empty, error) {
	if err := s.events.Forward(ctx, fromProto(r.Envelope)); err != nil {
		return nil, errdefs.ToGRPC(err)
	}

	return &ptypes.Empty{}, nil
}

func (s *service) Subscribe(ctx context.Context, req *api.SubscribeRequest, srv api.TTRPCEvents_SubscribeServer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	eventq, errq := s.events.Subscribe(ctx, req.Filters...)
	for {
		select {
		case ev := <-eventq:
			if err := srv.Send(toProto(ev)); err != nil {
				return fmt.Errorf("failed sending event to subscriber: %w", err)
			}
		case err := <-errq:
			if err != nil {
				return fmt.Errorf("subscription error: %w", err)
			}

			return nil
		}
	}
}

func toProto(env *events.Envelope) *types.Envelope {
	return &types.Envelope{
		Timestamp: protobuf.ToTimestamp(env.Timestamp),
		Namespace: env.Namespace,
		Topic:     env.Topic,
		Event:     protobuf.FromAny(env.Event),
	}
}

func fromProto(env *types.Envelope) *events.Envelope {
	return &events.Envelope{
		Timestamp: protobuf.FromTimestamp(env.Timestamp),
		Namespace: env.Namespace,
		Topic:     env.Topic,
		Event:     env.Event,
	}
}
