package plugin

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"github.com/opentalon/opentalon/proto/pluginpb"
	"google.golang.org/grpc"
)

// gateway exposes one already-loaded plugin's Execute RPC to external
// callers over its own TCP listener. Core has never had an inbound
// PluginService surface — it is only ever a client of that proto (see
// client.go) — so a plugin meant to be called directly by something outside
// the orchestrator's LLM tool-call loop (e.g. a CI workflow) had no way in.
// The request is forwarded to the plugin byte-for-byte; the gateway does not
// inspect, gate, or rate-limit it, so auth is entirely the plugin's own
// concern (e.g. an API key carried as a regular Execute arg, as
// talooner-plugin does).
type gateway struct {
	pluginpb.UnimplementedPluginServiceServer
	name   string
	client *Client
	server *grpc.Server
	lis    net.Listener
}

func (g *gateway) Execute(ctx context.Context, req *pluginpb.ToolCallRequest) (*pluginpb.ToolResultResponse, error) {
	return g.client.ExecuteRaw(ctx, req)
}

// startGateway binds a gRPC server to port and starts forwarding Execute
// calls to client in the background. TLS is deliberately not handled here —
// this listener is meant to sit behind an ingress/proxy that terminates it,
// the same way talooner-plugin's own standalone TCP mode does.
func startGateway(name string, client *Client, port int) (*gateway, error) {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, fmt.Errorf("plugin gateway %s: listen :%d: %w", name, port, err)
	}

	g := &gateway{name: name, client: client, server: grpc.NewServer(), lis: lis}
	pluginpb.RegisterPluginServiceServer(g.server, g)

	go func() {
		if err := g.server.Serve(lis); err != nil {
			slog.Info("plugin gateway stopped", "component", "plugin-manager", "plugin", name, "error", err)
		}
	}()
	slog.Info("plugin gateway listening", "component", "plugin-manager", "plugin", name, "port", port)

	return g, nil
}

func (g *gateway) stop() {
	g.server.GracefulStop()
}

// addr returns the bound listener address, e.g. "[::]:50100" — useful in
// tests that start a gateway on port 0 and need to know which port it got.
func (g *gateway) addr() string {
	return g.lis.Addr().String()
}
