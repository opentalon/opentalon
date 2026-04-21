package plugin

import (
	"context"
	"net"
	"testing"

	"github.com/opentalon/opentalon/proto/pluginpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// credHeaderCapturingHandler records the CredentialHeaders from each Execute call.
type credHeaderCapturingHandler struct {
	received chan map[string]CredentialHeader
}

func (h *credHeaderCapturingHandler) Capabilities() CapabilitiesMsg {
	return CapabilitiesMsg{Name: "test"}
}

func (h *credHeaderCapturingHandler) Execute(req Request) Response {
	h.received <- req.CredentialHeaders
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

// TestGRPCServer_CredentialHeadersReachHandler verifies that credential headers sent in
// ToolCallRequest are forwarded to Handler.Execute via Request.CredentialHeaders.
func TestGRPCServer_CredentialHeadersReachHandler(t *testing.T) {
	received := make(chan map[string]CredentialHeader, 1)
	client := startGRPCServer(t, &credHeaderCapturingHandler{received: received})

	_, err := client.Execute(context.Background(), &pluginpb.ToolCallRequest{
		Id:     "c1",
		Plugin: "mcp",
		Action: "call",
		CredentialHeaders: map[string]*pluginpb.CredentialHeader{
			"myapp": {Header: "X-App-User", Value: "user-123"},
			"jira":  {Header: "Authorization", Value: "Bearer jira-xyz"},
		},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	creds := <-received
	if c := creds["myapp"]; c.Header != "X-App-User" || c.Value != "user-123" {
		t.Errorf("CredentialHeaders[myapp] = %+v, want {X-App-User user-123}", c)
	}
	if c := creds["jira"]; c.Header != "Authorization" || c.Value != "Bearer jira-xyz" {
		t.Errorf("CredentialHeaders[jira] = %+v, want {Authorization Bearer jira-xyz}", c)
	}
}

// TestGRPCServer_NoCredentialHeaders verifies that nil credential headers are forwarded as
// nil (not an empty map) when the request carries none.
func TestGRPCServer_NoCredentialHeaders(t *testing.T) {
	received := make(chan map[string]CredentialHeader, 1)
	client := startGRPCServer(t, &credHeaderCapturingHandler{received: received})

	_, err := client.Execute(context.Background(), &pluginpb.ToolCallRequest{
		Id:     "c2",
		Plugin: "mcp",
		Action: "call",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if creds := <-received; len(creds) != 0 {
		t.Errorf("CredentialHeaders = %v, want empty when none sent", creds)
	}
}
