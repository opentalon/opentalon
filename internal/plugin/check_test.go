package plugin

import (
	"context"
	"net"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/orchestrator"
	"github.com/opentalon/opentalon/internal/profile"
	pkg "github.com/opentalon/opentalon/pkg/plugin"
	"github.com/opentalon/opentalon/proto/pluginpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// checkHandler is an SDK plugin whose handler implements pkg.Checker. check
// decides each verdict; requests receives every request the handler gets,
// from Execute and from Check.
type checkHandler struct {
	check    func(ctx context.Context, req pkg.Request) pkg.CheckResult
	requests chan pkg.Request
}

func (h *checkHandler) Capabilities() pkg.CapabilitiesMsg {
	return pkg.CapabilitiesMsg{Name: "checker"}
}
func (h *checkHandler) Execute(req pkg.Request) pkg.Response {
	h.requests <- req
	return pkg.Response{CallID: req.ID, Content: "executed"}
}
func (h *checkHandler) Check(ctx context.Context, req pkg.Request) pkg.CheckResult {
	h.requests <- req
	return h.check(ctx, req)
}

// executeOnlyHandler is an SDK plugin whose handler does not implement
// pkg.Checker.
type executeOnlyHandler struct{}

func (executeOnlyHandler) Capabilities() pkg.CapabilitiesMsg {
	return pkg.CapabilitiesMsg{Name: "plain"}
}
func (executeOnlyHandler) Execute(req pkg.Request) pkg.Response {
	return pkg.Response{CallID: req.ID, Content: "executed"}
}

// dialBufconn returns a host Client connected to lis.
func dialBufconn(t *testing.T, lis *bufconn.Listener) *Client {
	t.Helper()
	cc, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return &Client{conn: cc, client: pluginpb.NewPluginServiceClient(cc)}
}

// startSDKPlugin serves handler through the SDK's gRPC server over bufconn:
// the production wire path pkg/plugin.ServeListener → internal/plugin.Client.
func startSDKPlugin(t *testing.T, handler pkg.Handler) *Client {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	t.Cleanup(func() { _ = lis.Close() })
	go func() { _ = pkg.ServeListener(lis, handler) }()
	return dialBufconn(t, lis)
}

// startPluginService serves a raw PluginService implementation over bufconn.
// desc is the service description to register it under.
func startPluginService(t *testing.T, desc *grpc.ServiceDesc, svc pluginpb.PluginServiceServer) *Client {
	t.Helper()
	lis := bufconn.Listen(bufSize)
	srv := grpc.NewServer()
	srv.RegisterService(desc, svc)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return dialBufconn(t, lis)
}

// TestClientCheck_SendsTheRequestExecuteSends verifies the host's check
// carries the action, the arguments and the profile's credential headers
// exactly as Execute does, and returns the handler's refusal typed.
func TestClientCheck_SendsTheRequestExecuteSends(t *testing.T) {
	h := &checkHandler{
		check: func(context.Context, pkg.Request) pkg.CheckResult {
			return pkg.CheckResult{Verdict: pkg.CheckRefused, Kind: "invalid_argument", Message: "name is taken"}
		},
		requests: make(chan pkg.Request, 2),
	}
	client := startSDKPlugin(t, h)
	ctx := profile.WithProfile(context.Background(), &profile.Profile{
		EntityID: "u1",
		Credentials: map[string]profile.CredentialHeader{
			"myapp": {Header: "X-App-User", Value: "user-123"},
		},
	})
	call := orchestrator.ToolCall{ID: "c1", Plugin: "checker", Action: "start", Args: map[string]string{"guide": "ticket"}}

	if res := client.Execute(ctx, call); res.Error != "" {
		t.Fatalf("Execute: %s", res.Error)
	}
	got, err := client.Check(ctx, call)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	want := orchestrator.CheckResult{Verdict: orchestrator.CheckRefused, Kind: "invalid_argument", Message: "name is taken"}
	if got != want {
		t.Errorf("Check = %+v, want %+v", got, want)
	}

	executed, checked := <-h.requests, <-h.requests
	if executed.Method != "execute" || checked.Method != "check" {
		t.Fatalf("methods = %q, %q; want execute, check", executed.Method, checked.Method)
	}
	executed.Method, checked.Method = "", ""
	if !reflect.DeepEqual(checked, executed) {
		t.Errorf("check sent %+v, Execute sent %+v; want the same request", checked, executed)
	}
	if c := checked.CredentialHeaders["myapp"]; c.Header != "X-App-User" || c.Value != "user-123" {
		t.Errorf("check's CredentialHeaders[myapp] = %+v, want {X-App-User user-123}", c)
	}
}

// verdictService answers every Check with resp, as a plugin in any language
// may put it on the wire.
type verdictService struct {
	pluginpb.UnimplementedPluginServiceServer
	resp *pluginpb.CheckResponse
}

func (s *verdictService) Check(context.Context, *pluginpb.ToolCallRequest) (*pluginpb.CheckResponse, error) {
	return s.resp, nil
}

// TestClientCheck_VerdictFromTheWire pins the host's reading of a verdict:
// a refusal keeps its kind and message; unspecified and a verdict this host
// does not know read as unspecified, without kind or message.
func TestClientCheck_VerdictFromTheWire(t *testing.T) {
	tests := []struct {
		name string
		resp *pluginpb.CheckResponse
		want orchestrator.CheckResult
	}{
		{"passed", &pluginpb.CheckResponse{Verdict: pluginpb.CheckVerdict_CHECK_VERDICT_PASSED},
			orchestrator.CheckResult{Verdict: orchestrator.CheckPassed}},
		{"refused", &pluginpb.CheckResponse{Verdict: pluginpb.CheckVerdict_CHECK_VERDICT_REFUSED, Kind: "forbidden", Message: "missing right: tickets"},
			orchestrator.CheckResult{Verdict: orchestrator.CheckRefused, Kind: "forbidden", Message: "missing right: tickets"}},
		{"unspecified", &pluginpb.CheckResponse{Kind: "forbidden", Message: "ignored"},
			orchestrator.CheckResult{}},
		{"unknown verdict", &pluginpb.CheckResponse{Verdict: pluginpb.CheckVerdict(99), Kind: "forbidden", Message: "ignored"},
			orchestrator.CheckResult{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := startPluginService(t, &pluginpb.PluginService_ServiceDesc, &verdictService{resp: tt.resp})
			got, err := client.Check(context.Background(), orchestrator.ToolCall{ID: "c1", Plugin: "p", Action: "a"})
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if got != tt.want {
				t.Errorf("Check = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestClientCheck_PluginWithoutCheck verifies a plugin that does not check
// calls answers gRPC Unimplemented and the host reads no verdict: an SDK
// handler without pkg.Checker, and a plugin binary built before the Check
// operation existed.
func TestClientCheck_PluginWithoutCheck(t *testing.T) {
	predatesCheck := pluginpb.PluginService_ServiceDesc
	predatesCheck.Methods = slices.DeleteFunc(slices.Clone(predatesCheck.Methods), func(m grpc.MethodDesc) bool {
		return m.MethodName == "Check"
	})
	if len(predatesCheck.Methods) != len(pluginpb.PluginService_ServiceDesc.Methods)-1 {
		t.Fatal("PluginService has no Check method to remove")
	}

	tests := []struct {
		name   string
		client func(t *testing.T) *Client
	}{
		{"handler without Checker", func(t *testing.T) *Client { return startSDKPlugin(t, executeOnlyHandler{}) }},
		{"binary that predates Check", func(t *testing.T) *Client {
			return startPluginService(t, &predatesCheck, &fakePluginService{})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.client(t).Check(context.Background(), orchestrator.ToolCall{ID: "c1", Plugin: "p", Action: "a"})
			if status.Code(err) != codes.Unimplemented {
				t.Fatalf("Check error = %v, want Unimplemented", err)
			}
			if got != (orchestrator.CheckResult{}) {
				t.Errorf("Check = %+v, want no verdict", got)
			}
		})
	}
}

// TestClientCheck_EndsAtTheDeadline verifies the host's check ends at the
// deadline of the context it is given, and that the handler's check gets
// that context, so the plugin can stop too. The handler answers only after
// the test, so its answer cannot race the host's own deadline.
func TestClientCheck_EndsAtTheDeadline(t *testing.T) {
	handlerSaw := make(chan error, 1)
	release := make(chan struct{})
	h := &checkHandler{
		check: func(ctx context.Context, _ pkg.Request) pkg.CheckResult {
			<-ctx.Done()
			handlerSaw <- ctx.Err()
			<-release
			return pkg.CheckResult{Verdict: pkg.CheckPassed}
		},
		requests: make(chan pkg.Request, 1),
	}
	client := startSDKPlugin(t, h)
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	got, err := client.Check(ctx, orchestrator.ToolCall{ID: "c1", Plugin: "checker", Action: "start"})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("Check error = %v, want DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Check returned after %s, want it to end at the 200ms deadline", elapsed)
	}
	if got != (orchestrator.CheckResult{}) {
		t.Errorf("Check = %+v, want no verdict", got)
	}
	select {
	case err := <-handlerSaw:
		if err == nil {
			t.Error("handler's context ended without an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler's context did not end at the host's deadline")
	}
}
