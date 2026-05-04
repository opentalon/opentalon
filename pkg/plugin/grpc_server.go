package plugin

import (
	"context"

	"github.com/opentalon/opentalon/proto/pluginpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

// grpcServer implements pluginpb.PluginServiceServer by delegating to a Handler.
type grpcServer struct {
	pluginpb.UnimplementedPluginServiceServer
	handler Handler
}

func (s *grpcServer) Init(_ context.Context, req *pluginpb.PluginInitRequest) (*emptypb.Empty, error) {
	if c, ok := s.handler.(Configurable); ok {
		if err := c.Configure(req.GetConfigJson()); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "configure: %v", err)
		}
	}
	return &emptypb.Empty{}, nil
}

func (s *grpcServer) Capabilities(_ context.Context, _ *emptypb.Empty) (*pluginpb.PluginCapabilities, error) {
	caps := s.handler.Capabilities()
	return capsToProto(caps), nil
}

func (s *grpcServer) Execute(_ context.Context, req *pluginpb.ToolCallRequest) (*pluginpb.ToolResultResponse, error) {
	resp := s.handler.Execute(requestFromProto(req))
	return responseToProto(resp), nil
}

func (s *grpcServer) OnRunComplete(_ context.Context, req *pluginpb.RunCompleteEvent) (*pluginpb.RunCompleteResponse, error) {
	obs, ok := s.handler.(RunObserver)
	if !ok {
		return &pluginpb.RunCompleteResponse{}, nil
	}
	entries := make([]ToolCallEntry, len(req.GetToolCalls()))
	for i, tc := range req.GetToolCalls() {
		entries[i] = ToolCallEntry{
			Plugin:        tc.Plugin,
			Action:        tc.Action,
			Args:          tc.Args,
			ResultContent: tc.ResultContent,
			ResultError:   tc.ResultError,
		}
	}
	obs.OnRunComplete(RunCompleteEvent{
		SessionID:   req.SessionId,
		ActorID:     req.ActorId,
		UserMessage: req.UserMessage,
		Response:    req.Response,
		ToolCalls:   entries,
	})
	return &pluginpb.RunCompleteResponse{}, nil
}
