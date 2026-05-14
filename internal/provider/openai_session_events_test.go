package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/state/store/events"
	"github.com/opentalon/opentalon/internal/state/store/events/emit"
)

// recordingEventSink captures every emit.Event in arrival order. Mirrors
// the recordingSink used for DebugEventSink tests. The mutex protects
// concurrent Emit calls from arbitrary callers — note that this says
// nothing about the SafetyOfRecv/Close on the oaiResponseStream itself,
// whose accumulator fields are not mutex-protected and follow the
// orchestrator's single-goroutine Recv→Close contract.
type recordingEventSink struct {
	mu     sync.Mutex
	events []emit.Event
}

func (s *recordingEventSink) Emit(_ context.Context, e emit.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

func (s *recordingEventSink) snapshot() []emit.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]emit.Event, len(s.events))
	copy(out, s.events)
	return out
}

func (s *recordingEventSink) types() []string {
	got := s.snapshot()
	out := make([]string, len(got))
	for i, e := range got {
		out[i] = e.EventType
	}
	return out
}

// ----- Complete path -----

func TestOpenAIComplete_EmitsRequestAndResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A small sleep makes the millisecond-resolution latency
		// measurement deterministic. httptest loopback completes in
		// <1 ms otherwise and the LatencyMS field rounds to 0 with
		// omitempty stripping it from the JSON.
		time.Sleep(2 * time.Millisecond)
		resp := oaiResponse{
			ID:    "chatcmpl-evt-1",
			Model: "gpt-4o",
			Choices: []oaiChoice{{
				Index:        0,
				Message:      oaiMessage{Role: "assistant", Content: "Hello!"},
				FinishReason: "stop",
			}},
			Usage: oaiUsage{PromptTokens: 11, CompletionTokens: 7},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	sink := &recordingEventSink{}
	p := NewOpenAIProvider("openai", server.URL, "test-key", nil, WithOpenAISessionEventSink(sink))
	_, err := p.Complete(context.Background(), &CompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: RoleUser, Content: "Hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	got := sink.snapshot()
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (llm_request + llm_response)", len(got))
	}
	if got[0].EventType != events.TypeLLMRequest {
		t.Errorf("events[0] = %q, want %q", got[0].EventType, events.TypeLLMRequest)
	}
	if got[1].EventType != events.TypeLLMResponse {
		t.Errorf("events[1] = %q, want %q", got[1].EventType, events.TypeLLMResponse)
	}

	var req events.LLMRequestPayload
	if err := json.Unmarshal(got[0].Payload, &req); err != nil {
		t.Fatalf("unmarshal request payload: %v", err)
	}
	if req.ModelID != "gpt-4o" || req.MessageCount != 1 {
		t.Errorf("request payload mismatch: %+v", req)
	}

	var resp events.LLMResponsePayload
	if err := json.Unmarshal(got[1].Payload, &resp); err != nil {
		t.Fatalf("unmarshal response payload: %v", err)
	}
	if resp.RawContentExcerpt != "Hello!" {
		t.Errorf("RawContentExcerpt = %q", resp.RawContentExcerpt)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want %q (oaiChoice.FinishReason must propagate to event)", resp.FinishReason, "stop")
	}
	if resp.TokensIn != 11 || resp.TokensOut != 7 {
		t.Errorf("tokens = %d/%d", resp.TokensIn, resp.TokensOut)
	}
	if resp.ProviderResponseID != "chatcmpl-evt-1" {
		t.Errorf("ProviderResponseID = %q", resp.ProviderResponseID)
	}
	if got[1].DurationMS <= 0 {
		t.Errorf("row DurationMS = %d, want >0 (latency wraps the HTTP call)", got[1].DurationMS)
	}
}

func TestOpenAIComplete_EmitsRefusedOnContentFilter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := oaiResponse{
			ID:    "chatcmpl-refused",
			Model: "gpt-4o",
			Choices: []oaiChoice{{
				Index:        0,
				Message:      oaiMessage{Role: "assistant", Content: "I can't help with that."},
				FinishReason: "content_filter",
			}},
			Usage: oaiUsage{PromptTokens: 5, CompletionTokens: 6},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	sink := &recordingEventSink{}
	p := NewOpenAIProvider("openai", server.URL, "test-key", nil, WithOpenAISessionEventSink(sink))
	_, _ = p.Complete(context.Background(), &CompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: RoleUser, Content: "x"}},
	})

	got := sink.snapshot()
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2", len(got))
	}
	if got[1].EventType != events.TypeLLMRefused {
		t.Errorf("events[1] = %q, want %q (content_filter must classify as refusal, not response)",
			got[1].EventType, events.TypeLLMRefused)
	}
	var p2 events.LLMRefusedPayload
	if err := json.Unmarshal(got[1].Payload, &p2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p2.ContentSafetyHit != "content_filter" {
		t.Errorf("ContentSafetyHit = %q", p2.ContentSafetyHit)
	}
	if p2.RefusalText != "I can't help with that." {
		t.Errorf("RefusalText = %q", p2.RefusalText)
	}
}

