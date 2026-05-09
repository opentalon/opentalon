package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIComplete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type = %q", r.Header.Get("Content-Type"))
		}

		var req oaiRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Model != "gpt-4o" {
			t.Errorf("model = %q", req.Model)
		}
		if len(req.Messages) != 2 {
			t.Errorf("messages = %d", len(req.Messages))
		}
		if req.Messages[0].Role != "system" {
			t.Errorf("messages[0].role = %q", req.Messages[0].Role)
		}

		resp := oaiResponse{
			ID:    "chatcmpl-123",
			Model: "gpt-4o",
			Choices: []oaiChoice{
				{Index: 0, Message: oaiMessage{Role: "assistant", Content: "Hello! How can I help?"}},
			},
			Usage: oaiUsage{PromptTokens: 10, CompletionTokens: 5},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	p := NewOpenAIProvider("openai", server.URL, "test-key", nil)

	resp, err := p.Complete(context.Background(), &CompletionRequest{
		Model: "gpt-4o",
		Messages: []Message{
			{Role: RoleSystem, Content: "You are helpful."},
			{Role: RoleUser, Content: "Hi"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if resp.ID != "chatcmpl-123" {
		t.Errorf("id = %q", resp.ID)
	}
	if resp.Content != "Hello! How can I help?" {
		t.Errorf("content = %q", resp.Content)
	}
	if resp.Usage.InputTokens != 10 {
		t.Errorf("input_tokens = %d", resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens != 5 {
		t.Errorf("output_tokens = %d", resp.Usage.OutputTokens)
	}
}

func TestOpenAICompleteAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`))
	}))
	defer server.Close()

	p := NewOpenAIProvider("openai", server.URL, "key", nil)

	_, err := p.Complete(context.Background(), &CompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: RoleUser, Content: "Hi"}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestOpenAICompleteErrorInBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := oaiResponse{
			Error: &oaiError{Type: "invalid_request_error", Message: "bad model"},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	p := NewOpenAIProvider("openai", server.URL, "key", nil)

	_, err := p.Complete(context.Background(), &CompletionRequest{
		Model:    "bad-model",
		Messages: []Message{{Role: RoleUser, Content: "Hi"}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestOpenAINoAPIKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("should not send Authorization header when no API key")
		}
		resp := oaiResponse{
			ID:      "local-1",
			Model:   "llama3",
			Choices: []oaiChoice{{Message: oaiMessage{Content: "ok"}}},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	p := NewOpenAIProvider("ollama", server.URL, "", nil)

	resp, err := p.Complete(context.Background(), &CompletionRequest{
		Model:    "llama3",
		Messages: []Message{{Role: RoleUser, Content: "Hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "ok" {
		t.Errorf("content = %q", resp.Content)
	}
}

func TestOpenAIProviderInterface(t *testing.T) {
	models := []ModelInfo{
		{ID: "gpt-4o", ProviderID: "openai", Features: []Feature{FeatureStreaming, FeatureTools}},
	}
	p := NewOpenAIProvider("openai", "", "key", models)

	if p.ID() != "openai" {
		t.Errorf("id = %q", p.ID())
	}
	if len(p.Models()) != 1 {
		t.Errorf("models = %d", len(p.Models()))
	}
	if !p.SupportsFeature(FeatureTools) {
		t.Error("should support tools")
	}
	if p.SupportsFeature(FeatureReasoning) {
		t.Error("should not support reasoning")
	}
}

func TestOpenAIDefaultBaseURL(t *testing.T) {
	p := NewOpenAIProvider("openai", "", "key", nil)
	if p.baseURL != openAIDefaultBaseURL {
		t.Errorf("baseURL = %q, want %q", p.baseURL, openAIDefaultBaseURL)
	}
}

func TestOpenAITrailingSlash(t *testing.T) {
	p := NewOpenAIProvider("openai", "https://api.example.com/v1/", "key", nil)
	if p.baseURL != "https://api.example.com/v1" {
		t.Errorf("baseURL = %q, should strip trailing slash", p.baseURL)
	}
}

func TestOpenAIStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req oaiRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if !req.Stream {
			t.Error("expected stream=true")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("server does not support flushing")
		}

		chunks := []string{
			`{"id":"c1","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
			`{"id":"c1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`,
			`{"id":"c1","model":"gpt-4o","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}`,
			`{"id":"c1","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		}
		for _, c := range chunks {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", c)
			flusher.Flush()
		}
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	p := NewOpenAIProvider("openai", server.URL, "test-key", nil)

	stream, err := p.Stream(context.Background(), &CompletionRequest{
		Model: "gpt-4o",
		Messages: []Message{
			{Role: RoleUser, Content: "Hi"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()

	var got strings.Builder
	for {
		chunk, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if chunk.Done {
			break
		}
		got.WriteString(chunk.Content)
	}

	if got.String() != "Hello world" {
		t.Errorf("streamed content = %q, want %q", got.String(), "Hello world")
	}
}

func TestOpenAIStreamHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer server.Close()

	p := NewOpenAIProvider("openai", server.URL, "key", nil)

	_, err := p.Stream(context.Background(), &CompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: RoleUser, Content: "Hi"}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestOpenAIStreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"error":{"type":"server_error","message":"internal error"}}`)
	}))
	defer server.Close()

	p := NewOpenAIProvider("openai", server.URL, "key", nil)

	stream, err := p.Stream(context.Background(), &CompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: RoleUser, Content: "Hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()

	_, err = stream.Recv()
	if err == nil {
		t.Fatal("expected error from stream chunk")
	}
}

func TestOpenAIRejectsFileAttachments(t *testing.T) {
	p := NewOpenAIProvider("openai", "", "key", nil)

	_, err := p.Complete(context.Background(), &CompletionRequest{
		Model: "gpt-4o",
		Messages: []Message{
			{Role: RoleUser, Content: "see attached", Files: []MessageFile{
				{MimeType: "text/csv", Data: []byte("a,b\n1,2")},
			}},
		},
	})
	if err == nil {
		t.Fatal("expected error for file attachment, got nil")
	}
}

func TestNativeArgToString(t *testing.T) {
	cases := []struct {
		name string
		v    interface{}
		want string
	}{
		{"string", "hello", "hello"},
		{"bool true", true, "true"},
		{"bool false", false, "false"},
		{"nil", nil, "null"},
		{"integer float64", float64(42), "42"},
		{"large integer float64", float64(2037838), "2037838"},
		{"fractional float64", float64(3.14), "3.14"},
		{"array", []interface{}{"all"}, `["all"]`},
		{"nested object", map[string]interface{}{"status": "active"}, `{"status":"active"}`},
		{"empty array", []interface{}{}, `[]`},
		{"integer array", []interface{}{float64(1), float64(2)}, `[1,2]`},
		{"zero float64", float64(0), "0"},
		{"json.Number", json.Number("999"), "999"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := nativeArgToString(tc.v)
			if got != tc.want {
				t.Errorf("nativeArgToString(%v) = %q, want %q", tc.v, got, tc.want)
			}
		})
	}
}

// withSlogCapture redirects the package-default slog handler at the given
// level to a buffer and returns the buffer + a restore func. Mirrors the
// JSONHandler used in production so tests assert against the actual log
// shape operators see.
func withSlogCapture(t *testing.T, level slog.Level) (*bytes.Buffer, func()) {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: level})))
	return &buf, func() { slog.SetDefault(prev) }
}

