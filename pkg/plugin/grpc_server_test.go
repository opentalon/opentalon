package plugin

import (
	"context"
	"net"
	"testing"

	"github.com/opentalon/opentalon/proto/pluginpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
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

// refreshableHandler implements Handler + Refreshable. RefreshCapabilities
// returns a capability set distinct from Capabilities() so the test can tell
// the refresh path (not the cached read) was taken.
type refreshableHandler struct{}

func (h *refreshableHandler) Capabilities() CapabilitiesMsg {
	return CapabilitiesMsg{Name: "mcp", Actions: []ActionMsg{{Name: "cached"}}}
}
func (h *refreshableHandler) Execute(req Request) Response { return Response{CallID: req.ID} }
func (h *refreshableHandler) RefreshCapabilities() CapabilitiesMsg {
	return CapabilitiesMsg{Name: "mcp", Actions: []ActionMsg{{Name: "fresh"}}}
}

// TestGRPCServer_RefreshCapabilities_Refreshable verifies the RPC calls the
// handler's RefreshCapabilities when it implements Refreshable.
func TestGRPCServer_RefreshCapabilities_Refreshable(t *testing.T) {
	client := startGRPCServer(t, &refreshableHandler{})
	resp, err := client.RefreshCapabilities(context.Background(), &emptypb.Empty{})
	if err != nil {
		t.Fatalf("RefreshCapabilities: %v", err)
	}
	if len(resp.Actions) != 1 || resp.Actions[0].Name != "fresh" {
		t.Errorf("expected fresh actions from refresh path, got %+v", resp.Actions)
	}
}

// TestGRPCServer_RefreshCapabilities_NotRefreshable verifies a handler that does
// not implement Refreshable reports gRPC Unimplemented, which the host poll uses
// to skip non-refreshable plugins.
func TestGRPCServer_RefreshCapabilities_NotRefreshable(t *testing.T) {
	client := startGRPCServer(t, &credHeaderCapturingHandler{received: make(chan map[string]CredentialHeader, 1)})
	_, err := client.RefreshCapabilities(context.Background(), &emptypb.Empty{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("expected Unimplemented for non-refreshable handler, got %v", err)
	}
}

// checkingHandler implements Handler + Checker. It answers every check with
// verdict and records the request it got. Execute fails the test: a check
// must never reach it.
type checkingHandler struct {
	t        *testing.T
	verdict  CheckResult
	received chan Request
}

func (h *checkingHandler) Capabilities() CapabilitiesMsg { return CapabilitiesMsg{Name: "test"} }
func (h *checkingHandler) Execute(req Request) Response {
	h.t.Errorf("Execute called for %q; a check must not execute", req.Action)
	return Response{CallID: req.ID}
}
func (h *checkingHandler) Check(_ context.Context, req Request) CheckResult {
	h.received <- req
	return h.verdict
}

// TestGRPCServer_Check verifies the Check RPC hands the handler the request
// Execute would get, marked as a check, and answers with its typed verdict.
func TestGRPCServer_Check(t *testing.T) {
	tests := []struct {
		name    string
		verdict CheckResult
		want    *pluginpb.CheckResponse
	}{
		{"passed", CheckResult{Verdict: CheckPassed},
			&pluginpb.CheckResponse{Verdict: pluginpb.CheckVerdict_CHECK_VERDICT_PASSED}},
		{"passed, confirmation not required", CheckResult{Verdict: CheckPassed, ConfirmationNotRequired: true},
			&pluginpb.CheckResponse{Verdict: pluginpb.CheckVerdict_CHECK_VERDICT_PASSED, ConfirmationNotRequired: true}},
		{"refused", CheckResult{Verdict: CheckRefused, Kind: "forbidden", Message: "missing right: tickets"},
			&pluginpb.CheckResponse{Verdict: pluginpb.CheckVerdict_CHECK_VERDICT_REFUSED, Kind: "forbidden", Message: "missing right: tickets"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &checkingHandler{t: t, verdict: tt.verdict, received: make(chan Request, 1)}
			client := startGRPCServer(t, h)

			resp, err := client.Check(context.Background(), &pluginpb.ToolCallRequest{
				Id:     "c1",
				Plugin: "mcp",
				Action: "start_walkthrough",
				Args:   map[string]string{"guide": "ticket"},
				CredentialHeaders: map[string]*pluginpb.CredentialHeader{
					"myapp": {Header: "X-App-User", Value: "user-123"},
				},
			})
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if !proto.Equal(resp, tt.want) {
				t.Errorf("Check = %v, want %v", resp, tt.want)
			}

			req := <-h.received
			if req.Method != "check" || req.ID != "c1" || req.Action != "start_walkthrough" || req.Args["guide"] != "ticket" {
				t.Errorf("handler got %+v, want the check of c1 start_walkthrough with guide=ticket", req)
			}
			if c := req.CredentialHeaders["myapp"]; c.Header != "X-App-User" || c.Value != "user-123" {
				t.Errorf("CredentialHeaders[myapp] = %+v, want {X-App-User user-123}", c)
			}
		})
	}
}

// TestGRPCServer_Check_NotChecker verifies a handler that does not implement
// Checker reports gRPC Unimplemented, which the host reads as a check that
// could not be answered.
func TestGRPCServer_Check_NotChecker(t *testing.T) {
	client := startGRPCServer(t, &credHeaderCapturingHandler{received: make(chan map[string]CredentialHeader, 1)})
	_, err := client.Check(context.Background(), &pluginpb.ToolCallRequest{Id: "c1", Action: "call"})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("expected Unimplemented for a handler without Checker, got %v", err)
	}
}