func TestOpenAIComplete_EmitsErrorOnNonOKStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream timeout","type":"server_error"}}`))
	}))
	defer server.Close()

	sink := &recordingEventSink{}
	p := NewOpenAIProvider("openai", server.URL, "test-key", nil, WithOpenAISessionEventSink(sink))
	_, err := p.Complete(context.Background(), &CompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: RoleUser, Content: "x"}},
	})
	if err == nil {
		t.Fatal("Complete must error on 502")
	}

	got := sink.snapshot()
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (request + error)", len(got))
	}
	if got[1].EventType != events.TypeLLMError {
		t.Errorf("events[1] = %q, want %q", got[1].EventType, events.TypeLLMError)
	}
	var p2 events.LLMErrorPayload
	_ = json.Unmarshal(got[1].Payload, &p2)
	if p2.StatusCode != 502 {
		t.Errorf("StatusCode = %d, want 502", p2.StatusCode)
	}
	if !strings.Contains(p2.ResponseBodyExcerpt, "upstream timeout") {
		t.Errorf("ResponseBodyExcerpt = %q, want substring 'upstream timeout'", p2.ResponseBodyExcerpt)
	}
	if p2.Phase == "" {
		t.Error("Phase must be set on llm_error (helps analytics group errors by location)")
	}
}

func TestOpenAIComplete_EmitsErrorOnBodyReadFailure(t *testing.T) {
	// io.ReadAll failure path is hard to trigger over a real server
	// because httptest always lets the body finish. A custom transport
	// hands back a Response whose Body errors on Read — exercising the
	// "chat.read_response" phase that real upstreams hit when the wire
	// drops mid-body (proxy timeout, TLS hiccup).
	failingClient := &http.Client{Transport: roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(&erroringReader{err: errors.New("simulated body read failure")}),
			Header:     make(http.Header),
		}, nil
	})}

	sink := &recordingEventSink{}
	p := NewOpenAIProvider("openai", "http://example.invalid", "test-key", nil,
		WithOpenAIHTTPClient(failingClient),
		WithOpenAISessionEventSink(sink))
	_, err := p.Complete(context.Background(), &CompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: RoleUser, Content: "x"}},
	})
	if err == nil {
		t.Fatal("Complete must propagate body-read error")
	}

	got := sink.snapshot()
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (request + body-read error); types=%v", len(got), sink.types())
	}
	var perr events.LLMErrorPayload
	_ = json.Unmarshal(got[1].Payload, &perr)
	if perr.Phase != phaseChatReadResponse {
		t.Errorf("Phase = %q, want %q", perr.Phase, phaseChatReadResponse)
	}
}

func TestOpenAIComplete_EmitsErrorOnMalformedJSON(t *testing.T) {
	// HTTP 200 with body that fails json.Unmarshal — the chat.unmarshal
	// phase. Real upstreams hit this when proxies inject HTML error
	// pages with 200 status, or when a buggy SDK serialises with NaN.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`not a valid json body`))
	}))
	defer server.Close()

	sink := &recordingEventSink{}
	p := NewOpenAIProvider("openai", server.URL, "test-key", nil, WithOpenAISessionEventSink(sink))
	_, err := p.Complete(context.Background(), &CompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: RoleUser, Content: "x"}},
	})
	if err == nil {
		t.Fatal("Complete must error on unparseable body")
	}

	got := sink.snapshot()
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2; types=%v", len(got), sink.types())
	}
	var perr events.LLMErrorPayload
	_ = json.Unmarshal(got[1].Payload, &perr)
	if perr.Phase != phaseChatUnmarshal {
		t.Errorf("Phase = %q, want %q", perr.Phase, phaseChatUnmarshal)
	}
	if perr.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200 (unmarshal errors carry the original status)", perr.StatusCode)
	}
}

