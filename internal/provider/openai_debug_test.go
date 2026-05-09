package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// recordingSink captures DebugEvent submissions for assertion.
type recordingSink struct {
	mu     sync.Mutex
	events []DebugEvent
}

func (r *recordingSink) Submit(_ context.Context, e DebugEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *recordingSink) snapshot() []DebugEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]DebugEvent, len(r.events))
	copy(out, r.events)
	return out
}

// alwaysOnResolver feeds the OpenAI provider session/trace IDs and returns
// enabled=true for every ctx — used to drive the capture path in tests.
func alwaysOnResolver(sessionID, traceID string) DebugContextResolver {
	return func(_ context.Context) (string, string, bool) {
		return sessionID, traceID, true
	}
}

func disabledResolver() DebugContextResolver {
	return func(_ context.Context) (string, string, bool) {
		return "", "", false
	}
}

func TestOpenAI_Complete_PersistsRequestAndResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := oaiResponse{
			ID:    "chatcmpl-1",
			Model: "gpt-4o",
			Choices: []oaiChoice{
				{Index: 0, Message: oaiMessage{Role: "assistant", Content: "ok"}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	sink := &recordingSink{}
	p := NewOpenAIProvider("openai", server.URL, "test-key", nil,
		WithOpenAIDebugSink(sink),
		WithOpenAIDebugResolver(alwaysOnResolver("sess-A", "trace-A")),
	)

	if _, err := p.Complete(context.Background(), &CompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	events := sink.snapshot()
	if len(events) != 2 {
		t.Fatalf("captured %d events, want 2 (request+response): %+v", len(events), events)
	}
	if events[0].Direction != "request" || events[1].Direction != "response" {
		t.Errorf("directions = %q, %q; want request, response", events[0].Direction, events[1].Direction)
	}
	if events[0].SessionID != "sess-A" || events[0].TraceID != "trace-A" {
		t.Errorf("event[0] correlation: session=%q trace=%q", events[0].SessionID, events[0].TraceID)
	}
	if events[1].Status != http.StatusOK {
		t.Errorf("response Status = %d, want 200", events[1].Status)
	}
	if !strings.Contains(events[0].Body, `"model":"gpt-4o"`) {
		t.Errorf("request body missing model: %q", events[0].Body)
	}
	if !strings.Contains(events[1].Body, `"chatcmpl-1"`) {
		t.Errorf("response body missing id: %q", events[1].Body)
	}
}

func TestOpenAI_Complete_DisabledResolverSkipsCapture(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(oaiResponse{ID: "x", Choices: []oaiChoice{{}}})
	}))
	defer server.Close()

	sink := &recordingSink{}
	p := NewOpenAIProvider("openai", server.URL, "k", nil,
		WithOpenAIDebugSink(sink),
		WithOpenAIDebugResolver(disabledResolver()),
	)
	_, _ = p.Complete(context.Background(), &CompletionRequest{Model: "x", Messages: []Message{{Role: RoleUser, Content: "h"}}})

	if got := sink.snapshot(); len(got) != 0 {
		t.Errorf("captured %d events, want 0 (resolver said no)", len(got))
	}
}

