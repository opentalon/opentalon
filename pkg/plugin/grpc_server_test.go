package plugin

import (
	"context"
	"net"
	"testing"

	"github.com/opentalon/opentalon/proto/pluginpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// credCapturingHandler records the Credentials map from each Execute call.
type credCapturingHandler struct {
	received chan map[string]string
}

func (h *credCapturingHandler) Capabilities() CapabilitiesMsg {
	return CapabilitiesMsg{Name: "test"}
}

func (h *credCapturingHandler) Execute(req Request) Response {
	h.received <- req.Credentials
	return Response{CallID: req.ID, Content: "ok"}
}

func startGRPCServer(t *testing.T, handler Handler) pluginpb.PluginServiceClient {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	pluginpb.RegisterPluginServiceServer(srv, &grpcServer{handler: handler})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Stop() })

	cc, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return pluginpb.NewPluginServiceClient(cc)
}

// TestGRPCServer_CredentialsReachHandler verifies that credentials sent in
// ToolCallRequest are forwarded to Handler.Execute via Request.Credentials.
func TestGRPCServer_CredentialsReachHandler(t *testing.T) {
	received := make(chan map[string]string, 1)
	client := startGRPCServer(t, &credCapturingHandler{received: received})

	_, err := client.Execute(context.Background(), &pluginpb.ToolCallRequest{
		Id:     "c1",
		Plugin: "mcp",
		Action: "call",
		Credentials: map[string]string{
			"mymcp": "user-token-abc",
			"jira":  "user-jira-xyz",
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	creds := <-received
	if creds["mymcp"] != "user-token-abc" {
		t.Errorf("Credentials[mymcp] = %q, want user-token-abc", creds["mymcp"])
	}
	if creds["jira"] != "user-jira-xyz" {
		t.Errorf("Credentials[jira] = %q, want user-jira-xyz", creds["jira"])
	}
}

// TestGRPCServer_NoCredentials verifies that nil credentials are forwarded as
// nil (not an empty map) when the request carries no credentials.
func TestGRPCServer_NoCredentials(t *testing.T) {
	received := make(chan map[string]string, 1)
	client := startGRPCServer(t, &credCapturingHandler{received: received})

	_, err := client.Execute(context.Background(), &pluginpb.ToolCallRequest{
		Id:     "c2",
		Plugin: "mcp",
		Action: "call",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if creds := <-received; len(creds) != 0 {
		t.Errorf("Credentials = %v, want empty when none sent", creds)
	}
}