func TestOpenAIComplete_EmitsErrorOnAPIErrorIn200Body(t *testing.T) {
	// HTTP 200 with a {"error":{...}} payload — OpenAI sometimes does
	// this for quota / auth issues. The chat.api_error phase exists
	// specifically so analytics keeps these distinct from 4xx/5xx.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":{"message":"quota exceeded","type":"insufficient_quota","code":"q1"}}`))
	}))
	defer server.Close()

	sink := &recordingEventSink{}
	p := NewOpenAIProvider("openai", server.URL, "test-key", nil, WithOpenAISessionEventSink(sink))
	_, err := p.Complete(context.Background(), &CompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: RoleUser, Content: "x"}},
	})
	if err == nil {
		t.Fatal("Complete must propagate embedded-error body")
	}

	got := sink.snapshot()
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2; types=%v", len(got), sink.types())
	}
	var perr events.LLMErrorPayload
	_ = json.Unmarshal(got[1].Payload, &perr)
	if perr.Phase != phaseChatAPIError {
		t.Errorf("Phase = %q, want %q (api_error must be distinct from http_status)", perr.Phase, phaseChatAPIError)
	}
	if !strings.Contains(perr.ResponseBodyExcerpt, "quota exceeded") {
		t.Errorf("ResponseBodyExcerpt = %q, want substring 'quota exceeded'", perr.ResponseBodyExcerpt)
	}
}

func TestOpenAIComplete_EmitsErrorOnTransportFailure(t *testing.T) {
	// Server is created and immediately closed: any subsequent request
	// hits a dead listener, triggering a transport-level error.
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	server.Close()

	sink := &recordingEventSink{}
	p := NewOpenAIProvider("openai", server.URL, "test-key", nil, WithOpenAISessionEventSink(sink))
	_, err := p.Complete(context.Background(), &CompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: RoleUser, Content: "x"}},
	})
	if err == nil {
		t.Fatal("Complete must error on transport failure")
	}

	got := sink.snapshot()
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (request + transport error)", len(got))
	}
	if got[1].EventType != events.TypeLLMError {
		t.Errorf("events[1] = %q, want %q", got[1].EventType, events.TypeLLMError)
	}
	var p2 events.LLMErrorPayload
	_ = json.Unmarshal(got[1].Payload, &p2)
	if p2.Phase != phaseChatTransport {
		t.Errorf("Phase = %q, want %q (transport phase must be distinct from http_status phase)",
			p2.Phase, phaseChatTransport)
	}
}

