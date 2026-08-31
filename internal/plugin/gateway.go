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
	mgr    *Manager
	server *grpc.Server
	lis    net.Listener
}

// Execute dispatches over ExecuteBidi when the plugin declares
// SupportsCallbacks and the host's CallbackHandler is wired. Without that, a
// plugin needing to call back into the host mid-request would always see
// host == nil, since plain unary Execute has no callback channel. This
// branch can only see cb == nil if it races startServing (see there) — in
// steady state the handler is always set before Serve runs.
func (g *gateway) Execute(ctx context.Context, req *pluginpb.ToolCallRequest) (*pluginpb.ToolResultResponse, error) {
	if g.client.Capability().SupportsCallbacks {
		if cb := g.mgr.callbackHandler(); cb != nil {
			return g.client.ExecuteBidiRaw(ctx, req, cb)
		}
		slog.Warn("plugin gateway: callback handler not wired yet, falling back to unary Execute",
			"component", "plugin-manager", "plugin", g.name)
	}
	return g.client.ExecuteRaw(ctx, req)
}

// startGateway binds a gRPC server to port and registers the handler, but
// does NOT start accepting connections — the caller (Manager.loadLocked /
// Manager.SetCallbackHandler) decides when startServing runs, so that no
// gateway serves a single request before the host's CallbackHandler is
// wired (see startServing). The listener is still bound here, synchronously,
// so the port is claimed and load errors surface immediately, same as
// before.
func startGateway(name string, client *Client, port int, mgr *Manager) (*gateway, error) {
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return nil, fmt.Errorf("plugin gateway %s: listen :%d: %w", name, port, err)
	}

	g := &gateway{name: name, client: client, mgr: mgr, server: grpc.NewServer(), lis: lis}
	pluginpb.RegisterPluginServiceServer(g.server, g)

	return g, nil
}

// startServing begins accepting connections in the background. Called
// exactly once per gateway, either immediately (Manager.loadLocked, when
// the CallbackHandler is already set — true for any plugin loaded after
// startup, e.g. via the retry loop or Reload) or deferred until
// Manager.SetCallbackHandler runs (true for every gateway started during
// the initial LoadAll, since that happens before the orchestrator —  and
// therefore the CallbackHandler — exists). Either way, Execute never sees a
// request before g.mgr.callbackHandler() can return non-nil for a
// SupportsCallbacks plugin.
func (g *gateway) startServing() {
	go func() {
		if err := g.server.Serve(g.lis); err != nil {
			slog.Info("plugin gateway stopped", "component", "plugin-manager", "plugin", g.name, "error", err)
		}
	}()
	slog.Info("plugin gateway listening", "component", "plugin-manager", "plugin", g.name, "port", g.lis.Addr())
}

func (g *gateway) stop() {
	g.server.GracefulStop()
	// GracefulStop only closes listeners the server has actually Serve'd.
	// A gateway stopped while still in Manager.pendingGateways (startServing
	// never ran) would otherwise leak its bound port — the next Load of the
	// same plugin/port would then fail with "address already in use".
	_ = g.lis.Close()
}

// addr returns the bound listener address, e.g. "[::]:50100" — useful in
// tests that start a gateway on port 0 and need to know which port it got.
func (g *gateway) addr() string {
	return g.lis.Addr().String()
}
