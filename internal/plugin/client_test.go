package plugin

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/orchestrator"
	"github.com/opentalon/opentalon/proto/pluginpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"
)

const bufSize = 1024 * 1024

// fakePluginService is a gRPC server-side implementation for tests.
type fakePluginService struct {
	pluginpb.UnimplementedPluginServiceServer
}

func (s *fakePluginService) Init(_ context.Context, _ *pluginpb.PluginInitRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (s *fakePluginService) Capabilities(_ context.Context, _ *emptypb.Empty) (*pluginpb.PluginCapabilities, error) {
	return &pluginpb.PluginCapabilities{
		Name:        "echo",
		Description: "Echoes arguments back",
		Actions: []*pluginpb.Action{
			{
				Name:        "say",
				Description: "Echo a message",
				Parameters: []*pluginpb.Parameter{
					{Name: "text", Description: "Text to echo", Type: "string", Required: true},
				},
			},
		},
	}, nil
}

func (s *fakePluginService) Execute(_ context.Context, req *pluginpb.ToolCallRequest) (*pluginpb.ToolResultResponse, error) {
	text := req.Args["text"]
	if text == "" {
		return &pluginpb.ToolResultResponse{CallId: req.Id, Error: "missing text"}, nil
	}
	return &pluginpb.ToolResultResponse{CallId: req.Id, Content: "echo: " + text}, nil
}

func startFakePluginServer(t *testing.T) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	pluginpb.RegisterPluginServiceServer(srv, &fakePluginService{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { srv.Stop() })

	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return cc
}

func newTestPluginClient(t *testing.T) *Client {
	t.Helper()
	cc := startFakePluginServer(t)
	c := &Client{
		conn:   cc,
		client: pluginpb.NewPluginServiceClient(cc),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.fetchCapabilities(ctx, ""); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestClientDialAndCapabilities(t *testing.T) {
	client := newTestPluginClient(t)
	defer func() { _ = client.Close() }()

	if client.Name() != "echo" {
		t.Errorf("name = %q, want echo", client.Name())
	}

	cap := client.Capability()
	if cap.Description != "Echoes arguments back" {
		t.Errorf("description = %q", cap.Description)
	}
	if len(cap.Actions) != 1 {
		t.Fatalf("actions = %d", len(cap.Actions))
	}
	if cap.Actions[0].Name != "say" {
		t.Errorf("action name = %q", cap.Actions[0].Name)
	}
	if len(cap.Actions[0].Parameters) != 1 {
		t.Fatalf("params = %d", len(cap.Actions[0].Parameters))
	}
	if !cap.Actions[0].Parameters[0].Required {
		t.Error("text param should be required")
	}
}

func TestClientExecute(t *testing.T) {
	client := newTestPluginClient(t)
	defer func() { _ = client.Close() }()

	result := client.Execute(orchestrator.ToolCall{
		ID:     "c1",
		Plugin: "echo",
		Action: "say",
		Args:   map[string]string{"text": "hello world"},
	})

	if result.Error != "" {
		t.Fatalf("unexpected error: %s", result.Error)
	}
	if result.CallID != "c1" {
		t.Errorf("call_id = %q", result.CallID)
	}
	if result.Content != "echo: hello world" {
		t.Errorf("content = %q", result.Content)
	}
}

func TestClientExecuteError(t *testing.T) {
	client := newTestPluginClient(t)
	defer func() { _ = client.Close() }()

	result := client.Execute(orchestrator.ToolCall{
		ID:     "c2",
		Plugin: "echo",
		Action: "say",
		Args:   map[string]string{},
	})

	if result.Error != "missing text" {
		t.Errorf("error = %q, want 'missing text'", result.Error)
	}
}

func TestClientMultipleCalls(t *testing.T) {
	client := newTestPluginClient(t)
	defer func() { _ = client.Close() }()

	for i := 0; i < 10; i++ {
		result := client.Execute(orchestrator.ToolCall{
			ID:     "multi",
			Plugin: "echo",
			Action: "say",
			Args:   map[string]string{"text": "ping"},
		})
		if result.Content != "echo: ping" {
			t.Fatalf("call %d: content = %q", i, result.Content)
		}
	}
}

// legacyPluginService does NOT implement Init — simulates plugins built
// against an older SDK (before Init was added).
type legacyPluginService struct {
	pluginpb.UnimplementedPluginServiceServer
}

func (s *legacyPluginService) Capabilities(_ context.Context, _ *emptypb.Empty) (*pluginpb.PluginCapabilities, error) {
	return &pluginpb.PluginCapabilities{
		Name:        "legacy",
		Description: "Plugin without Init",
		Actions: []*pluginpb.Action{
			{Name: "ping", Description: "Ping"},
		},
	}, nil
}

func (s *legacyPluginService) Execute(_ context.Context, req *pluginpb.ToolCallRequest) (*pluginpb.ToolResultResponse, error) {
	return &pluginpb.ToolResultResponse{CallId: req.Id, Content: "pong"}, nil
}

func TestClientRejectsPluginWithoutInit(t *testing.T) {
	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	pluginpb.RegisterPluginServiceServer(srv, &legacyPluginService{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { srv.Stop() })

	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
			return lis.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })

	c := &Client{conn: cc, client: pluginpb.NewPluginServiceClient(cc)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = c.fetchCapabilities(ctx, "")
	if err == nil {
		t.Fatal("expected error for plugin without Init")
	}
	if got := err.Error(); !strings.Contains(got, "does not implement Init") {
		t.Errorf("error = %q, want message about missing Init", got)
	}
}

func TestClientDialFailure(t *testing.T) {
	_, err := Dial("unix", "/nonexistent/plugin.sock", defaultDialTimeout, "")
	// gRPC NewClient is lazy, so dial failure happens on first RPC (fetchCapabilities).
	if err == nil {
		t.Error("expected error for nonexistent socket")
	}
}

func TestUserOnlyMappedFromProto(t *testing.T) {
	// Verify that user_only=true in the proto Action is carried through to orchestrator.Action.
	pb := &pluginpb.PluginCapabilities{
		Name:        "myplugin",
		Description: "Test plugin",
		Actions: []*pluginpb.Action{
			{Name: "public_action", Description: "Public", UserOnly: false},
			{Name: "admin_action", Description: "Admin only", UserOnly: true},
		},
	}
	cap := toPluginCapability(pb)
	if len(cap.Actions) != 2 {
		t.Fatalf("expected 2 actions, got %d", len(cap.Actions))
	}
	if cap.Actions[0].UserOnly {
		t.Error("public_action should have UserOnly=false")
	}
	if !cap.Actions[1].UserOnly {
		t.Error("admin_action should have UserOnly=true")
	}
}

func TestManagerLoadAndUnload(t *testing.T) {
	registry := orchestrator.NewToolRegistry()

	cc := startFakePluginServer(t)
	client := &Client{
		conn:   cc,
		client: pluginpb.NewPluginServiceClient(cc),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.fetchCapabilities(ctx, ""); err != nil {
		t.Fatal(err)
	}

	cap := client.Capability()
	if err := registry.Register(cap, client); err != nil {
		t.Fatal(err)
	}

	exec, ok := registry.GetExecutor("echo")
	if !ok {
		t.Fatal("echo not in registry")
	}

	result := exec.Execute(orchestrator.ToolCall{
		ID: "m1", Plugin: "echo", Action: "say",
		Args: map[string]string{"text": "from manager"},
	})
	if result.Content != "echo: from manager" {
		t.Errorf("content = %q", result.Content)
	}

	registry.Deregister("echo")
	_ = client.Close()

	_, ok = registry.GetExecutor("echo")
	if ok {
		t.Error("echo should be deregistered")
	}
}