func TestOpenAIComplete_NativeToolCallsRawInPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := oaiResponse{
			ID:    "chatcmpl-tools",
			Model: "gpt-4o",
			Choices: []oaiChoice{{
				Index: 0,
				Message: oaiMessage{
					Role: "assistant", Content: "",
					ToolCalls: []oaiToolCall{{
						ID:   "call_1",
						Type: "function",
						Function: oaiToolCallFunction{
							Name:      "tickets.show",
							Arguments: `{"id":"42"}`,
						},
					}},
				},
				FinishReason: "tool_calls",
			}},
			Usage: oaiUsage{PromptTokens: 8, CompletionTokens: 4},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	sink := &recordingEventSink{}
	p := NewOpenAIProvider("openai", server.URL, "test-key", nil, WithOpenAISessionEventSink(sink))
	_, err := p.Complete(context.Background(), &CompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: RoleUser, Content: "show 42"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	var resp events.LLMResponsePayload
	_ = json.Unmarshal(sink.snapshot()[1].Payload, &resp)
	if len(resp.NativeToolCallsRaw) == 0 {
		t.Fatal("NativeToolCallsRaw must be non-empty when provider returned tool_calls")
	}
	if !json.Valid(resp.NativeToolCallsRaw) {
		t.Errorf("NativeToolCallsRaw = %q, want valid inline JSON", string(resp.NativeToolCallsRaw))
	}
	// Inline-embedded, not escaped-string: the first byte must be '['
	// (array open) — string-encoded would be '"'.
	if resp.NativeToolCallsRaw[0] != '[' {
		t.Errorf("NativeToolCallsRaw[0] = %q, want '[' (inline JSON, not escaped string)", resp.NativeToolCallsRaw[0])
	}
	// Round-trip: the embedded JSON must decode back into the same
	// oaiToolCall shape the upstream sent. A regression that
	// double-encoded or lost call_id / arguments would slip past the
	// looser "starts with [" check but fail this assertion.
	var roundTrip []oaiToolCall
	if err := json.Unmarshal(resp.NativeToolCallsRaw, &roundTrip); err != nil {
		t.Fatalf("NativeToolCallsRaw round-trip unmarshal: %v", err)
	}
	if len(roundTrip) != 1 {
		t.Fatalf("round-trip tool calls = %d, want 1", len(roundTrip))
	}
	if roundTrip[0].ID != "call_1" {
		t.Errorf("round-trip call_id = %q, want %q", roundTrip[0].ID, "call_1")
	}
	if roundTrip[0].Function.Name != "tickets.show" {
		t.Errorf("round-trip function name = %q", roundTrip[0].Function.Name)
	}
	if roundTrip[0].Function.Arguments != `{"id":"42"}` {
		t.Errorf("round-trip arguments = %q (must preserve verbatim provider JSON)", roundTrip[0].Function.Arguments)
	}
}

// ----- Stream path -----

// streamResponse builds an SSE response body that mirrors what OpenAI
// sends for a streamed chat completion: a few content delta chunks, an
// optional content_filter-bearing chunk, a usage-only chunk, and a
// terminating [DONE] marker.
func streamResponse(deltas []string, finishReason string, includeUsage bool) string {
	var b strings.Builder
	for _, d := range deltas {
		chunk := map[string]any{
			"id":    "chatcmpl-stream",
			"model": "gpt-4o",
			"choices": []map[string]any{{
				"index":         0,
				"delta":         map[string]string{"content": d},
				"finish_reason": nil,
			}},
		}
		raw, _ := json.Marshal(chunk)
		b.WriteString("data: ")
		b.Write(raw)
		b.WriteString("\n\n")
	}
	if finishReason != "" {
		chunk := map[string]any{
			"id":    "chatcmpl-stream",
			"model": "gpt-4o",
			"choices": []map[string]any{{
				"index":         0,
				"delta":         map[string]string{},
				"finish_reason": finishReason,
			}},
		}
		raw, _ := json.Marshal(chunk)
		b.WriteString("data: ")
		b.Write(raw)
		b.WriteString("\n\n")
	}
	if includeUsage {
		chunk := map[string]any{
			"id":      "chatcmpl-stream",
			"model":   "gpt-4o",
			"choices": []map[string]any{},
			"usage":   map[string]int{"prompt_tokens": 12, "completion_tokens": 9},
		}
		raw, _ := json.Marshal(chunk)
		b.WriteString("data: ")
		b.Write(raw)
		b.WriteString("\n\n")
	}
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func drainStream(t *testing.T, stream ResponseStream) {
	t.Helper()
	for {
		chunk, err := stream.Recv()
		if err != nil {
			break
		}
		if chunk.Done {
			break
		}
	}
}

func TestOpenAIStream_EmitsRequestThenResponseOnClose(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Small sleep makes the latency endpoint (last-chunk-arrival) strictly
		// greater than the start timestamp at millisecond resolution.
		// Otherwise loopback flushes too fast and DurationMS rounds to 0.
		time.Sleep(2 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(streamResponse([]string{"hello, ", "world"}, "stop", true)))
	}))
	defer server.Close()

	sink := &recordingEventSink{}
	p := NewOpenAIProvider("openai", server.URL, "test-key", nil, WithOpenAISessionEventSink(sink))
	stream, err := p.Stream(context.Background(), &CompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: RoleUser, Content: "Hi"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drainStream(t, stream)
	// Sleep AFTER draining: latency must NOT include this gap because
	// the endpoint is last-chunk-arrival, not Close(). If a future
	// regression switches back to time.Since(start) in Close, this
	// sleep would inflate DurationMS by >20 ms and the upper bound
	// below would catch the drift.
	time.Sleep(20 * time.Millisecond)
	if err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got := sink.snapshot()
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (llm_request + llm_response on Close), types=%v", len(got), sink.types())
	}
	if got[0].EventType != events.TypeLLMRequest {
		t.Errorf("events[0] = %q, want %q", got[0].EventType, events.TypeLLMRequest)
	}
	if got[1].EventType != events.TypeLLMResponse {
		t.Errorf("events[1] = %q, want %q", got[1].EventType, events.TypeLLMResponse)
	}
	var resp events.LLMResponsePayload
	_ = json.Unmarshal(got[1].Payload, &resp)
	if resp.RawContentExcerpt != "hello, world" {
		t.Errorf("RawContentExcerpt = %q (deltas must be reassembled)", resp.RawContentExcerpt)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q", resp.FinishReason)
	}
	if resp.TokensIn != 12 || resp.TokensOut != 9 {
		t.Errorf("tokens = %d/%d (usage-only chunk must propagate to event)", resp.TokensIn, resp.TokensOut)
	}
	if resp.ProviderResponseID != "chatcmpl-stream" {
		t.Errorf("ProviderResponseID = %q", resp.ProviderResponseID)
	}
	// Latency endpoint is last-chunk-arrival, not Close. With a 2 ms
	// server sleep + 20 ms post-drain wait, the measured latency must
	// be at least the server sleep but well under the orchestrator
	// drain window.
	if got[1].DurationMS < 2 {
		t.Errorf("row DurationMS = %d, want >=2 (server sleep)", got[1].DurationMS)
	}
	if got[1].DurationMS >= 20 {
		t.Errorf("row DurationMS = %d, want <20 (latency must NOT include post-drain wait — bug if endpoint moved to Close)", got[1].DurationMS)
	}
}