func TestOpenAI_Complete_NoSinkConfiguredSkipsCapture(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(oaiResponse{ID: "x", Choices: []oaiChoice{{}}})
	}))
	defer server.Close()

	// Resolver returns enabled but no sink wired.
	p := NewOpenAIProvider("openai", server.URL, "k", nil,
		WithOpenAIDebugResolver(alwaysOnResolver("s", "t")),
	)
	_, err := p.Complete(context.Background(), &CompletionRequest{Model: "x", Messages: []Message{{Role: RoleUser, Content: "h"}}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// No way for the test to inspect a nil sink — assertion is just "doesn't panic".
}

func TestOpenAI_Complete_CapturesNon200ResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"bad request"}`)
	}))
	defer server.Close()

	sink := &recordingSink{}
	p := NewOpenAIProvider("openai", server.URL, "k", nil,
		WithOpenAIDebugSink(sink),
		WithOpenAIDebugResolver(alwaysOnResolver("s", "t")),
	)
	_, err := p.Complete(context.Background(), &CompletionRequest{Model: "x", Messages: []Message{{Role: RoleUser, Content: "h"}}})
	if err == nil {
		t.Error("expected error from 400 response")
	}

	events := sink.snapshot()
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2 (request + non-200 response): %+v", len(events), events)
	}
	if events[1].Direction != "response" || events[1].Status != http.StatusBadRequest {
		t.Errorf("event[1]: direction=%q status=%d, want response/400", events[1].Direction, events[1].Status)
	}
}

func TestOpenAI_Complete_CapturesTransportError(t *testing.T) {
	// Hand the provider a deliberately broken URL so client.Do fails.
	sink := &recordingSink{}
	p := NewOpenAIProvider("openai", "http://127.0.0.1:1", "k", nil,
		WithOpenAIDebugSink(sink),
		WithOpenAIDebugResolver(alwaysOnResolver("s", "t")),
	)

	_, err := p.Complete(context.Background(), &CompletionRequest{Model: "x", Messages: []Message{{Role: RoleUser, Content: "h"}}})
	if err == nil {
		t.Error("expected error from unreachable endpoint")
	}

	events := sink.snapshot()
	// Expect: request (always) + error (from failed Do).
	var sawError bool
	for _, e := range events {
		if e.Direction == "error" {
			sawError = true
			if !strings.Contains(strings.ToLower(e.Body), "connect") &&
				!strings.Contains(strings.ToLower(e.Body), "refused") &&
				!strings.Contains(strings.ToLower(e.Body), "dial") {
				t.Errorf("error event body did not look like a connection error: %q", e.Body)
			}
		}
	}
	if !sawError {
		t.Errorf("no error-direction event captured; events: %+v", events)
	}
}

func TestOpenAI_Stream_AggregatesEndOfStreamResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		_, _ = fmt.Fprintln(w, `data: {"id":"1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"Hello"}}]}`)
		f.Flush()
		_, _ = fmt.Fprintln(w, `data: {"id":"1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":" world"}}]}`)
		f.Flush()
		_, _ = fmt.Fprintln(w, `data: {"id":"1","model":"gpt-4o","usage":{"prompt_tokens":3,"completion_tokens":2}}`)
		f.Flush()
		_, _ = fmt.Fprintln(w, `data: [DONE]`)
		f.Flush()
	}))
	defer server.Close()

	sink := &recordingSink{}
	p := NewOpenAIProvider("openai", server.URL, "k", nil,
		WithOpenAIDebugSink(sink),
		WithOpenAIDebugResolver(alwaysOnResolver("sess-S", "trace-S")),
	)

	stream, err := p.Stream(context.Background(), &CompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	for {
		c, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if c.Done {
			break
		}
	}
	if err := stream.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}

	events := sink.snapshot()
	// Request + aggregated streaming response.
	if len(events) < 2 {
		t.Fatalf("events = %d, want >=2: %+v", len(events), events)
	}
	last := events[len(events)-1]
	if last.Direction != "response" {
		t.Errorf("last event direction = %q, want response", last.Direction)
	}
	if last.SessionID != "sess-S" {
		t.Errorf("last.SessionID = %q, want sess-S", last.SessionID)
	}
	// Aggregated body must contain both deltas + the [DONE] marker.
	if !strings.Contains(last.Body, "Hello") || !strings.Contains(last.Body, " world") {
		t.Errorf("aggregated body missing deltas: %q", last.Body)
	}
	if !strings.Contains(last.Body, "[DONE]") {
		t.Errorf("aggregated body missing [DONE] marker: %q", last.Body)
	}
}