// rawHTTPRecord parses one captured "openai raw http" event from the JSON
// log buffer, returning its parsed `body` and the surrounding attrs.
// Test helper — fails the calling test if the matching record isn't
// present or its shape is wrong.
func rawHTTPRecord(t *testing.T, buf *bytes.Buffer, direction string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["msg"] != "openai raw http" {
			continue
		}
		if rec["direction"] != direction {
			continue
		}
		return rec
	}
	t.Fatalf("no openai raw http record with direction=%q in:\n%s", direction, buf.String())
	return nil
}

// TestOpenAIComplete_RawHTTPLogAtDebug pins the contract that operators
// rely on for the live A/B comparison: at LOG_LEVEL=debug, the full
// request body and full response body are emitted on stderr (kubectl
// logs) under a single "openai raw http" message tag with a `direction`
// attribute, with `body` as a nested JSON object — `kubectl logs -f
// opentalon-0 | jq 'select(.msg=="openai raw http")'` is the supported
// live tail.
func TestOpenAIComplete_RawHTTPLogAtDebug(t *testing.T) {
	buf, restore := withSlogCapture(t, slog.LevelDebug)
	defer restore()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(oaiResponse{
			ID:      "chatcmpl-raw",
			Model:   "gpt-4o",
			Choices: []oaiChoice{{Index: 0, Message: oaiMessage{Role: "assistant", Content: "ok"}}},
		})
	}))
	defer server.Close()

	p := NewOpenAIProvider("openai", server.URL, "test-key", nil)
	if _, err := p.Complete(context.Background(), &CompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: RoleUser, Content: "Hi"}},
	}); err != nil {
		t.Fatal(err)
	}

	// Request side — body must be a nested JSON object; a request_body
	// rendered as a quote-escaped string would defeat the purpose of the
	// JSON log handler. We assert on shape, not on string substring.
	reqRec := rawHTTPRecord(t, buf, "request")
	reqBody, ok := reqRec["body"].(map[string]any)
	if !ok {
		t.Fatalf("request body is not a JSON object: %v (%T)", reqRec["body"], reqRec["body"])
	}
	if reqBody["model"] != "gpt-4o" {
		t.Errorf("request body model = %v, want gpt-4o", reqBody["model"])
	}
	msgs, ok := reqBody["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("request body messages missing or wrong type: %v (%T)", reqBody["messages"], reqBody["messages"])
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "user" || first["content"] != "Hi" {
		t.Errorf("first message = %v, want role=user content=Hi", first)
	}

	// Response side — same nested-JSON shape expectation.
	respRec := rawHTTPRecord(t, buf, "response")
	if respRec["status"].(float64) != 200 {
		t.Errorf("response status = %v, want 200", respRec["status"])
	}
	respBody, ok := respRec["body"].(map[string]any)
	if !ok {
		t.Fatalf("response body is not a JSON object: %v (%T)", respRec["body"], respRec["body"])
	}
	if respBody["id"] != "chatcmpl-raw" {
		t.Errorf("response body id = %v, want chatcmpl-raw", respBody["id"])
	}
}

