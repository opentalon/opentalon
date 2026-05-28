package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opentalon/opentalon/proto/pluginpb"
)

// streamHandler is a StreamingHandler test double: it lets the test
// specify the action body as a closure that receives the live
// HostCaller, so each scenario can fire whatever callbacks it wants
// and produce whatever Response it returns.
type streamHandler struct {
	caps CapabilitiesMsg
	body func(ctx context.Context, req Request, host HostCaller) Response
}

func (h *streamHandler) Capabilities() CapabilitiesMsg { return h.caps }
func (h *streamHandler) Execute(_ Request) Response    { return Response{Error: "unary not supported"} }
func (h *streamHandler) ExecuteWithCallbacks(ctx context.Context, req Request, host HostCaller) Response {
	return h.body(ctx, req, host)
}

// fakeStream stubs PluginService_ExecuteBidiServer for unit-testing
// the SDK's stream loop without spinning up a real gRPC server.
type fakeStream struct {
	pluginpb.PluginService_ExecuteBidiServer
	ctx    context.Context
	in     chan *pluginpb.HostMessage
	outCh  chan *pluginpb.PluginMessage
	closed atomic.Bool
}

func newFakeStream() *fakeStream {
	return &fakeStream{
		ctx:   context.Background(),
		in:    make(chan *pluginpb.HostMessage, 16),
		outCh: make(chan *pluginpb.PluginMessage, 16),
	}
}

func (f *fakeStream) Context() context.Context { return f.ctx }
func (f *fakeStream) Send(m *pluginpb.PluginMessage) error {
	if f.closed.Load() {
		return io.EOF
	}
	f.outCh <- m
	return nil
}
func (f *fakeStream) Recv() (*pluginpb.HostMessage, error) {
	m, ok := <-f.in
	if !ok {
		return nil, io.EOF
	}
	return m, nil
}
func (f *fakeStream) close() {
	if f.closed.CompareAndSwap(false, true) {
		close(f.in)
	}
}

