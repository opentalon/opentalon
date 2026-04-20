package plugin

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/orchestrator"
	"github.com/opentalon/opentalon/internal/profile"
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

	result := client.Execute(context.Background(), orchestrator.ToolCall{
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

	result := client.Execute(context.Background(), orchestrator.ToolCall{
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
		result := client.Execute(context.Background(), orchestrator.ToolCall{
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

func TestInjectContextArgsMappedFromProto(t *testing.T) {
	pb := &pluginpb.PluginCapabilities{
		Name:        "myplugin",
		Description: "Test plugin",
		Actions: []*pluginpb.Action{
			{Name: "save_cred", Description: "Save", InjectContextArgs: []string{"actor_id"}},
			{Name: "navigate", Description: "Nav"},
		},
	}
	cap := toPluginCapability(pb)
	if len(cap.Actions) != 2 {
		t.Fatalf("expected 2 actions, got %d", len(cap.Actions))
	}
	if len(cap.Actions[0].InjectContextArgs) != 1 || cap.Actions[0].InjectContextArgs[0] != "actor_id" {
		t.Errorf("InjectContextArgs = %v, want [actor_id]", cap.Actions[0].InjectContextArgs)
	}
	if len(cap.Actions[1].InjectContextArgs) != 0 {
		t.Errorf("navigate should have no InjectContextArgs, got %v", cap.Actions[1].InjectContextArgs)
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

// credCapturingPluginService records the credentials map from each Execute call.
type credCapturingPluginService struct {
	pluginpb.UnimplementedPluginServiceServer
	received chan map[string]string
}

func (s *credCapturingPluginService) Init(_ context.Context, _ *pluginpb.PluginInitRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (s *credCapturingPluginService) Capabilities(_ context.Context, _ *emptypb.Empty) (*pluginpb.PluginCapabilities, error) {
	return &pluginpb.PluginCapabilities{
		Name: "cred-echo",
		Actions: []*pluginpb.Action{
			{Name: "noop", Description: "No-op"},
		},
	}, nil
}

func (s *credCapturingPluginService) Execute(_ context.Context, req *pluginpb.ToolCallRequest) (*pluginpb.ToolResultResponse, error) {
	s.received <- req.Credentials
	return &pluginpb.ToolResultResponse{CallId: req.Id, Content: "ok"}, nil
}

func startCredCapturingServer(t *testing.T) (*grpc.ClientConn, chan map[string]string) {
	t.Helper()
	received := make(chan map[string]string, 1)
	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	pluginpb.RegisterPluginServiceServer(srv, &credCapturingPluginService{received: received})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { srv.Stop() })

	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return cc, received
}

// TestExecuteForwardsCredentialsFromProfile verifies that credentials stored
// on the profile in context are forwarded to the plugin via ToolCallRequest.
func TestExecuteForwardsCredentialsFromProfile(t *testing.T) {
	cc, received := startCredCapturingServer(t)
	c := &Client{conn: cc, client: pluginpb.NewPluginServiceClient(cc)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.fetchCapabilities(ctx, ""); err != nil {
		t.Fatal(err)
	}

	p := &profile.Profile{
		EntityID: "u1",
		Credentials: map[string]string{
			"timly": "user-api-token-abc",
			"jira":  "user-jira-token-xyz",
		},
	}
	ctx = profile.WithProfile(context.Background(), p)

	result := c.Execute(ctx, orchestrator.ToolCall{ID: "c1", Plugin: "cred-echo", Action: "noop"})
	if result.Error != "" {
		t.Fatalf("unexpected error: %s", result.Error)
	}

	creds := <-received
	if creds["timly"] != "user-api-token-abc" {
		t.Errorf("credentials[timly] = %q, want user-api-token-abc", creds["timly"])
	}
	if creds["jira"] != "user-jira-token-xyz" {
		t.Errorf("credentials[jira] = %q, want user-jira-token-xyz", creds["jira"])
	}
}

// TestExecuteNoCredentialsWithoutProfile verifies that nil credentials are sent
// when no profile is in the context.
func TestExecuteNoCredentialsWithoutProfile(t *testing.T) {
	cc, received := startCredCapturingServer(t)
	c := &Client{conn: cc, client: pluginpb.NewPluginServiceClient(cc)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.fetchCapabilities(ctx, ""); err != nil {
		t.Fatal(err)
	}

	result := c.Execute(context.Background(), orchestrator.ToolCall{ID: "c2", Plugin: "cred-echo", Action: "noop"})
	if result.Error != "" {
		t.Fatalf("unexpected error: %s", result.Error)
	}

	if creds := <-received; len(creds) != 0 {
		t.Errorf("credentials = %v, want empty when no profile in context", creds)
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

	result := exec.Execute(context.Background(), orchestrator.ToolCall{
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