func TestOpenAI_Stream_NoCaptureWhenDisabled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		_, _ = fmt.Fprintln(w, `data: {"id":"1","model":"x","choices":[{"index":0,"delta":{"content":"hi"}}]}`)
		f.Flush()
		_, _ = fmt.Fprintln(w, `data: [DONE]`)
		f.Flush()
	}))
	defer server.Close()

	sink := &recordingSink{}
	p := NewOpenAIProvider("openai", server.URL, "k", nil,
		WithOpenAIDebugSink(sink),
		WithOpenAIDebugResolver(disabledResolver()),
	)

	stream, err := p.Stream(context.Background(), &CompletionRequest{Model: "x", Messages: []Message{{Role: RoleUser, Content: "h"}}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for {
		c, err := stream.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if c.Done {
			break
		}
	}
	_ = stream.Close()

	if got := sink.snapshot(); len(got) != 0 {
		t.Errorf("disabled resolver still produced %d events: %+v", len(got), got)
	}
}

// TestOpenAI_Stream_FlushesPartialBodyOnReadError verifies that capture
// survives a network-level abort mid-stream: the orchestrator's deferred
// Close() runs, onClose flushes whatever lines accumulated before the
// error, and the response row appears in the sink with the partial body.
func TestOpenAI_Stream_FlushesPartialBodyOnReadError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		_, _ = fmt.Fprintln(w, `data: {"id":"1","model":"x","choices":[{"index":0,"delta":{"content":"partial"}}]}`)
		f.Flush()
		// Hijack and close abruptly without sending [DONE].
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("test server does not support hijacking")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
	}))
	defer server.Close()

	sink := &recordingSink{}
	p := NewOpenAIProvider("openai", server.URL, "k", nil,
		WithOpenAIDebugSink(sink),
		WithOpenAIDebugResolver(alwaysOnResolver("sess-P", "trace-P")),
	)

	stream, err := p.Stream(context.Background(), &CompletionRequest{Model: "x", Messages: []Message{{Role: RoleUser, Content: "h"}}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	// Read until exhaustion (Recv may return Done because Scanner.Scan
	// returns false on EOF without surfacing the abrupt close as an error).
	for {
		c, err := stream.Recv()
		if err != nil || c.Done {
			break
		}
	}
	_ = stream.Close()

	events := sink.snapshot()
	if len(events) < 2 {
		t.Fatalf("events=%d, want >=2 (request+partial response)", len(events))
	}
	last := events[len(events)-1]
	if last.Direction != "response" {
		t.Errorf("last event direction = %q, want response", last.Direction)
	}
	if !strings.Contains(last.Body, "partial") {
		t.Errorf("partial body lost on abort: %q", last.Body)
	}
}

// TestOpenAI_Stream_CloseIsIdempotent ensures double Close() doesn't
// double-flush onClose or return a use-of-closed-connection error.
func TestOpenAI_Stream_CloseIsIdempotent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		_, _ = fmt.Fprintln(w, `data: {"id":"1","model":"x","choices":[{"index":0,"delta":{"content":"hi"}}]}`)
		f.Flush()
		_, _ = fmt.Fprintln(w, `data: [DONE]`)
		f.Flush()
	}))
	defer server.Close()

	sink := &recordingSink{}
	p := NewOpenAIProvider("openai", server.URL, "k", nil,
		WithOpenAIDebugSink(sink),
		WithOpenAIDebugResolver(alwaysOnResolver("s", "t")),
	)
	stream, err := p.Stream(context.Background(), &CompletionRequest{Model: "x", Messages: []Message{{Role: RoleUser, Content: "h"}}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for {
		c, err := stream.Recv()
		if err != nil || c.Done {
			break
		}
	}
	if err := stream.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Errorf("second Close should be no-op, got: %v", err)
	}

	// Exactly one response row, not two.
	var responseRows int
	for _, e := range sink.snapshot() {
		if e.Direction == "response" {
			responseRows++
		}
	}
	if responseRows != 1 {
		t.Errorf("response rows = %d, want 1 (double-Close must not double-flush)", responseRows)
	}
}

func TestStreamAccumulator_Truncates(t *testing.T) {
	a := &streamAccumulator{}
	long := strings.Repeat("a", maxStreamCaptureBytes/2)
	a.append(long)
	a.append(long)
	// Third append exceeds budget — must mark truncated and stop accumulating.
	a.append(long)
	if !a.truncated {
		t.Error("expected truncated=true after over-budget append")
	}
	if !strings.Contains(string(a.buf), "[truncated") {
		t.Error("expected truncation marker in buffer")
	}
	// Further appends are no-ops.
	bufLen := len(a.buf)
	a.append("more")
	if len(a.buf) != bufLen {
		t.Errorf("post-truncation append grew buffer: %d -> %d", bufLen, len(a.buf))
	}
}

// Ensure the stub-error type used in CapturesTransportError compiles.
var _ = errors.New