// TestExecuteBidi_NoCallbacks: a handler that returns immediately
// without firing any callbacks. Verifies the round-trip of one
// ToolCallRequest → one ToolResultResponse.
func TestExecuteBidi_NoCallbacks(t *testing.T) {
	h := &streamHandler{
		caps: CapabilitiesMsg{Name: "p", SupportsCallbacks: true},
		body: func(_ context.Context, req Request, _ HostCaller) Response {
			return Response{CallID: req.ID, Content: "ok:" + req.Action}
		},
	}
	srv := &grpcServer{handler: h}
	stream := newFakeStream()

	stream.in <- &pluginpb.HostMessage{
		Payload: &pluginpb.HostMessage_Call{
			Call: &pluginpb.ToolCallRequest{Id: "c1", Plugin: "p", Action: "go"},
		},
	}

	done := make(chan error, 1)
	go func() { done <- srv.ExecuteBidi(stream) }()

	select {
	case msg := <-stream.outCh:
		res := msg.GetResult()
		if res == nil {
			t.Fatalf("expected Result frame, got %T", msg.GetPayload())
		}
		if res.GetCallId() != "c1" || res.GetContent() != "ok:go" {
			t.Errorf("result mismatch: %+v", res)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for Result frame")
	}

	stream.close()
	if err := <-done; err != nil && !errors.Is(err, io.EOF) {
		t.Errorf("ExecuteBidi returned: %v", err)
	}
}

// TestExecuteBidi_WithCallback: the handler fires one RunAction; the
// test plays the host side, receiving the CallbackRequest, sending
// back a CallbackResponse, then receiving the final Result. Mirrors
// what internal/plugin.Client.ExecuteBidi does in production.
func TestExecuteBidi_WithCallback(t *testing.T) {
	h := &streamHandler{
		caps: CapabilitiesMsg{Name: "p", SupportsCallbacks: true},
		body: func(ctx context.Context, req Request, host HostCaller) Response {
			res, err := host.RunAction(ctx, "inventory", "list-items", map[string]string{"q": "test"})
			if err != nil {
				return Response{Error: err.Error()}
			}
			return Response{CallID: req.ID, Content: "got: " + res.Content}
		},
	}
	srv := &grpcServer{handler: h}
	stream := newFakeStream()

	stream.in <- &pluginpb.HostMessage{
		Payload: &pluginpb.HostMessage_Call{
			Call: &pluginpb.ToolCallRequest{Id: "c1", Plugin: "p", Action: "go"},
		},
	}

	done := make(chan error, 1)
	go func() { done <- srv.ExecuteBidi(stream) }()

	// Expect a CallbackRequest first.
	var cbID string
	select {
	case msg := <-stream.outCh:
		cb := msg.GetCallbackRequest()
		if cb == nil {
			t.Fatalf("expected CallbackRequest frame, got %T", msg.GetPayload())
		}
		if cb.GetPlugin() != "inventory" || cb.GetAction() != "list-items" {
			t.Errorf("callback mismatch: %+v", cb)
		}
		if cb.GetArgs()["q"] != "test" {
			t.Errorf("args mismatch: %+v", cb.GetArgs())
		}
		cbID = cb.GetId()
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for CallbackRequest")
	}

	// Reply with the callback result.
	stream.in <- &pluginpb.HostMessage{
		Payload: &pluginpb.HostMessage_CallbackResponse{
			CallbackResponse: &pluginpb.CallbackResponse{Id: cbID, Content: "42 items"},
		},
	}

	// Expect the final Result.
	select {
	case msg := <-stream.outCh:
		res := msg.GetResult()
		if res == nil {
			t.Fatalf("expected Result frame, got %T", msg.GetPayload())
		}
		if res.GetContent() != "got: 42 items" {
			t.Errorf("final result: %q", res.GetContent())
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for Result frame")
	}

	stream.close()
	if err := <-done; err != nil && !errors.Is(err, io.EOF) {
		t.Errorf("ExecuteBidi returned: %v", err)
	}
}

// TestExecuteBidi_CallbackError: host returns error in the
// CallbackResponse; handler should see the error from RunAction and
// surface it in the final Response.
func TestExecuteBidi_CallbackError(t *testing.T) {
	h := &streamHandler{
		caps: CapabilitiesMsg{Name: "p", SupportsCallbacks: true},
		body: func(ctx context.Context, _ Request, host HostCaller) Response {
			_, err := host.RunAction(ctx, "x", "y", nil)
			if err == nil {
				return Response{Error: "expected error but got none"}
			}
			return Response{Content: "saw: " + err.Error()}
		},
	}
	srv := &grpcServer{handler: h}
	stream := newFakeStream()

	stream.in <- &pluginpb.HostMessage{
		Payload: &pluginpb.HostMessage_Call{Call: &pluginpb.ToolCallRequest{Id: "c1"}},
	}

	done := make(chan error, 1)
	go func() { done <- srv.ExecuteBidi(stream) }()

	cb := (<-stream.outCh).GetCallbackRequest()
	stream.in <- &pluginpb.HostMessage{
		Payload: &pluginpb.HostMessage_CallbackResponse{
			CallbackResponse: &pluginpb.CallbackResponse{Id: cb.GetId(), Error: "tool not allowed"},
		},
	}

	res := (<-stream.outCh).GetResult()
	if res.GetContent() != "saw: tool not allowed" {
		t.Errorf("content: %q", res.GetContent())
	}

	stream.close()
	<-done
}

// TestExecuteBidi_HandlerWithoutStreamingImpl: plain Handler that
// doesn't implement StreamingHandler. ExecuteBidi must fail with a
// clear error (not panic).
func TestExecuteBidi_HandlerWithoutStreamingImpl(t *testing.T) {
	h := &credHeaderCapturingHandler{}
	srv := &grpcServer{handler: h}
	stream := newFakeStream()
	defer stream.close()

	err := srv.ExecuteBidi(stream)
	if err == nil {
		t.Fatal("expected error when handler is not StreamingHandler")
	}
	if msg := fmt.Sprintf("%v", err); msg == "" {
		t.Errorf("error message empty")
	}
}

// Compile-time: streamHandler satisfies StreamingHandler.
var _ StreamingHandler = (*streamHandler)(nil)