// TestOpenAIComplete_RawHTTPLogSilentAtInfo guarantees the capture is
// gated by LOG_LEVEL — operators can leave the patched binary running
// at the default `info` level without stderr getting flooded with
// 30 KB request bodies.
func TestOpenAIComplete_RawHTTPLogSilentAtInfo(t *testing.T) {
	buf, restore := withSlogCapture(t, slog.LevelInfo)
	defer restore()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(oaiResponse{
			Choices: []oaiChoice{{Index: 0, Message: oaiMessage{Role: "assistant", Content: "ok"}}},
		})
	}))
	defer server.Close()

	p := NewOpenAIProvider("openai", server.URL, "test-key", nil)
	if _, err := p.Complete(context.Background(), &CompletionRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: RoleUser, Content: "Hi"}},
	}); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(buf.String(), "openai raw http") {
		t.Errorf("info-level log must not include raw http capture; got:\n%s", buf.String())
	}
}

// TestRawJSONOrString covers the response-body fallback: valid JSON keeps
// its shape, anything else (HTML error page, truncated chunk) survives
// as a literal string instead of vanishing.
func TestRawJSONOrString(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want any
	}{
		{"json object", `{"a":1}`, json.RawMessage(`{"a":1}`)},
		{"json array", `[1,2,3]`, json.RawMessage(`[1,2,3]`)},
		{"html error page", `<html><body>500</body></html>`, "<html><body>500</body></html>"},
		{"truncated json", `{"a":`, `{"a":`},
		{"empty", ``, ``},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := rawJSONOrString([]byte(c.in))
			switch w := c.want.(type) {
			case json.RawMessage:
				rm, ok := got.(json.RawMessage)
				if !ok {
					t.Fatalf("got %T, want json.RawMessage", got)
				}
				if string(rm) != string(w) {
					t.Errorf("got %q, want %q", string(rm), string(w))
				}
			case string:
				s, ok := got.(string)
				if !ok {
					t.Fatalf("got %T (%v), want string", got, got)
				}
				if s != w {
					t.Errorf("got %q, want %q", s, w)
				}
			}
		})
	}
}
