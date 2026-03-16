package plugin

import (
	"context"
	"log"

	"github.com/opentalon/opentalon/proto/pluginpb"
)

// grpcServer implements pluginpb.PluginServiceServer by delegating to a Handler.
type grpcServer struct {
	pluginpb.UnimplementedPluginServiceServer
	handler Handler
}

func (s *grpcServer) Capabilities(_ context.Context, req *pluginpb.PluginInitRequest) (*pluginpb.PluginCapabilities, error) {
	if c, ok := s.handler.(Configurable); ok {
		if err := c.Configure(req.GetConfigJson()); err != nil {
			log.Printf("plugin: configure: %v", err)
		}
	}
	caps := s.handler.Capabilities()
	return capsToProto(caps), nil
}

func (s *grpcServer) Execute(_ context.Context, req *pluginpb.ToolCallRequest) (*pluginpb.ToolResultResponse, error) {
	resp := s.handler.Execute(requestFromProto(req))
	return responseToProto(resp), nil
}
