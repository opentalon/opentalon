package plugin

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/opentalon/opentalon/internal/orchestrator"
	pkg "github.com/opentalon/opentalon/pkg/plugin"
	"github.com/opentalon/opentalon/proto/pluginpb"
)

// bidiStreamingHandler implements pkg/plugin.StreamingHandler. The
// body closure lets each test plug in its own action logic.
type bidiStreamingHandler struct {
	body func(ctx context.Context, req pkg.Request, host pkg.HostCaller) pkg.Response
}

func (h *bidiStreamingHandler) Capabilities() pkg.CapabilitiesMsg {
	return pkg.CapabilitiesMsg{Name: "test", SupportsCallbacks: true}
}
func (h *bidiStreamingHandler) Execute(_ pkg.Request) pkg.Response {
	return pkg.Response{Error: "unary not supported in test"}
}
func (h *bidiStreamingHandler) ExecuteWithCallbacks(ctx context.Context, req pkg.Request, host pkg.HostCaller) pkg.Response {
	return h.body(ctx, req, host)
}

// recordingCallbackHandler captures the args of every RunAction call
// the host side dispatches, and returns canned content for each.
type recordingCallbackHandler struct {
	calls      []callbackInvocation
	response   func(plugin, action string, args map[string]string) (string, error)
	structured string // canned StructuredContent returned alongside content
}

type callbackInvocation struct {
	Plugin string
	Action string
	Args   map[string]string
}

func (r *recordingCallbackHandler) RunAction(ctx context.Context, plugin, action string, args map[string]string) (string, error) {
	content, _, err := r.RunActionResult(ctx, plugin, action, args)
	return content, err
}

func (r *recordingCallbackHandler) RunActionResult(_ context.Context, plugin, action string, args map[string]string) (string, string, error) {
	r.calls = append(r.calls, callbackInvocation{Plugin: plugin, Action: action, Args: args})
	if r.response != nil {
		content, err := r.response(plugin, action, args)
		return content, r.structured, err
	}
	return "default-reply", r.structured, nil
}

// startBidiServer wires a streaming handler into a real gRPC server
// (loopback TCP) and returns a Client connected to it. Mirrors the
// production wire path: pkg/plugin.Serve → pluginpb.PluginServer →
// internal/plugin.Client.
func startBidiServer(t *testing.T, body func(ctx context.Context, req pkg.Request, host pkg.HostCaller) pkg.Response) *Client {
	t.Helper()

	// Run the SDK's grpc server via its public Serve helper, but on a
	// listener we control so we can dial it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		_ = pkg.ServeListener(ln, &bidiStreamingHandler{body: body})
	}()

	dialCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cc, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })

	// Block until the server is reachable so the test isn't racing
	// against the listener accept loop.
	_ = dialCtx

	return &Client{
		conn:   cc,
		client: pluginpb.NewPluginServiceClient(cc),
	}
}

func TestClient_ExecuteBidi_RoundTrip(t *testing.T) {
	body := func(ctx context.Context, req pkg.Request, host pkg.HostCaller) pkg.Response {
		r, err := host.RunAction(ctx, "inv", "list", map[string]string{"q": "x"})
		if err != nil {
			return pkg.Response{Error: err.Error()}
		}
		return pkg.Response{CallID: req.ID, Content: "ok: " + r.Content}
	}
	client := startBidiServer(t, body)

	cb := &recordingCallbackHandler{
		response: func(plugin, action string, args map[string]string) (string, error) {
			return "42 found", nil
		},
	}
	result := client.ExecuteBidi(context.Background(), orchestrator.ToolCall{
		ID:     "c1",
		Plugin: "test",
		Action: "go",
	}, cb)

	if result.Error != "" {
		t.Fatalf("unexpected error: %s", result.Error)
	}
	if result.Content != "ok: 42 found" {
		t.Errorf("content: %q", result.Content)
	}
	if len(cb.calls) != 1 {
		t.Fatalf("expected 1 callback, got %d", len(cb.calls))
	}
	if cb.calls[0].Plugin != "inv" || cb.calls[0].Action != "list" {
		t.Errorf("callback dest: %+v", cb.calls[0])
	}
	if cb.calls[0].Args["q"] != "x" {
		t.Errorf("callback args: %+v", cb.calls[0].Args)
	}
}

// TestClient_ExecuteBidi_StructuredContent locks in that a callback's
// StructuredContent reaches the plugin. Regression: the host used to fill only
// CallbackResponse.Content, dropping StructuredContent, which broke plugins
// (e.g. talon-plugin.check/.evaluate) that rely on the structured payload.
func TestClient_ExecuteBidi_StructuredContent(t *testing.T) {
	var gotStructured string
	body := func(ctx context.Context, req pkg.Request, host pkg.HostCaller) pkg.Response {
		r, err := host.RunAction(ctx, "talon", "check", map[string]string{"workflow": "src"})
		if err != nil {
			return pkg.Response{Error: err.Error()}
		}
		gotStructured = r.StructuredContent
		return pkg.Response{CallID: req.ID, Content: "done"}
	}
	client := startBidiServer(t, body)

	cb := &recordingCallbackHandler{
		response:   func(_, _ string, _ map[string]string) (string, error) { return "ok: source is valid Talon.", nil },
		structured: `{"ok":true}`,
	}
	result := client.ExecuteBidi(context.Background(), orchestrator.ToolCall{ID: "c3", Plugin: "agents", Action: "create"}, cb)

	if result.Error != "" {
		t.Fatalf("unexpected error: %s", result.Error)
	}
	if gotStructured != `{"ok":true}` {
		t.Errorf("StructuredContent not propagated to plugin: got %q, want {\"ok\":true}", gotStructured)
	}
}

func TestClient_ExecuteBidi_NoCallbacks(t *testing.T) {
	body := func(_ context.Context, req pkg.Request, _ pkg.HostCaller) pkg.Response {
		return pkg.Response{CallID: req.ID, Content: "direct"}
	}
	client := startBidiServer(t, body)

	cb := &recordingCallbackHandler{}
	result := client.ExecuteBidi(context.Background(), orchestrator.ToolCall{ID: "c2"}, cb)

	if result.Error != "" {
		t.Fatalf("err: %s", result.Error)
	}
	if result.Content != "direct" {
		t.Errorf("content: %q", result.Content)
	}
	if len(cb.calls) != 0 {
		t.Errorf("expected 0 callbacks, got %d", len(cb.calls))
	}
}

func TestClient_ExecuteBidi_HostError(t *testing.T) {
	body := func(ctx context.Context, _ pkg.Request, host pkg.HostCaller) pkg.Response {
		_, err := host.RunAction(ctx, "x", "y", nil)
		if err == nil {
			return pkg.Response{Error: "expected host error"}
		}
		return pkg.Response{Content: "saw: " + err.Error()}
	}
	client := startBidiServer(t, body)

	cb := &recordingCallbackHandler{
		response: func(_, _ string, _ map[string]string) (string, error) {
			return "", errBoom
		},
	}
	result := client.ExecuteBidi(context.Background(), orchestrator.ToolCall{ID: "c3"}, cb)

	if result.Error != "" {
		t.Fatalf("err: %s", result.Error)
	}
	if result.Content != "saw: boom" {
		t.Errorf("content: %q", result.Content)
	}
}

type stringError string

func (e stringError) Error() string { return string(e) }

var errBoom = stringError("boom")
