package plugin

import (
	"context"
	"testing"
	"time"

	pkg "github.com/opentalon/opentalon/pkg/plugin"
	"github.com/opentalon/opentalon/proto/pluginpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// dialGateway connects to a gateway the way an external caller (e.g.
// talooner's CLI) does: a real TCP dial, no host process in between.
func dialGateway(t *testing.T, addr string) pluginpb.PluginServiceClient {
	t.Helper()
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return pluginpb.NewPluginServiceClient(cc)
}

func TestGatewayForwardsExecute(t *testing.T) {
	cc := startFakePluginServer(t)
	client := &Client{conn: cc, client: pluginpb.NewPluginServiceClient(cc)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.fetchCapabilities(ctx, ""); err != nil {
		t.Fatal(err)
	}

	gw, err := startGateway("echo", client, 0, NewManager(nil))
	if err != nil {
		t.Fatalf("startGateway: %v", err)
	}
	t.Cleanup(gw.stop)

	rpc := dialGateway(t, gw.addr())
	resp, err := rpc.Execute(ctx, &pluginpb.ToolCallRequest{Id: "g1", Args: map[string]string{"text": "hi"}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.Content != "echo: hi" {
		t.Errorf("content = %q, want %q", resp.Content, "echo: hi")
	}
}

// A gateway is a pure forward, not a validator — it must not swallow or
// reshape an error the plugin itself returned, since the plugin owns auth
// and argument validation for this path.
func TestGatewayForwardsPluginError(t *testing.T) {
	cc := startFakePluginServer(t)
	client := &Client{conn: cc, client: pluginpb.NewPluginServiceClient(cc)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.fetchCapabilities(ctx, ""); err != nil {
		t.Fatal(err)
	}

	gw, err := startGateway("echo", client, 0, NewManager(nil))
	if err != nil {
		t.Fatalf("startGateway: %v", err)
	}
	t.Cleanup(gw.stop)

	rpc := dialGateway(t, gw.addr())
	resp, err := rpc.Execute(ctx, &pluginpb.ToolCallRequest{Id: "g2", Args: map[string]string{}})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.Error != "missing text" {
		t.Errorf("error = %q, want %q", resp.Error, "missing text")
	}
}

// A plugin declaring SupportsCallbacks must be dispatched via ExecuteBidi
// through the gateway, with callbacks routed through the manager's wired
// CallbackHandler — this is the fix for talooner generate_ruleset always
// falling back (host == nil) when called through the external gateway.
func TestGatewayUsesExecuteBidiWhenSupported(t *testing.T) {
	body := func(ctx context.Context, req pkg.Request, host pkg.HostCaller) pkg.Response {
		r, err := host.RunAction(ctx, "inv", "list", map[string]string{"q": "x"})
		if err != nil {
			return pkg.Response{Error: err.Error()}
		}
		return pkg.Response{CallID: req.ID, Content: "ok: " + r.Content}
	}
	client := startBidiServer(t, body)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.fetchCapabilities(ctx, ""); err != nil {
		t.Fatal(err)
	}

	mgr := NewManager(nil)
	cb := &recordingCallbackHandler{
		response: func(plugin, action string, args map[string]string) (string, error) {
			return "42 found", nil
		},
	}
	mgr.SetCallbackHandler(cb)

	gw, err := startGateway("test", client, 0, mgr)
	if err != nil {
		t.Fatalf("startGateway: %v", err)
	}
	t.Cleanup(gw.stop)

	rpc := dialGateway(t, gw.addr())
	resp, err := rpc.Execute(ctx, &pluginpb.ToolCallRequest{Id: "g1", Plugin: "test", Action: "go"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.Content != "ok: 42 found" {
		t.Errorf("content = %q, want %q", resp.Content, "ok: 42 found")
	}
	if len(cb.calls) != 1 {
		t.Fatalf("expected 1 callback dispatched through gateway, got %d", len(cb.calls))
	}
	if cb.calls[0].Plugin != "inv" || cb.calls[0].Action != "list" {
		t.Errorf("callback dest: %+v", cb.calls[0])
	}
}

// Before SetCallbackHandler has run (startup ordering: gateways start while
// plugins load, before the orchestrator exists), a SupportsCallbacks plugin
// must still fall back to unary Execute rather than hang or error opaquely.
func TestGatewayFallsBackToUnaryWhenCallbackHandlerNotWired(t *testing.T) {
	body := func(ctx context.Context, req pkg.Request, host pkg.HostCaller) pkg.Response {
		t.Fatal("bidi body should not run when the gateway falls back to unary")
		return pkg.Response{}
	}
	client := startBidiServer(t, body)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.fetchCapabilities(ctx, ""); err != nil {
		t.Fatal(err)
	}

	gw, err := startGateway("test", client, 0, NewManager(nil))
	if err != nil {
		t.Fatalf("startGateway: %v", err)
	}
	t.Cleanup(gw.stop)

	rpc := dialGateway(t, gw.addr())
	resp, err := rpc.Execute(ctx, &pluginpb.ToolCallRequest{Id: "g1", Plugin: "test", Action: "go"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.Error != "unary not supported in test" {
		t.Errorf("error = %q, want the bidiStreamingHandler's unary-fallback error", resp.Error)
	}
}

func TestGatewayStopClosesListener(t *testing.T) {
	cc := startFakePluginServer(t)
	client := &Client{conn: cc, client: pluginpb.NewPluginServiceClient(cc)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.fetchCapabilities(ctx, ""); err != nil {
		t.Fatal(err)
	}

	gw, err := startGateway("echo", client, 0, NewManager(nil))
	if err != nil {
		t.Fatalf("startGateway: %v", err)
	}
	addr := gw.addr()
	gw.stop()

	rpc := dialGateway(t, addr)
	callCtx, callCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer callCancel()
	if _, err := rpc.Execute(callCtx, &pluginpb.ToolCallRequest{Id: "g3"}); err == nil {
		t.Error("Execute after stop: want error, got nil")
	}
}
