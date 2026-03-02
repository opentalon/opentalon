package channel

import (
	"context"
	"fmt"

	"github.com/opentalon/opentalon/pkg/channel/channelpb"
	"google.golang.org/protobuf/types/known/emptypb"
)

// grpcServer implements channelpb.ChannelServiceServer by delegating to a Channel.
type grpcServer struct {
	channelpb.UnimplementedChannelServiceServer
	ch Channel
}

func (s *grpcServer) Capabilities(_ context.Context, _ *emptypb.Empty) (*channelpb.ChannelCapabilities, error) {
	caps := s.ch.Capabilities()
	return capabilitiesToProto(caps), nil
}

func (s *grpcServer) Configure(_ context.Context, req *channelpb.ConfigureRequest) (*channelpb.ConfigureResponse, error) {
	cc, ok := s.ch.(ConfigurableChannel)
	if !ok {
		// Channels that don't implement ConfigurableChannel silently accept configure.
		return &channelpb.ConfigureResponse{}, nil
	}
	cfg := configFromStruct(req.GetConfig())
	if err := cc.Configure(cfg); err != nil {
		return nil, fmt.Errorf("configure: %w", err)
	}
	return &channelpb.ConfigureResponse{}, nil
}

func (s *grpcServer) Tools(_ context.Context, _ *emptypb.Empty) (*channelpb.ToolsResponse, error) {
	tp, ok := s.ch.(ToolProvider)
	if !ok {
		return &channelpb.ToolsResponse{}, nil
	}
	return &channelpb.ToolsResponse{Tools: toolsToProto(tp.Tools())}, nil
}

func (s *grpcServer) Start(_ *emptypb.Empty, stream channelpb.ChannelService_StartServer) error {
	inbox := make(chan InboundMessage, 32)
	if err := s.ch.Start(stream.Context(), inbox); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	for {
		select {
		case msg, ok := <-inbox:
			if !ok {
				return nil
			}
			if err := stream.Send(inboundToProto(msg)); err != nil {
				return err
			}
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
}

func (s *grpcServer) Send(ctx context.Context, msg *channelpb.OutboundMessage) (*channelpb.SendResponse, error) {
	if err := s.ch.Send(ctx, outboundFromProto(msg)); err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}
	return &channelpb.SendResponse{}, nil
}
