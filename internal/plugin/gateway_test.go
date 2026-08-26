package plugin

import (
	"context"
	"testing"
	"time"

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

	gw, err := startGateway("echo", client, 0)
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

	gw, err := startGateway("echo", client, 0)
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

func TestGatewayStopClosesListener(t *testing.T) {
	cc := startFakePluginServer(t)
	client := &Client{conn: cc, client: pluginpb.NewPluginServiceClient(cc)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.fetchCapabilities(ctx, ""); err != nil {
		t.Fatal(err)
	}

	gw, err := startGateway("echo", client, 0)
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
