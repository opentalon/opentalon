package channel

import (
	"context"
	"net"
	"testing"
	"time"

	pkg "github.com/opentalon/opentalon/pkg/channel"
	"github.com/opentalon/opentalon/pkg/channel/channelpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const bufSize = 1024 * 1024

// fakeChannelService is a gRPC server-side implementation for tests.
type fakeChannelService struct {
	channelpb.UnimplementedChannelServiceServer
	caps       *channelpb.ChannelCapabilities
	tools      []*channelpb.ToolDefinition
	configSeen map[string]interface{}
	received   []*channelpb.OutboundMessage
}

func (s *fakeChannelService) Capabilities(_ context.Context, _ *emptypb.Empty) (*channelpb.ChannelCapabilities, error) {
	return s.caps, nil
}

func (s *fakeChannelService) Configure(_ context.Context, req *channelpb.ConfigureRequest) (*channelpb.ConfigureResponse, error) {
	if req.Config != nil {
		s.configSeen = req.Config.AsMap()
	}
	return &channelpb.ConfigureResponse{}, nil
}

func (s *fakeChannelService) Tools(_ context.Context, _ *emptypb.Empty) (*channelpb.ToolsResponse, error) {
	return &channelpb.ToolsResponse{Tools: s.tools}, nil
}

func (s *fakeChannelService) Start(_ *emptypb.Empty, stream channelpb.ChannelService_StartServer) error {
	// Send one test inbound message.
	msg := &channelpb.InboundMessage{
		ChannelId:      s.caps.Id,
		ConversationId: "conv-1",
		SenderId:       "user-1",
		SenderName:     "Diana",
		Content:        "hello from plugin",
		Timestamp:      timestamppb.Now(),
	}
	if err := stream.Send(msg); err != nil {
		return err
	}
	// Keep stream open until cancelled.
	<-stream.Context().Done()
	return nil
}

func (s *fakeChannelService) Send(_ context.Context, msg *channelpb.OutboundMessage) (*channelpb.SendResponse, error) {
	s.received = append(s.received, msg)
	return &channelpb.SendResponse{}, nil
}

func startFakeChannelServer(t *testing.T, svc *fakeChannelService) *grpc.ClientConn {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	channelpb.RegisterChannelServiceServer(srv, svc)
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

func newTestClient(t *testing.T, svc *fakeChannelService) *PluginClient {
	t.Helper()
	cc := startFakeChannelServer(t, svc)
	client := &PluginClient{
		conn:   cc,
		client: channelpb.NewChannelServiceClient(cc),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.fetchCapabilities(ctx); err != nil {
		t.Fatal(err)
	}
	return client
}

func TestChannelClientCapabilities(t *testing.T) {
	svc := &fakeChannelService{
		caps: &channelpb.ChannelCapabilities{
			Id:               "test-slack",
			Name:             "Slack",
			Threads:          true,
			Files:            true,
			MaxMessageLength: 40000,
		},
	}

	client := newTestClient(t, svc)
	defer func() { _ = client.Stop() }()

	if client.ID() != "test-slack" {
		t.Errorf("id = %q, want test-slack", client.ID())
	}

	caps := client.Capabilities()
	if caps.Name != "Slack" {
		t.Errorf("name = %q", caps.Name)
	}
	if !caps.Threads {
		t.Error("threads should be true")
	}
	if !caps.Files {
		t.Error("files should be true")
	}
	if caps.MaxMessageLength != 40000 {
		t.Errorf("max_message_length = %d", caps.MaxMessageLength)
	}
}

func TestChannelClientSend(t *testing.T) {
	svc := &fakeChannelService{
		caps: &channelpb.ChannelCapabilities{Id: "test-ch", Name: "Test"},
	}

	client := newTestClient(t, svc)
	defer func() { _ = client.Stop() }()

	msg := pkg.OutboundMessage{
		ConversationID: "conv-1",
		Content:        "hello from core",
	}

	if err := client.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}

	if len(svc.received) != 1 {
		t.Fatalf("expected 1 received message, got %d", len(svc.received))
	}
	if svc.received[0].Content != "hello from core" {
		t.Errorf("content = %q", svc.received[0].Content)
	}
}

func TestChannelClientReceive(t *testing.T) {
	svc := &fakeChannelService{
		caps: &channelpb.ChannelCapabilities{Id: "recv-ch", Name: "Recv"},
	}

	client := newTestClient(t, svc)
	defer func() { _ = client.Stop() }()

	inbox := make(chan pkg.InboundMessage, 10)
	if err := client.Start(context.Background(), inbox); err != nil {
		t.Fatal(err)
	}

	select {
	case msg := <-inbox:
		if msg.Content != "hello from plugin" {
			t.Errorf("content = %q", msg.Content)
		}
		if msg.SenderName != "Diana" {
			t.Errorf("sender = %q", msg.SenderName)
		}
		if msg.ChannelID != "recv-ch" {
			t.Errorf("channel_id = %q", msg.ChannelID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for inbound message")
	}
}

func TestChannelClientDialFailure(t *testing.T) {
	_, err := DialChannel("unix", "/nonexistent/channel.sock", defaultDialTimeout)
	// gRPC NewClient is lazy, so dial failure happens on first RPC (fetchCapabilities).
	if err == nil {
		t.Error("expected error for nonexistent socket")
	}
}

func TestChannelClientConfigure(t *testing.T) {
	svc := &fakeChannelService{
		caps: &channelpb.ChannelCapabilities{Id: "cfg-ch", Name: "Config"},
	}

	client := newTestClient(t, svc)
	defer func() { _ = client.Stop() }()

	cfg := map[string]interface{}{
		"app_token_env": "SLACK_APP_TOKEN",
		"bot_token_env": "SLACK_BOT_TOKEN",
	}
	if err := client.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	if svc.configSeen == nil {
		t.Fatal("configure was not received by server")
	}
	if svc.configSeen["app_token_env"] != "SLACK_APP_TOKEN" {
		t.Errorf("config app_token_env = %v", svc.configSeen["app_token_env"])
	}
}

func TestChannelClientTools(t *testing.T) {
	svc := &fakeChannelService{
		caps: &channelpb.ChannelCapabilities{Id: "tools-ch", Name: "Tools"},
		tools: []*channelpb.ToolDefinition{
			{
				Plugin:            "slack",
				Action:            "post_message",
				ActionDescription: "Post a message to Slack",
				Parameters: []*channelpb.ToolParam{
					{Name: "channel", Required: true},
					{Name: "text", Required: true},
				},
			},
		},
	}

	client := newTestClient(t, svc)
	defer func() { _ = client.Stop() }()

	tools, err := client.Tools()
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0].Action != "post_message" {
		t.Errorf("action = %q", tools[0].Action)
	}
	if len(tools[0].Parameters) != 2 {
		t.Errorf("params = %d", len(tools[0].Parameters))
	}
}