func TestOpenAIStream_EmitsErrorOnTransportFailure(t *testing.T) {
	// Mirror of the Complete-side transport-failure test for the stream
	// path. Distinct phase label so analytics can group transport
	// failures by call type.
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	server.Close()

	sink := &recordingEventSink{}
	p := NewOpenAIProvider("openai", server.URL, "test-key", nil, WithOpenAISessionEventSink(sink))
	_, err := p.Stream(context.Background(), &CompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: RoleUser, Content: "x"}},
	})
	if err == nil {
		t.Fatal("Stream must error on transport failure")
	}

	got := sink.snapshot()
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (llm_request + stream transport error); types=%v", len(got), sink.types())
	}
	var perr events.LLMErrorPayload
	_ = json.Unmarshal(got[1].Payload, &perr)
	if perr.Phase != phaseStreamTransport {
		t.Errorf("Phase = %q, want %q", perr.Phase, phaseStreamTransport)
	}
}

func TestOpenAIStream_EmitsRefusedOnContentFilter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(streamResponse([]string{"I can't ", "help"}, "content_filter", true)))
	}))
	defer server.Close()

	sink := &recordingEventSink{}
	p := NewOpenAIProvider("openai", server.URL, "test-key", nil, WithOpenAISessionEventSink(sink))
	stream, err := p.Stream(context.Background(), &CompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: RoleUser, Content: "x"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drainStream(t, stream)
	_ = stream.Close()

	got := sink.snapshot()
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2; types=%v", len(got), sink.types())
	}
	if got[1].EventType != events.TypeLLMRefused {
		t.Errorf("events[1] = %q, want %q (content_filter on a streamed response classifies as refusal)",
			got[1].EventType, events.TypeLLMRefused)
	}
	var refused events.LLMRefusedPayload
	_ = json.Unmarshal(got[1].Payload, &refused)
	if refused.RefusalText != "I can't help" {
		t.Errorf("RefusalText = %q", refused.RefusalText)
	}
}

func TestOpenAIStream_EmitsErrorOnHTTPStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	defer server.Close()

	sink := &recordingEventSink{}
	p := NewOpenAIProvider("openai", server.URL, "test-key", nil, WithOpenAISessionEventSink(sink))
	_, err := p.Stream(context.Background(), &CompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: RoleUser, Content: "x"}},
	})
	if err == nil {
		t.Fatal("Stream must error on 500")
	}

	got := sink.snapshot()
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (llm_request + llm_error); types=%v", len(got), sink.types())
	}
	if got[1].EventType != events.TypeLLMError {
		t.Errorf("events[1] = %q, want %q", got[1].EventType, events.TypeLLMError)
	}
	var perr events.LLMErrorPayload
	_ = json.Unmarshal(got[1].Payload, &perr)
	if perr.Phase != phaseStreamHTTPStatus {
		t.Errorf("Phase = %q, want %q (stream-side status error must be distinct from chat.http_status)",
			perr.Phase, phaseStreamHTTPStatus)
	}
	if perr.StatusCode != 500 {
		t.Errorf("StatusCode = %d", perr.StatusCode)
	}
}

func TestOpenAIStream_EmitsErrorOnChunkError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"error":{"message":"chunk explode","type":"server_error"}}` + "\n\n"))
	}))
	defer server.Close()

	sink := &recordingEventSink{}
	p := NewOpenAIProvider("openai", server.URL, "test-key", nil, WithOpenAISessionEventSink(sink))
	stream, err := p.Stream(context.Background(), &CompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: RoleUser, Content: "x"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	// Drain — Recv will surface the chunk error.
	for {
		_, err := stream.Recv()
		if err != nil {
			break
		}
	}
	_ = stream.Close()

	got := sink.snapshot()
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2; types=%v", len(got), sink.types())
	}
	if got[1].EventType != events.TypeLLMError {
		t.Errorf("events[1] = %q, want %q (mid-stream errors emit llm_error from Close)",
			got[1].EventType, events.TypeLLMError)
	}
	var perr events.LLMErrorPayload
	_ = json.Unmarshal(got[1].Payload, &perr)
	if perr.Phase != phaseStreamRecv {
		t.Errorf("Phase = %q, want %q", perr.Phase, phaseStreamRecv)
	}
}

func TestOpenAIStream_DoubleCloseEmitsOnce(t *testing.T) {
	// The closed-bool guard on oaiResponseStream is the sole "emit-once"
	// guarantee. If Close is hit twice (orchestrator defer + caller
	// defer), the second call must be a no-op for the event sink too.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(streamResponse([]string{"ok"}, "stop", true)))
	}))
	defer server.Close()

	sink := &recordingEventSink{}
	p := NewOpenAIProvider("openai", server.URL, "test-key", nil, WithOpenAISessionEventSink(sink))
	stream, err := p.Stream(context.Background(), &CompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: RoleUser, Content: "x"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drainStream(t, stream)
	_ = stream.Close()
	_ = stream.Close() // must not re-emit

	got := sink.snapshot()
	if len(got) != 2 {
		t.Fatalf("got %d events on double Close, want 2; types=%v", len(got), sink.types())
	}
}

func TestNewOpenAIProvider_NoSinkOptionDefaultsToNoOp(t *testing.T) {
	// Constructing without WithOpenAISessionEventSink must NOT panic at
	// emission time: the field defaults to emit.NoOpSink{}. Belt-and-
	// braces — also confirms no nil-Sink reaches the helpers.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := oaiResponse{
			ID: "x", Model: "gpt-4o",
			Choices: []oaiChoice{{Message: oaiMessage{Role: "assistant", Content: "ok"}, FinishReason: "stop"}},
			Usage:   oaiUsage{PromptTokens: 1, CompletionTokens: 1},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	p := NewOpenAIProvider("openai", server.URL, "test-key", nil)
	if _, err := p.Complete(context.Background(), &CompletionRequest{
		Model: "gpt-4o", Messages: []Message{{Role: RoleUser, Content: "Hi"}},
	}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
}

func TestWithOpenAISessionEventSink_NilArgReplacedByNoOp(t *testing.T) {
	// Defensive contract: WithOpenAISessionEventSink(nil) is valid and
	// replaces the field with NoOpSink rather than leaving it nil.
	p := NewOpenAIProvider("openai", "http://localhost", "key", nil, WithOpenAISessionEventSink(nil))
	if p.eventSink == nil {
		t.Error("eventSink must not be nil after WithOpenAISessionEventSink(nil); want NoOpSink")
	}
}

// ----- Stream edge cases -----

func TestOpenAIStream_RefusalWithEmptyContent(t *testing.T) {
	// A model that issues a content_filter finish_reason with NO
	// content deltas at all (it refused before any token streamed)
	// must still produce an llm_refused event — with an empty
	// RefusalText. Analytics consumers may filter on the type, not
	// the text, so the absence of text is meaningful information.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(streamResponse(nil, "content_filter", true)))
	}))
	defer server.Close()

	sink := &recordingEventSink{}
	p := NewOpenAIProvider("openai", server.URL, "test-key", nil, WithOpenAISessionEventSink(sink))
	stream, err := p.Stream(context.Background(), &CompletionRequest{
		Model: "gpt-4o", Messages: []Message{{Role: RoleUser, Content: "x"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drainStream(t, stream)
	_ = stream.Close()

	got := sink.snapshot()
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2; types=%v", len(got), sink.types())
	}
	if got[1].EventType != events.TypeLLMRefused {
		t.Errorf("events[1] = %q, want llm_refused", got[1].EventType)
	}
	var refused events.LLMRefusedPayload
	_ = json.Unmarshal(got[1].Payload, &refused)
	if refused.RefusalText != "" {
		t.Errorf("RefusalText = %q, want empty (no content deltas streamed before refusal)", refused.RefusalText)
	}
	if refused.ContentSafetyHit != "content_filter" {
		t.Errorf("ContentSafetyHit = %q", refused.ContentSafetyHit)
	}
}

func TestOpenAIStream_ContentDeltaThenErrorEmitsError(t *testing.T) {
	// Content delta arrives first, then a chunk-level error mid-stream.
	// emitStreamEnd must prefer the error path (llm_error with
	// stream.recv phase) over the partial-content path (llm_response).
	// Otherwise a transient mid-stream failure would be misattributed
	// as a successful generation with truncated content.
	body := strings.Builder{}
	chunk := map[string]any{
		"id":    "chatcmpl-stream",
		"model": "gpt-4o",
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]string{"content": "partial answer"},
			"finish_reason": nil,
		}},
	}
	raw, _ := json.Marshal(chunk)
	body.WriteString("data: ")
	body.Write(raw)
	body.WriteString("\n\ndata: {\"error\":{\"message\":\"mid-stream boom\",\"type\":\"server_error\"}}\n\n")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body.String()))
	}))
	defer server.Close()

	sink := &recordingEventSink{}
	p := NewOpenAIProvider("openai", server.URL, "test-key", nil, WithOpenAISessionEventSink(sink))
	stream, err := p.Stream(context.Background(), &CompletionRequest{
		Model: "gpt-4o", Messages: []Message{{Role: RoleUser, Content: "x"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for {
		_, recvErr := stream.Recv()
		if recvErr != nil {
			break
		}
	}
	_ = stream.Close()

	got := sink.snapshot()
	if len(got) != 2 {
		t.Fatalf("got %d events; types=%v", len(got), sink.types())
	}
	if got[1].EventType != events.TypeLLMError {
		t.Errorf("events[1] = %q, want llm_error (mid-stream error must outrank partial content)", got[1].EventType)
	}
	var perr events.LLMErrorPayload
	_ = json.Unmarshal(got[1].Payload, &perr)
	if perr.Phase != phaseStreamRecv {
		t.Errorf("Phase = %q, want %q", perr.Phase, phaseStreamRecv)
	}
	if !strings.Contains(perr.ResponseBodyExcerpt, "mid-stream boom") {
		t.Errorf("ResponseBodyExcerpt = %q, want substring 'mid-stream boom'", perr.ResponseBodyExcerpt)
	}
}

func TestOpenAIStream_NoFinishReasonEmitsResponseWithEmpty(t *testing.T) {
	// A clean stream (terminated by [DONE]) that never carries a
	// finish_reason chunk and never carries a usage chunk should still
	// emit llm_response (not llm_error) with FinishReason == "" and
	// token counts == 0. This is the "finished response with missing
	// metadata" mode — analytics needs to see it tagged correctly, not
	// silently reclassified as an error.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// streamResponse always appends "data: [DONE]\n\n". Passing
		// finishReason="" and includeUsage=false omits the
		// metadata-bearing chunks but keeps the [DONE] terminator so
		// the scanner exits cleanly (not via scanner-error).
		_, _ = w.Write([]byte(streamResponse([]string{"partial"}, "", false)))
	}))
	defer server.Close()

	sink := &recordingEventSink{}
	p := NewOpenAIProvider("openai", server.URL, "test-key", nil, WithOpenAISessionEventSink(sink))
	stream, err := p.Stream(context.Background(), &CompletionRequest{
		Model: "gpt-4o", Messages: []Message{{Role: RoleUser, Content: "x"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drainStream(t, stream)
	_ = stream.Close()

	got := sink.snapshot()
	if len(got) != 2 {
		t.Fatalf("got %d events; types=%v", len(got), sink.types())
	}
	if got[1].EventType != events.TypeLLMResponse {
		t.Errorf("events[1] = %q, want llm_response (no finish_reason should NOT be classified as error)",
			got[1].EventType)
	}
	var resp events.LLMResponsePayload
	_ = json.Unmarshal(got[1].Payload, &resp)
	if resp.FinishReason != "" {
		t.Errorf("FinishReason = %q, want empty (no finish_reason chunk arrived)", resp.FinishReason)
	}
	if resp.RawContentExcerpt != "partial" {
		t.Errorf("RawContentExcerpt = %q (deltas before truncation must persist)", resp.RawContentExcerpt)
	}
}

func TestOpenAIStream_CloseWithoutRecvUsesNowFallback(t *testing.T) {
	// Reaches the `if endpoint.IsZero() { endpoint = time.Now() }`
	// fallback in emitStreamEnd: Stream() returns a stream, the
	// orchestrator never reads any chunk (e.g. immediately abandoned
	// because the outer turn cancelled), then Close is called. Without
	// the fallback, latency would be the literal value 0 and analytics
	// dashboards would see a phantom zero-latency row.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(streamResponse([]string{"unused"}, "stop", true)))
	}))
	defer server.Close()

	sink := &recordingEventSink{}
	p := NewOpenAIProvider("openai", server.URL, "test-key", nil, WithOpenAISessionEventSink(sink))
	stream, err := p.Stream(context.Background(), &CompletionRequest{
		Model: "gpt-4o", Messages: []Message{{Role: RoleUser, Content: "x"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	// Deliberately do NOT call Recv. Brief sleep so time.Now() at Close
	// is provably after startTime at millisecond resolution.
	time.Sleep(2 * time.Millisecond)
	_ = stream.Close()

	got := sink.snapshot()
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2; types=%v", len(got), sink.types())
	}
	if got[1].EventType != events.TypeLLMResponse {
		t.Errorf("events[1] = %q, want llm_response (no streamErr, no content_filter)", got[1].EventType)
	}
	if got[1].DurationMS < 2 {
		t.Errorf("row DurationMS = %d, want >=2ms (fallback must use time.Now() so latency stays non-zero)",
			got[1].DurationMS)
	}
}

func TestOpenAIStream_FirstErrorWinsAcrossRecvRetries(t *testing.T) {
	// The streamErr guard claims "first error wins: a caller that
	// ignored the error and re-entered Recv must not overwrite the
	// original cause". The straight-line orchestrator wouldn't retry
	// after an error, but the invariant is load-bearing for the
	// emitted llm_error payload's accuracy and must not regress.
	body := strings.Builder{}
	body.WriteString("data: {\"error\":{\"message\":\"first failure\",\"type\":\"server_error\"}}\n\n")
	body.WriteString("data: {\"error\":{\"message\":\"second failure\",\"type\":\"server_error\"}}\n\n")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body.String()))
	}))
	defer server.Close()

	sink := &recordingEventSink{}
	p := NewOpenAIProvider("openai", server.URL, "test-key", nil, WithOpenAISessionEventSink(sink))
	stream, err := p.Stream(context.Background(), &CompletionRequest{
		Model: "gpt-4o", Messages: []Message{{Role: RoleUser, Content: "x"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	// Drain both error chunks. A defensive caller normally exits the
	// loop after the first error; this loop deliberately keeps reading
	// to exercise the guard.
	for i := 0; i < 2; i++ {
		_, err := stream.Recv()
		if err == nil {
			break
		}
	}
	_ = stream.Close()

	got := sink.snapshot()
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (request + first error)", len(got))
	}
	var perr events.LLMErrorPayload
	_ = json.Unmarshal(got[1].Payload, &perr)
	if !strings.Contains(perr.ResponseBodyExcerpt, "first failure") {
		t.Errorf("ResponseBodyExcerpt = %q, want substring 'first failure' (guard must preserve the original cause)",
			perr.ResponseBodyExcerpt)
	}
	if strings.Contains(perr.ResponseBodyExcerpt, "second failure") {
		t.Errorf("ResponseBodyExcerpt = %q must NOT contain 'second failure' (guard regression — first error must win)",
			perr.ResponseBodyExcerpt)
	}
}

func TestOpenAIStream_FinishReasonAndDeltaInSameChunk(t *testing.T) {
	// OpenAI can pack the final content delta AND finish_reason into a
	// single chunk. The accumulator must capture both rather than
	// treating "has finish_reason" as "no more content".
	body := strings.Builder{}
	chunk := map[string]any{
		"id":    "chatcmpl-stream",
		"model": "gpt-4o",
		"choices": []map[string]any{{
			"index":         0,
			"delta":         map[string]string{"content": "all in one"},
			"finish_reason": "stop",
		}},
	}
	raw, _ := json.Marshal(chunk)
	body.WriteString("data: ")
	body.Write(raw)
	body.WriteString("\n\ndata: [DONE]\n\n")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body.String()))
	}))
	defer server.Close()

	sink := &recordingEventSink{}
	p := NewOpenAIProvider("openai", server.URL, "test-key", nil, WithOpenAISessionEventSink(sink))
	stream, err := p.Stream(context.Background(), &CompletionRequest{
		Model: "gpt-4o", Messages: []Message{{Role: RoleUser, Content: "x"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	drainStream(t, stream)
	_ = stream.Close()

	got := sink.snapshot()
	var resp events.LLMResponsePayload
	_ = json.Unmarshal(got[1].Payload, &resp)
	if resp.RawContentExcerpt != "all in one" {
		t.Errorf("RawContentExcerpt = %q (combined chunk must propagate both fields)", resp.RawContentExcerpt)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q", resp.FinishReason)
	}
}

// ----- test helpers shared by the error-path tests above -----

// roundTripperFunc adapts a closure into an http.RoundTripper so the
// chat.read_response path can be exercised without a live server.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// erroringReader returns err on the first Read so io.ReadAll surfaces
// it to the provider's body-read path.
type erroringReader struct{ err error }

func (r *erroringReader) Read(_ []byte) (int, error) { return 0, r.err }
