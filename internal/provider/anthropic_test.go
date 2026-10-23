package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/state/store/events"
)

func TestAnthropicComplete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "sk-ant-test" {
			t.Errorf("x-api-key = %q", r.Header.Get("x-api-key"))
		}
		if r.Header.Get("anthropic-version") != anthropicAPIVersion {
			t.Errorf("anthropic-version = %q", r.Header.Get("anthropic-version"))
		}

		var req anthRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Model != "claude-sonnet-4-20250514" {
			t.Errorf("model = %q", req.Model)
		}
		if got := systemText(t, req.System); got != "You are helpful." {
			t.Errorf("system = %q", got)
		}
		if len(req.Messages) != 1 {
			t.Fatalf("messages = %d, want 1 (system extracted)", len(req.Messages))
		}
		if req.Messages[0].Role != "user" {
			t.Errorf("messages[0].role = %q", req.Messages[0].Role)
		}
		if req.MaxTokens != 4096 {
			t.Errorf("max_tokens = %d, want 4096 (default)", req.MaxTokens)
		}

		resp := anthResponse{
			ID:    "msg_01ABC",
			Model: "claude-sonnet-4-20250514",
			Content: []anthContentBlock{
				{Type: "text", Text: "Hello! I can help with that."},
			},
			Usage: anthUsage{InputTokens: 15, OutputTokens: 8},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	p := NewAnthropicProvider("anthropic", server.URL, "sk-ant-test", nil)

	resp, err := p.Complete(context.Background(), &CompletionRequest{
		Model: "claude-sonnet-4-20250514",
		Messages: []Message{
			{Role: RoleSystem, Content: "You are helpful."},
			{Role: RoleUser, Content: "Hi"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	if resp.ID != "msg_01ABC" {
		t.Errorf("id = %q", resp.ID)
	}
	if resp.Content != "Hello! I can help with that." {
		t.Errorf("content = %q", resp.Content)
	}
	if resp.Usage.InputTokens != 15 {
		t.Errorf("input_tokens = %d", resp.Usage.InputTokens)
	}
	if resp.Usage.OutputTokens != 8 {
		t.Errorf("output_tokens = %d", resp.Usage.OutputTokens)
	}
}

// systemText decodes a request's system field sent as one string.
func systemText(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Errorf("system field is not a string: %s", raw)
	}
	return s
}

func TestAnthropicMultipleContentBlocks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := anthResponse{
			ID:    "msg_02",
			Model: "claude-sonnet-4-20250514",
			Content: []anthContentBlock{
				{Type: "text", Text: "First part."},
				{Type: "text", Text: "Second part."},
			},
			Usage: anthUsage{InputTokens: 5, OutputTokens: 10},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	p := NewAnthropicProvider("anthropic", server.URL, "key", nil)

	resp, err := p.Complete(context.Background(), &CompletionRequest{
		Model:    "claude-sonnet-4-20250514",
		Messages: []Message{{Role: RoleUser, Content: "Hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "First part.\n\nSecond part." {
		t.Errorf("content = %q", resp.Content)
	}
}

func TestAnthropicAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"invalid api key"}}`))
	}))
	defer server.Close()

	p := NewAnthropicProvider("anthropic", server.URL, "bad-key", nil)

	_, err := p.Complete(context.Background(), &CompletionRequest{
		Model:    "claude-sonnet-4-20250514",
		Messages: []Message{{Role: RoleUser, Content: "Hi"}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestAnthropicErrorInBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := anthResponse{
			Error: &anthError{Type: "invalid_request_error", Message: "model not found"},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	p := NewAnthropicProvider("anthropic", server.URL, "key", nil)

	_, err := p.Complete(context.Background(), &CompletionRequest{
		Model:    "nonexistent",
		Messages: []Message{{Role: RoleUser, Content: "Hi"}},
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestAnthropicCustomMaxTokens(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req anthRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.MaxTokens != 1024 {
			t.Errorf("max_tokens = %d, want 1024", req.MaxTokens)
		}

		resp := anthResponse{
			ID:      "msg_03",
			Model:   "claude-haiku-3",
			Content: []anthContentBlock{{Type: "text", Text: "ok"}},
			Usage:   anthUsage{InputTokens: 1, OutputTokens: 1},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	p := NewAnthropicProvider("anthropic", server.URL, "key", nil)

	_, err := p.Complete(context.Background(), &CompletionRequest{
		Model:     "claude-haiku-3",
		MaxTokens: 1024,
		Messages:  []Message{{Role: RoleUser, Content: "Hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestAnthropicMaxTokens covers the max_tokens sent: the request's, else the
// model's, else 4096; a thinking budget raises it as before.
func TestAnthropicMaxTokens(t *testing.T) {
	cases := []struct {
		name           string
		modelMaxTokens int
		maxTokens      int
		reasoning      bool
		want           int
	}{
		{"the model's max_tokens", 2048, 0, false, 2048},
		{"4096 without either", 0, 0, false, 4096},
		{"the request's max_tokens over the model's", 2048, 1024, false, 1024},
		{"the model's max_tokens raised for a thinking budget", 2048, 0, true, 14096},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bodies, err := anthropicBodies(t, []ModelInfo{{ID: "claude-x", MaxTokens: tc.modelMaxTokens}},
				&CompletionRequest{
					Model:     "claude-x",
					Messages:  []Message{{Role: RoleUser, Content: "Hi"}},
					MaxTokens: tc.maxTokens,
					Reasoning: tc.reasoning,
				}, http.StatusOK)
			if err != nil {
				t.Fatal(err)
			}
			if len(bodies) != 1 {
				t.Fatalf("requests = %d, want 1", len(bodies))
			}
			var sent anthRequest
			if err := json.Unmarshal([]byte(bodies[0]), &sent); err != nil {
				t.Fatal(err)
			}
			if sent.MaxTokens != tc.want {
				t.Errorf("max_tokens = %d, want %d", sent.MaxTokens, tc.want)
			}
		})
	}
}

func TestAnthropicProviderInterface(t *testing.T) {
	models := []ModelInfo{
		{ID: "claude-sonnet-4-20250514", ProviderID: "anthropic", Features: []Feature{FeatureStreaming, FeatureReasoning}},
	}
	p := NewAnthropicProvider("anthropic", "", "key", models)

	if p.ID() != "anthropic" {
		t.Errorf("id = %q", p.ID())
	}
	if len(p.Models()) != 1 {
		t.Errorf("models = %d", len(p.Models()))
	}
	if !p.SupportsFeature(FeatureReasoning) {
		t.Error("should support reasoning")
	}
	if p.SupportsFeature(FeatureImages) {
		t.Error("should not support images")
	}
}

func TestAnthropicDefaultBaseURL(t *testing.T) {
	p := NewAnthropicProvider("anthropic", "", "key", nil)
	if p.baseURL != anthropicDefaultBaseURL {
		t.Errorf("baseURL = %q", p.baseURL)
	}
}

func TestToAnthMessageFileBlocks(t *testing.T) {
	p := NewAnthropicProvider("anthropic", "", "key", nil)

	tests := []struct {
		name           string
		mimeType       string
		data           []byte
		wantBlockType  string
		wantSourceType string // "base64", "text", or "" (no source)
		wantMediaType  string // expected source.media_type; empty means omitted
		wantData       string // expected source.data
	}{
		{
			name:           "image becomes image block with base64 source",
			mimeType:       "image/png",
			data:           []byte{0x89, 0x50, 0x4e, 0x47},
			wantBlockType:  "image",
			wantSourceType: "base64",
			wantMediaType:  "image/png",
		},
		{
			name:           "pdf becomes document block with base64 source",
			mimeType:       "application/pdf",
			data:           []byte("%PDF-1.4"),
			wantBlockType:  "document",
			wantSourceType: "base64",
			wantMediaType:  "application/pdf",
		},
		{
			name:           "csv becomes document block with text source",
			mimeType:       "text/csv",
			data:           []byte("a,b,c\n1,2,3"),
			wantBlockType:  "document",
			wantSourceType: "text",
			wantMediaType:  "text/plain",
			wantData:       "a,b,c\n1,2,3",
		},
		{
			name:           "plain text becomes document block with text source",
			mimeType:       "text/plain",
			data:           []byte("hello"),
			wantBlockType:  "document",
			wantSourceType: "text",
			wantMediaType:  "text/plain",
			wantData:       "hello",
		},
		{
			name:           "application/json becomes document block with text source",
			mimeType:       "application/json",
			data:           []byte(`{"k":"v"}`),
			wantBlockType:  "document",
			wantSourceType: "text",
			wantMediaType:  "text/plain",
			wantData:       `{"k":"v"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := p.toAnthMessage(Message{
				Role:    RoleUser,
				Content: "see attached",
				Files:   []MessageFile{{MimeType: tc.mimeType, Data: tc.data}},
			})
			if err != nil {
				t.Fatal(err)
			}

			var blocks []anthContentBlock
			if err := json.Unmarshal(msg.Content, &blocks); err != nil {
				t.Fatal(err)
			}
			// blocks[0] is the file block, blocks[1] is the text message
			if len(blocks) != 2 {
				t.Fatalf("blocks = %d, want 2", len(blocks))
			}
			b := blocks[0]
			if b.Type != tc.wantBlockType {
				t.Errorf("block type = %q, want %q", b.Type, tc.wantBlockType)
			}
			if b.Source == nil {
				t.Fatal("expected source, got nil")
			}
			if b.Source.Type != tc.wantSourceType {
				t.Errorf("source type = %q, want %q", b.Source.Type, tc.wantSourceType)
			}
			if b.Source.MediaType != tc.wantMediaType {
				t.Errorf("media_type = %q, want %q", b.Source.MediaType, tc.wantMediaType)
			}
			if tc.wantData != "" && b.Source.Data != tc.wantData {
				t.Errorf("source data = %q, want %q", b.Source.Data, tc.wantData)
			}
		})
	}
}

func TestToAnthMessageUnsupportedMimeType(t *testing.T) {
	p := NewAnthropicProvider("anthropic", "", "key", nil)

	unsupported := []string{"application/zip", "application/octet-stream", "audio/mpeg", "video/mp4"}
	for _, mime := range unsupported {
		t.Run(mime, func(t *testing.T) {
			_, err := p.toAnthMessage(Message{
				Role:  RoleUser,
				Files: []MessageFile{{MimeType: mime, Data: []byte{0x00, 0x01}}},
			})
			if err == nil {
				t.Errorf("expected error for mime type %q, got nil", mime)
			}
		})
	}
}

func TestToAnthMessageMixedFiles(t *testing.T) {
	p := NewAnthropicProvider("anthropic", "", "key", nil)

	msg, err := p.toAnthMessage(Message{
		Role:    RoleUser,
		Content: "review these",
		Files: []MessageFile{
			{MimeType: "image/jpeg", Data: []byte{0xff, 0xd8}},
			{MimeType: "application/pdf", Data: []byte("%PDF")},
			{MimeType: "text/csv", Data: []byte("x,y\n1,2")},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var blocks []anthContentBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		t.Fatal(err)
	}
	// 3 file blocks + 1 text block
	if len(blocks) != 4 {
		t.Fatalf("blocks = %d, want 4", len(blocks))
	}
	if blocks[0].Type != "image" {
		t.Errorf("blocks[0] type = %q, want image", blocks[0].Type)
	}
	if blocks[1].Type != "document" || blocks[1].Source.Type != "base64" {
		t.Errorf("blocks[1]: type=%q source.type=%q, want document/base64", blocks[1].Type, blocks[1].Source.Type)
	}
	if blocks[2].Type != "document" || blocks[2].Source.Type != "text" {
		t.Errorf("blocks[2]: type=%q source.type=%q, want document/text", blocks[2].Type, blocks[2].Source.Type)
	}
	if blocks[3].Type != "text" || blocks[3].Text != "review these" {
		t.Errorf("blocks[3]: type=%q text=%q, want text/review these", blocks[3].Type, blocks[3].Text)
	}
}

func TestToAnthMessageEmptyContent(t *testing.T) {
	p := NewAnthropicProvider("anthropic", "", "key", nil)

	msg, err := p.toAnthMessage(Message{
		Role:    RoleUser,
		Content: "",
		Files:   []MessageFile{{MimeType: "text/csv", Data: []byte("a,b")}},
	})
	if err != nil {
		t.Fatal(err)
	}

	var blocks []anthContentBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		t.Fatal(err)
	}
	// empty Content must not produce a trailing text block
	if len(blocks) != 1 {
		t.Fatalf("blocks = %d, want 1 (no trailing text block for empty content)", len(blocks))
	}
	if blocks[0].Type != "document" {
		t.Errorf("blocks[0] type = %q, want document", blocks[0].Type)
	}
}

// --- native tool calling ---

// TestAnthropicToolsInRequest verifies the request side: ToolDefinitions map
// onto Anthropic's `tools` array with input_schema, and the fully-qualified
// "__"-separated tool name is sent verbatim (no wire encoding).
func TestAnthropicToolsInRequest(t *testing.T) {
	const fqn = "timly__timly__list-items"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req anthRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if len(req.Tools) != 1 {
			t.Fatalf("tools = %d, want 1", len(req.Tools))
		}
		if req.Tools[0].Name != fqn {
			t.Errorf("tool name = %q, want %q (verbatim, no encoding)", req.Tools[0].Name, fqn)
		}
		if req.Tools[0].InputSchema == nil {
			t.Error("input_schema missing")
		}
		resp := anthResponse{
			ID:      "msg_t",
			Model:   "claude-sonnet-4-20250514",
			Content: []anthContentBlock{{Type: "text", Text: "ok"}},
			Usage:   anthUsage{InputTokens: 1, OutputTokens: 1},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	p := NewAnthropicProvider("anthropic", server.URL, "key", nil)
	_, err := p.Complete(context.Background(), &CompletionRequest{
		Model:    "claude-sonnet-4-20250514",
		Messages: []Message{{Role: RoleUser, Content: "how many?"}},
		Tools: []ToolDefinition{{
			Name:        fqn,
			Description: "list items",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"limit": map[string]interface{}{"type": "string"}},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestAnthropicNoToolsOmitsField guards the "Anthropic 400s on empty tools"
// invariant: a request with no tools must omit the field entirely.
func TestAnthropicNoToolsOmitsField(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Fatal(err)
		}
		if _, present := raw["tools"]; present {
			t.Error("tools field present, want omitted when no tools")
		}
		resp := anthResponse{ID: "m", Content: []anthContentBlock{{Type: "text", Text: "ok"}}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	p := NewAnthropicProvider("anthropic", server.URL, "key", nil)
	if _, err := p.Complete(context.Background(), &CompletionRequest{
		Model:    "claude-sonnet-4-20250514",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
}

// TestAnthropicToolUseResponse verifies the response side: a tool_use content
// block parses into CompletionResponse.ToolCalls with the name passed through
// verbatim, the id preserved, and arguments flattened to map[string]string.
func TestAnthropicToolUseResponse(t *testing.T) {
	const fqn = "_meta__load_tools"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := anthResponse{
			ID:         "msg_tu",
			Model:      "claude-sonnet-4-20250514",
			StopReason: "tool_use",
			Content: []anthContentBlock{
				{Type: "text", Text: "Let me load that."},
				{
					Type:  "tool_use",
					ID:    "toolu_01",
					Name:  fqn,
					Input: json.RawMessage(`{"names":"timly__list-items","limit":5}`),
				},
			},
			Usage: anthUsage{InputTokens: 20, OutputTokens: 12},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	p := NewAnthropicProvider("anthropic", server.URL, "key", nil)
	resp, err := p.Complete(context.Background(), &CompletionRequest{
		Model:    "claude-sonnet-4-20250514",
		Messages: []Message{{Role: RoleUser, Content: "list my items"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "Let me load that." {
		t.Errorf("content = %q", resp.Content)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.Name != fqn {
		t.Errorf("tool name = %q, want %q (verbatim)", tc.Name, fqn)
	}
	if tc.ID != "toolu_01" {
		t.Errorf("tool id = %q", tc.ID)
	}
	if tc.Arguments["names"] != "timly__list-items" {
		t.Errorf("args[names] = %q", tc.Arguments["names"])
	}
	if tc.Arguments["limit"] != "5" {
		t.Errorf("args[limit] = %q, want \"5\" (number flattened)", tc.Arguments["limit"])
	}
}

// TestToAnthMessageToolResult verifies a RoleTool message becomes a role:user
// message carrying a single tool_result block keyed by the tool_use id.
func TestToAnthMessageToolResult(t *testing.T) {
	p := NewAnthropicProvider("anthropic", "", "key", nil)

	msg, err := p.toAnthMessage(Message{
		Role:       RoleTool,
		Content:    `{"count":952}`,
		ToolCallID: "toolu_01",
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Role != string(RoleUser) {
		t.Errorf("role = %q, want user (Anthropic carries tool results in user turns)", msg.Role)
	}
	var blocks []anthContentBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 1 || blocks[0].Type != "tool_result" {
		t.Fatalf("blocks = %+v, want one tool_result", blocks)
	}
	if blocks[0].ToolUseID != "toolu_01" {
		t.Errorf("tool_use_id = %q", blocks[0].ToolUseID)
	}
	var content string
	if err := json.Unmarshal(blocks[0].Content, &content); err != nil {
		t.Fatal(err)
	}
	if content != `{"count":952}` {
		t.Errorf("tool_result content = %q", content)
	}
}

// TestToAnthMessageAssistantToolUse verifies an assistant turn that invoked a
// tool replays as [optional text] + tool_use blocks with the name verbatim and
// the arguments re-shaped into the input object.
func TestToAnthMessageAssistantToolUse(t *testing.T) {
	p := NewAnthropicProvider("anthropic", "", "key", nil)
	const fqn = "timly__timly__list-items"

	msg, err := p.toAnthMessage(Message{
		Role:    RoleAssistant,
		Content: "Checking now.",
		ToolCalls: []ToolCall{{
			ID:        "toolu_42",
			Name:      fqn,
			Arguments: map[string]string{"limit": "5"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.Role != string(RoleAssistant) {
		t.Errorf("role = %q, want assistant", msg.Role)
	}
	var blocks []anthContentBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2 (text + tool_use)", len(blocks))
	}
	if blocks[0].Type != "text" || blocks[0].Text != "Checking now." {
		t.Errorf("blocks[0] = %+v, want text 'Checking now.'", blocks[0])
	}
	if blocks[1].Type != "tool_use" || blocks[1].Name != fqn || blocks[1].ID != "toolu_42" {
		t.Errorf("blocks[1] = %+v, want tool_use %q id toolu_42", blocks[1], fqn)
	}
	var input map[string]string
	if err := json.Unmarshal(blocks[1].Input, &input); err != nil {
		t.Fatal(err)
	}
	if input["limit"] != "5" {
		t.Errorf("input[limit] = %q", input["limit"])
	}
}

// TestAnthropicMultipleToolUseBlocks verifies that a response carrying more
// than one tool_use block parses into multiple ToolCalls, preserving each
// block's id, name, and flattened arguments in order.
func TestAnthropicMultipleToolUseBlocks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := anthResponse{
			ID:         "msg_multi",
			Model:      "claude-sonnet-4-20250514",
			StopReason: "tool_use",
			Content: []anthContentBlock{
				{Type: "text", Text: "Running both."},
				{
					Type:  "tool_use",
					ID:    "toolu_a",
					Name:  "timly__timly__list-items",
					Input: json.RawMessage(`{"limit":3}`),
				},
				{
					Type:  "tool_use",
					ID:    "toolu_b",
					Name:  "timly__timly__show-item",
					Input: json.RawMessage(`{"id":"42"}`),
				},
			},
			Usage: anthUsage{InputTokens: 9, OutputTokens: 14},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	p := NewAnthropicProvider("anthropic", server.URL, "key", nil)
	resp, err := p.Complete(context.Background(), &CompletionRequest{
		Model:    "claude-sonnet-4-20250514",
		Messages: []Message{{Role: RoleUser, Content: "do both"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("tool_calls = %d, want 2", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].ID != "toolu_a" || resp.ToolCalls[0].Name != "timly__timly__list-items" {
		t.Errorf("tool_calls[0] = %+v", resp.ToolCalls[0])
	}
	if resp.ToolCalls[0].Arguments["limit"] != "3" {
		t.Errorf("tool_calls[0].args[limit] = %q, want 3", resp.ToolCalls[0].Arguments["limit"])
	}
	if resp.ToolCalls[1].ID != "toolu_b" || resp.ToolCalls[1].Name != "timly__timly__show-item" {
		t.Errorf("tool_calls[1] = %+v", resp.ToolCalls[1])
	}
	if resp.ToolCalls[1].Arguments["id"] != "42" {
		t.Errorf("tool_calls[1].args[id] = %q, want 42", resp.ToolCalls[1].Arguments["id"])
	}
}

// TestAnthropicThinkingBlockExtraction verifies that a thinking content block
// carries its text in the `thinking` field (not `text`) and is captured by
// extractContent. Regression guard for reading the wrong field.
func TestAnthropicThinkingBlockExtraction(t *testing.T) {
	content, thinking, _, calls := (&AnthropicProvider{}).extractContent([]anthContentBlock{
		{Type: "thinking", Thinking: "step-by-step reasoning"},
		{Type: "text", Text: "final answer"},
	})
	if thinking != "step-by-step reasoning" {
		t.Errorf("thinking = %q, want %q (must read the thinking field, not text)", thinking, "step-by-step reasoning")
	}
	if content != "final answer" {
		t.Errorf("content = %q, want %q", content, "final answer")
	}
	if len(calls) != 0 {
		t.Errorf("tool_calls = %d, want 0", len(calls))
	}
}

// Thinking blocks of a tool-calling response, as Anthropic returns them: the
// signature and the redacted data are fields the provider does not model.
const (
	thinkingBlock         = `{"type":"thinking","thinking":"","signature":"sig-1"}`
	redactedThinkingBlock = `{"type":"redacted_thinking","data":"enc-1"}`
)

// TestAnthropicReplaysThinkingBlocks sends a conversation's next request
// after a response with thinking blocks and a tool call: the assistant
// message goes back as the thinking blocks unchanged, then its text, then its
// tool_use. The same message without blocks, or to a model without thinking
// turned on, is sent as before.
func TestAnthropicReplaysThinkingBlocks(t *testing.T) {
	const toolUse = `{"type":"tool_use","id":"toolu_1","name":"inv__list-items","input":{"limit":"5"}}`
	const textAndToolUse = `{"type":"text","text":"Checking."},` + toolUse
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		content := `{"type":"text","text":"Done."}`
		if len(bodies) == 1 {
			content = thinkingBlock + "," + redactedThinkingBlock + "," + textAndToolUse
		}
		_, _ = io.WriteString(w, `{"id":"r","model":"claude-x","content":[`+content+`],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer server.Close()
	p := NewAnthropicProvider("anthropic", server.URL, "key", []ModelInfo{{ID: "claude-x", Thinking: "adaptive"}})
	thinkingOff := NewAnthropicProvider("anthropic", server.URL, "key", nil)
	user := Message{Role: RoleUser, Content: "How many items?"}

	resp, err := p.Complete(context.Background(), &CompletionRequest{Model: "claude-x", Messages: []Message{user}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Thinking) != 2 || string(resp.Thinking[0]) != thinkingBlock || string(resp.Thinking[1]) != redactedThinkingBlock {
		t.Fatalf("response thinking = %q, want both blocks as received", resp.Thinking)
	}
	if resp.ThinkingFingerprint == "" {
		t.Fatal("response thinking has no fingerprint")
	}

	cases := []struct {
		name        string
		p           *AnthropicProvider
		thinking    []json.RawMessage
		wantContent string
	}{
		{"with blocks", p, resp.Thinking, `[` + thinkingBlock + `,` + redactedThinkingBlock + `,` + textAndToolUse + `]`},
		{"without blocks", p, nil, `[` + textAndToolUse + `]`},
		{"thinking off", thinkingOff, resp.Thinking, `[` + textAndToolUse + `]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assistant := Message{Role: RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls,
				Thinking: tc.thinking, ThinkingFingerprint: resp.ThinkingFingerprint}
			result := Message{Role: RoleTool, Content: "5 items", ToolCallID: "toolu_1"}
			if _, err := tc.p.Complete(context.Background(), &CompletionRequest{Model: "claude-x", Messages: []Message{user, assistant, result}}); err != nil {
				t.Fatal(err)
			}
			var sent struct {
				Messages []struct {
					Content json.RawMessage `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal([]byte(bodies[len(bodies)-1]), &sent); err != nil {
				t.Fatal(err)
			}
			if got := string(sent.Messages[1].Content); got != tc.wantContent {
				t.Errorf("replayed assistant content:\n got %s\nwant %s", got, tc.wantContent)
			}
		})
	}
}

// thinkingReplayRig answers the n-th request with replies[n-1], or a text
// answer past the end, and records the request bodies.
func thinkingReplayRig(t *testing.T, models []ModelInfo, replies ...string) (*AnthropicProvider, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		bodies = append(bodies, string(body))
		content := `{"type":"text","text":"Done."}`
		if n := len(bodies); n <= len(replies) {
			content = replies[n-1]
		}
		_, _ = io.WriteString(w, `{"id":"r","model":"claude-x","content":[`+content+`],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(server.Close)
	return NewAnthropicProvider("anthropic", server.URL, "key", models), func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

// sentAnthMessages returns the messages of a request body as the bytes sent.
func sentAnthMessages(t *testing.T, body string) []string {
	t.Helper()
	var sent struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(sent.Messages))
	for i, m := range sent.Messages {
		out[i] = string(m)
	}
	return out
}

// TestAnthropicThinkingReplayFollowsBinding replays a response's block on the
// next request after one change each. With adaptive thinking the block goes
// back only while the system, the tools and the earlier messages are
// unchanged; an effort change keeps it. With a thinking budget it goes back
// whatever changed. A block without a fingerprint never goes back.
func TestAnthropicThinkingReplayFollowsBinding(t *testing.T) {
	const call = `{"type":"tool_use","id":"toolu_1","name":"inv__list-items","input":{}}`
	const withBlock = `{"role":"assistant","content":[` + thinkingBlock + `,` + call + `]}`
	const withoutBlock = `{"role":"assistant","content":[` + call + `]}`
	listItems := ToolDefinition{Name: "inv__list-items", Description: "List items", Parameters: map[string]interface{}{"type": "object"}}
	showItem := ToolDefinition{Name: "inv__show-item", Description: "Show an item", Parameters: map[string]interface{}{"type": "object"}}
	cases := []struct {
		name     string
		thinking string // the model's thinking form; "" thinks by budget
		change   func(req *CompletionRequest, assistant *Message)
		want     string
	}{
		{"adaptive, unchanged", "adaptive", func(*CompletionRequest, *Message) {}, withBlock},
		{"adaptive, effort changed", "adaptive", func(r *CompletionRequest, _ *Message) { r.ReasoningEffort = "low" }, withBlock},
		{"adaptive, system changed", "adaptive", func(r *CompletionRequest, _ *Message) { r.Messages[0].Content += " Be brief." }, withoutBlock},
		{"adaptive, tool added", "adaptive", func(r *CompletionRequest, _ *Message) { r.Tools = append(r.Tools, showItem) }, withoutBlock},
		{"adaptive, earlier message changed", "adaptive", func(r *CompletionRequest, _ *Message) { r.Messages[1].Content = "How many?" }, withoutBlock},
		{"adaptive, no fingerprint", "adaptive", func(_ *CompletionRequest, a *Message) { a.ThinkingFingerprint = "" }, withoutBlock},
		{"budget, system changed", "", func(r *CompletionRequest, _ *Message) { r.Messages[0].Content += " Be brief." }, withBlock},
		{"budget, no fingerprint", "", func(_ *CompletionRequest, a *Message) { a.ThinkingFingerprint = "" }, withoutBlock},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			models := []ModelInfo{{ID: "claude-x", Thinking: tc.thinking, ReasoningEffort: "high", Features: []Feature{FeatureReasoning}}}
			p, bodies := thinkingReplayRig(t, models, thinkingBlock+","+call)
			request := func(msgs ...Message) *CompletionRequest {
				return &CompletionRequest{Model: "claude-x", Messages: msgs, Tools: []ToolDefinition{listItems}, Reasoning: tc.thinking == ""}
			}
			system := Message{Role: RoleSystem, Content: "Rules."}
			user := Message{Role: RoleUser, Content: "How many items?"}
			resp, err := p.Complete(context.Background(), request(system, user))
			if err != nil {
				t.Fatal(err)
			}
			assistant := Message{Role: RoleAssistant, ToolCalls: resp.ToolCalls, Thinking: resp.Thinking, ThinkingFingerprint: resp.ThinkingFingerprint}
			next := request(system, user)
			tc.change(next, &assistant)
			next.Messages = append(next.Messages, assistant, Message{Role: RoleTool, Content: "5", ToolCallID: "toolu_1"})
			if _, err := p.Complete(context.Background(), next); err != nil {
				t.Fatal(err)
			}
			if got := sentAnthMessages(t, bodies()[1])[1]; got != tc.want {
				t.Errorf("replayed assistant message:\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

// TestAnthropicThinkingReplayAfterToolLoad loads a tool between two rounds of
// a turn: the block produced before the tool list changed is left out, the
// one produced after it goes back unchanged.
func TestAnthropicThinkingReplayAfterToolLoad(t *testing.T) {
	const block1 = `{"type":"thinking","thinking":"","signature":"sig-1"}`
	const block2 = `{"type":"thinking","thinking":"","signature":"sig-2"}`
	const call1 = `{"type":"tool_use","id":"toolu_1","name":"inv__load","input":{}}`
	const call2 = `{"type":"tool_use","id":"toolu_2","name":"inv__show-item","input":{}}`
	p, bodies := thinkingReplayRig(t, []ModelInfo{{ID: "claude-x", Thinking: "adaptive"}}, block1+","+call1, block2+","+call2)
	load := ToolDefinition{Name: "inv__load", Description: "Load tools", Parameters: map[string]interface{}{"type": "object"}}
	show := ToolDefinition{Name: "inv__show-item", Description: "Show an item", Parameters: map[string]interface{}{"type": "object"}}
	msgs := []Message{{Role: RoleUser, Content: "Show item 7."}}
	complete := func(tools ...ToolDefinition) {
		t.Helper()
		resp, err := p.Complete(context.Background(), &CompletionRequest{Model: "claude-x", Messages: msgs, Tools: tools})
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.ToolCalls) == 1 {
			msgs = append(msgs,
				Message{Role: RoleAssistant, ToolCalls: resp.ToolCalls, Thinking: resp.Thinking, ThinkingFingerprint: resp.ThinkingFingerprint},
				Message{Role: RoleTool, Content: "ok", ToolCallID: resp.ToolCalls[0].ID})
		}
	}
	complete(load)
	complete(load, show)
	complete(load, show)

	got := sentAnthMessages(t, bodies()[2])
	want := []string{
		`{"role":"user","content":"Show item 7."}`,
		`{"role":"assistant","content":[` + call1 + `]}`,
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}`,
		`{"role":"assistant","content":[` + block2 + `,` + call2 + `]}`,
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_2","content":"ok"}]}`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("third request messages:\n got %s\nwant %s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestThinkingBindingIgnoresCacheMarkers checks that adding, moving or
// removing a cache marker on the system blocks, or reordering the tools,
// leaves the fingerprint as it is, while a change of the system text does not.
func TestThinkingBindingIgnoresCacheMarkers(t *testing.T) {
	tools := []anthTool{{Name: "inv__a"}, {Name: "inv__b"}}
	sumWith := func(system string, tools []anthTool) string {
		t.Helper()
		b, err := newThinkingBinding(json.RawMessage(system), tools)
		if err != nil {
			t.Fatal(err)
		}
		return b.sum()
	}
	sum := func(system string) string { return sumWith(system, tools) }
	marker := `,"cache_control":{"type":"ephemeral"}`
	first := sum(`[{"type":"text","text":"Fixed."` + marker + `},{"type":"text","text":"Round."}]`)
	for name, system := range map[string]string{
		"moved":   `[{"type":"text","text":"Fixed."},{"type":"text","text":"Round."` + marker + `}]`,
		"removed": `[{"type":"text","text":"Fixed."},{"type":"text","text":"Round."}]`,
	} {
		if sum(system) != first {
			t.Errorf("marker %s: fingerprint changed", name)
		}
	}
	if sumWith(`[{"type":"text","text":"Fixed."`+marker+`},{"type":"text","text":"Round."}]`, []anthTool{tools[1], tools[0]}) != first {
		t.Error("reordered tools: fingerprint changed")
	}
	if sum(`[{"type":"text","text":"Fixed."`+marker+`},{"type":"text","text":"Round 2."}]`) == first {
		t.Error("a changed system text kept the fingerprint")
	}
}

// TestAnthropicConcatenatesSystemMessages verifies the defense-in-depth in
// toAnthRequest: multiple RoleSystem messages are joined (not last-wins) so a
// stray mid-array system message cannot silently clobber the real prompt.
func TestAnthropicConcatenatesSystemMessages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req anthRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if got := systemText(t, req.System); got != "Real prompt.\n\nStray nudge." {
			t.Errorf("system = %q, want both system messages concatenated", got)
		}
		resp := anthResponse{ID: "m", Content: []anthContentBlock{{Type: "text", Text: "ok"}}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	p := NewAnthropicProvider("anthropic", server.URL, "key", nil)
	_, err := p.Complete(context.Background(), &CompletionRequest{
		Model: "claude-sonnet-4-20250514",
		Messages: []Message{
			{Role: RoleSystem, Content: "Real prompt."},
			{Role: RoleUser, Content: "hi"},
			{Role: RoleSystem, Content: "Stray nudge."},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

// --- prompt caching ---

// cacheTestSegments hold "<", which encoding/json escapes, so the body pinned
// in anthropicBodyWithoutCache pins that escaping too.
var cacheTestSegments = SystemSegments{Fixed: "Fixed <rules>.\n", Conversation: "Conversation.\n", Round: "Round."}

// cacheTestRequest is a chat-loop request: its leading system message is the
// join of segs, and segments is what it carries on Segments.
func cacheTestRequest(segs SystemSegments, segments *SystemSegments) *CompletionRequest {
	return &CompletionRequest{
		Model: "claude-x",
		Messages: []Message{
			{Role: RoleSystem, Content: segs.Join()},
			{Role: RoleUser, Content: "How many items?"},
		},
		Tools: []ToolDefinition{{
			Name: "inv__list-items", Description: "List items",
			Parameters: map[string]interface{}{"type": "object"},
		}},
		Reasoning: true,
		Segments:  segments,
	}
}

// anthropicBodies sends req through a provider built with models and opts to
// a server that answers every request with status, and returns the request
// bodies it received with the error Complete returned.
func anthropicBodies(t *testing.T, models []ModelInfo, req *CompletionRequest, status int, opts ...AnthropicOption) ([]string, error) {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = io.WriteString(w, `{"id":"r1","model":"claude-x","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
			return
		}
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"rejected"}}`)
	}))
	defer server.Close()
	_, err := NewAnthropicProvider("anthropic", server.URL, "key", models, opts...).Complete(context.Background(), req)
	mu.Lock()
	defer mu.Unlock()
	return bodies, err
}

// anthropicBodyWithoutCache is the body sent for cacheTestRequest with
// cacheTestSegments before prompt caching existed.
const anthropicBodyWithoutCache = `{"model":"claude-x","system":"Fixed \u003crules\u003e.\nConversation.\nRound.",` +
	`"messages":[{"role":"user","content":"How many items?"}],"max_tokens":14096,` +
	`"thinking":{"type":"enabled","budget_tokens":10000},` +
	`"tools":[{"name":"inv__list-items","description":"List items","input_schema":{"type":"object"}}]}`

// TestAnthropicSystemStaysAString covers the requests that send the system
// prompt as one string: caching off, no segments, or segments that are not
// the leading system message's text. Their body is byte for byte the body
// sent before prompt caching existed.
func TestAnthropicSystemStaysAString(t *testing.T) {
	segs := cacheTestSegments
	other := SystemSegments{Fixed: "Another prompt."}
	on := []AnthropicOption{WithAnthropicPromptCache(true)}
	cases := []struct {
		name string
		req  *CompletionRequest
		opts []AnthropicOption
	}{
		{"caching off", cacheTestRequest(segs, &segs), nil},
		{"caching on, no segments", cacheTestRequest(segs, nil), on},
		{"caching on, segments of another text", cacheTestRequest(segs, &other), on},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bodies, err := anthropicBodies(t, nil, tc.req, http.StatusOK, tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			if len(bodies) != 1 || bodies[0] != anthropicBodyWithoutCache {
				t.Errorf("request bodies:\n got %q\nwant %q", bodies, anthropicBodyWithoutCache)
			}
		})
	}
}

// TestAnthropicCacheMarkedSystem checks the request body with prompt caching
// on and segments present: the system field is one text block per non-blank
// segment, then per further system message, with a cache marker on the
// first, and the request carries the automatic cache marker. The rest of the
// body is what caching off sends.
func TestAnthropicCacheMarkedSystem(t *testing.T) {
	cases := []struct {
		name       string
		segs       SystemSegments
		extra      string // a further system message after the user message
		wantSystem string
	}{
		{
			name: "three segments",
			segs: SystemSegments{Fixed: "Fixed.\n", Conversation: "Conversation.\n", Round: "Round."},
			wantSystem: `[{"type":"text","text":"Fixed.\n","cache_control":{"type":"ephemeral"}},` +
				`{"type":"text","text":"Conversation.\n"},{"type":"text","text":"Round."}]`,
		},
		{
			name:       "blank segments left out",
			segs:       SystemSegments{Fixed: "Fixed.\n", Conversation: "", Round: " \n"},
			wantSystem: `[{"type":"text","text":"Fixed.\n","cache_control":{"type":"ephemeral"}}]`,
		},
		{
			name: "marker on the first block sent",
			segs: SystemSegments{Fixed: "", Conversation: "Conversation.\n", Round: "Round."},
			wantSystem: `[{"type":"text","text":"Conversation.\n","cache_control":{"type":"ephemeral"}},` +
				`{"type":"text","text":"Round."}]`,
		},
		{
			name:  "further system message",
			segs:  SystemSegments{Fixed: "Fixed.\n", Round: "Round."},
			extra: "Note.",
			wantSystem: `[{"type":"text","text":"Fixed.\n","cache_control":{"type":"ephemeral"}},` +
				`{"type":"text","text":"Round."},{"type":"text","text":"Note."}]`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			segs := tc.segs
			req := cacheTestRequest(segs, &segs)
			if tc.extra != "" {
				req.Messages = append(req.Messages, Message{Role: RoleSystem, Content: tc.extra})
			}
			cachedBodies, err := anthropicBodies(t, nil, req, http.StatusOK, WithAnthropicPromptCache(true))
			if err != nil {
				t.Fatal(err)
			}
			plainBodies, err := anthropicBodies(t, nil, req, http.StatusOK)
			if err != nil {
				t.Fatal(err)
			}
			var cached, plain map[string]json.RawMessage
			if err := json.Unmarshal([]byte(cachedBodies[0]), &cached); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(plainBodies[0]), &plain); err != nil {
				t.Fatal(err)
			}
			if got := string(cached["system"]); got != tc.wantSystem {
				t.Errorf("system:\n got %s\nwant %s", got, tc.wantSystem)
			}
			if got := string(cached["cache_control"]); got != `{"type":"ephemeral"}` {
				t.Errorf("cache_control = %s, want the automatic marker", got)
			}
			delete(cached, "system")
			delete(cached, "cache_control")
			delete(plain, "system")
			if len(cached) != len(plain) {
				t.Errorf("fields besides system and cache_control: got %d, want %d", len(cached), len(plain))
			}
			for k, v := range plain {
				if string(cached[k]) != string(v) {
					t.Errorf("%s:\n got %s\nwant %s", k, cached[k], v)
				}
			}
		})
	}
}

// TestAnthropicCacheMarkedRequestRejected covers a cache-marked request the
// API answers with an error: the error comes back through the usual path, and
// every attempt sent, a transient retry included, is the same marked body.
func TestAnthropicCacheMarkedRequestRejected(t *testing.T) {
	cases := []struct {
		status   int
		attempts int
	}{
		{http.StatusBadRequest, 1},
		{http.StatusServiceUnavailable, 2},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			segs := cacheTestSegments
			bodies, err := anthropicBodies(t, nil, cacheTestRequest(segs, &segs), tc.status,
				WithAnthropicPromptCache(true),
				WithAnthropicRetryPolicy(RetryPolicy{MaxAttempts: tc.attempts, BaseDelay: time.Millisecond}))
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("anthropic api error (status %d)", tc.status)) {
				t.Errorf("error = %v, want the API's status %d", err, tc.status)
			}
			if len(bodies) != tc.attempts {
				t.Fatalf("requests sent = %d, want %d", len(bodies), tc.attempts)
			}
			for i, body := range bodies {
				marked := strings.HasPrefix(body, `{"model":"claude-x","system":[{`) &&
					strings.HasSuffix(body, `,"cache_control":{"type":"ephemeral"}}`)
				if body != bodies[0] || !marked {
					t.Errorf("attempt %d is not the first attempt's cache-marked body: %s", i+1, body)
				}
			}
		})
	}
}

// --- thinking ---

// TestAnthropicThinking covers the thinking form of a request. A model with
// thinking: adaptive is sent thinking {type: adaptive}, the request's effort,
// else the model's, as output_config.effort when one is set, and max_tokens
// as asked; the llm_request event records the effort sent. A model without
// the field is sent today's thinking budget, byte for byte, and no effort.
func TestAnthropicThinking(t *testing.T) {
	const adaptive = `{"model":"claude-x","system":"Fixed \u003crules\u003e.\nConversation.\nRound.",` +
		`"messages":[{"role":"user","content":"How many items?"}],"max_tokens":4096,` +
		`"thinking":{"type":"adaptive","block_binding":{"prefix_mismatch_behavior":"drop_block"}},`
	const tools = `"tools":[{"name":"inv__list-items","description":"List items","input_schema":{"type":"object"}}]}`
	cases := []struct {
		name        string
		thinking    string
		modelEffort string
		effort      string
		wantBody    string
		wantEffort  string // recorded in the llm_request event
	}{
		{"adaptive with effort", "adaptive", "", "high", adaptive + `"output_config":{"effort":"high"},` + tools, "high"},
		{"adaptive with the model's effort", "adaptive", "medium", "", adaptive + `"output_config":{"effort":"medium"},` + tools, "medium"},
		{"adaptive, the request's effort over the model's", "adaptive", "medium", "high",
			adaptive + `"output_config":{"effort":"high"},` + tools, "high"},
		{"adaptive without effort", "adaptive", "", "", adaptive + tools, ""},
		{"without thinking", "", "medium", "high", anthropicBodyWithoutCache, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := cacheTestRequest(cacheTestSegments, nil)
			req.ReasoningEffort = tc.effort
			models := []ModelInfo{{ID: "claude-x", Thinking: tc.thinking, ReasoningEffort: tc.modelEffort,
				Features: []Feature{FeatureReasoning}}}
			sink := &recordingEventSink{}
			bodies, err := anthropicBodies(t, models, req, http.StatusOK, WithAnthropicSessionEventSink(sink))
			if err != nil {
				t.Fatal(err)
			}
			if len(bodies) != 1 || bodies[0] != tc.wantBody {
				t.Errorf("request bodies:\n got %q\nwant %q", bodies, tc.wantBody)
			}
			got := sink.snapshot()
			if len(got) == 0 || got[0].EventType != events.TypeLLMRequest {
				t.Fatalf("events = %v, want llm_request first", sink.types())
			}
			var payload events.LLMRequestPayload
			if err := json.Unmarshal(got[0].Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if !payload.Reasoning || payload.ReasoningEffort != tc.wantEffort {
				t.Errorf("llm_request reasoning = %t, effort = %q, want true and %q", payload.Reasoning, payload.ReasoningEffort, tc.wantEffort)
			}
		})
	}
}

// TestAnthropicThinkingBindingBeta checks the beta header that goes with
// block_binding: a model with adaptive thinking sends it, joined with the
// cache-diagnosis beta on a diagnosed chat-loop request; a model with thinking
// by budget sends neither the header value nor block_binding.
func TestAnthropicThinkingBindingBeta(t *testing.T) {
	const binding = `{"type":"adaptive","block_binding":{"prefix_mismatch_behavior":"drop_block"}}`
	const budget = `{"type":"enabled","budget_tokens":10000}`
	cases := []struct {
		name         string
		thinking     string
		diagnostics  bool
		wantThinking string
		wantBeta     string
	}{
		{"adaptive", "adaptive", false, binding, anthropicThinkingBindingBeta},
		{"adaptive with cache diagnostics", "adaptive", true, binding, anthropicCacheDiagnosisBeta + "," + anthropicThinkingBindingBeta},
		{"budget", "", false, budget, ""},
		{"budget with cache diagnostics", "", true, budget, anthropicCacheDiagnosisBeta},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, sent := diagnosticsServer(t, "")
			models := []ModelInfo{{ID: "claude-x", Thinking: tc.thinking, Features: []Feature{FeatureReasoning}}}
			p := NewAnthropicProvider("anthropic", server.URL, "key", models,
				WithAnthropicPromptCache(true), WithAnthropicCacheDiagnostics(tc.diagnostics))
			completeInSession(t, p, "s1", true)
			reqs := sent()
			if len(reqs) != 1 {
				t.Fatalf("requests sent = %d, want 1", len(reqs))
			}
			var body struct {
				Thinking json.RawMessage `json:"thinking"`
			}
			if err := json.Unmarshal(reqs[0].body, &body); err != nil {
				t.Fatal(err)
			}
			if string(body.Thinking) != tc.wantThinking {
				t.Errorf("thinking = %s, want %s", body.Thinking, tc.wantThinking)
			}
			if got := reqs[0].header.Get("anthropic-beta"); got != tc.wantBeta {
				t.Errorf("anthropic-beta = %q, want %q", got, tc.wantBeta)
			}
		})
	}
}

// TestAnthropicThinkingDropped counts the thinking_dropped entries of a
// response's input_transformations onto the llm_response event; other entry
// types do not count, and none leaves the field out.
func TestAnthropicThinkingDropped(t *testing.T) {
	cases := []struct {
		name            string
		transformations string
		want            string // thinking_dropped in the payload; "" = absent
	}{
		{"two dropped", `[{"type":"thinking_dropped","path":"messages.1.content.0","reason":"prefix_binding_mismatch"},` +
			`{"type":"thinking_mismatch_allowed","path":"messages.3.content.0"},` +
			`{"type":"thinking_dropped","path":"messages.3.content.1","reason":"model_binding_mismatch"}]`, `"thinking_dropped":2`},
		{"none dropped", `[]`, ""},
		{"no field", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reply := `{"id":"r","model":"claude-x","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}`
			if tc.transformations != "" {
				reply += `,"input_transformations":` + tc.transformations
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, reply+"}")
			}))
			defer server.Close()
			sink := &recordingEventSink{}
			p := NewAnthropicProvider("anthropic", server.URL, "key", nil, WithAnthropicSessionEventSink(sink))
			if _, err := p.Complete(context.Background(), cacheTestRequest(cacheTestSegments, nil)); err != nil {
				t.Fatal(err)
			}
			got := sink.snapshot()
			if len(got) != 2 || got[1].EventType != events.TypeLLMResponse {
				t.Fatalf("events = %v, want llm_request then llm_response", sink.types())
			}
			payload := string(got[1].Payload)
			if tc.want != "" && !strings.Contains(payload, tc.want) {
				t.Errorf("llm_response payload lacks %s: %s", tc.want, payload)
			}
			if tc.want == "" && strings.Contains(payload, `"thinking_dropped"`) {
				t.Errorf("llm_response payload carries thinking_dropped: %s", payload)
			}
		})
	}
}

// TestAnthropicToolCallWithoutArguments checks that a replayed tool call
// without arguments is sent with an empty input object, never null.
func TestAnthropicToolCallWithoutArguments(t *testing.T) {
	req := &CompletionRequest{Model: "claude-x", Messages: []Message{
		{Role: RoleUser, Content: "Delete it."},
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "toolu_1", Name: "inv__delete"}}},
		{Role: RoleTool, Content: "deleted", ToolCallID: "toolu_1"},
	}}
	bodies, err := anthropicBodies(t, nil, req, http.StatusOK)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"inv__delete","input":{}}]}`
	if len(bodies) != 1 || !strings.Contains(bodies[0], want) {
		t.Errorf("request bodies %q, want the tool call as %s", bodies, want)
	}
}

// --- usage and cost ---

// TestAnthropicUsageAndCost covers R5: tokens in is uncached plus written plus
// read tokens, the input cost prices each part at its own rate, and the
// response and the llm_response event carry the same figures. A response
// without cache fields records zero for both.
func TestAnthropicUsageAndCost(t *testing.T) {
	models := []ModelInfo{{ID: "claude-x", Cost: ModelCost{Input: 1, Output: 5, CacheWrite: 1.25, CacheRead: 0.1}}}
	cases := []struct {
		name  string
		usage string
		want  Usage
	}{
		{
			name:  "cache usage",
			usage: `{"input_tokens":1000000,"cache_creation_input_tokens":2000000,"cache_read_input_tokens":10000000,"output_tokens":3000000}`,
			want: Usage{
				InputTokens: 13_000_000, OutputTokens: 3_000_000,
				CacheWriteTokens: 2_000_000, CacheReadTokens: 10_000_000,
				InputCost: 1 + 2.5 + 1, OutputCost: 15,
			},
		},
		{
			name:  "no cache fields",
			usage: `{"input_tokens":1000000,"output_tokens":3000000}`,
			want:  Usage{InputTokens: 1_000_000, OutputTokens: 3_000_000, InputCost: 1, OutputCost: 15},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"r1","model":"claude-x","content":[{"type":"text","text":"ok"}],"usage":`+tc.usage+`}`)
			}))
			defer server.Close()
			sink := &recordingEventSink{}
			p := NewAnthropicProvider("anthropic", server.URL, "key", models, WithAnthropicSessionEventSink(sink))
			resp, err := p.Complete(context.Background(), &CompletionRequest{
				Model:    "claude-x",
				Messages: []Message{{Role: RoleUser, Content: "Hi"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if resp.Usage != tc.want {
				t.Errorf("response usage:\n got %+v\nwant %+v", resp.Usage, tc.want)
			}

			got := sink.snapshot()
			if len(got) != 2 || got[1].EventType != events.TypeLLMResponse {
				t.Fatalf("events = %v, want llm_request then llm_response", sink.types())
			}
			var payload events.LLMResponsePayload
			if err := json.Unmarshal(got[1].Payload, &payload); err != nil {
				t.Fatal(err)
			}
			recorded := Usage{
				InputTokens: payload.TokensIn, OutputTokens: payload.TokensOut,
				CacheWriteTokens: payload.CacheWriteTokens, CacheReadTokens: payload.CacheReadTokens,
				InputCost: payload.CostInput, OutputCost: payload.CostOutput,
			}
			if recorded != tc.want {
				t.Errorf("llm_response records:\n got %+v\nwant %+v\npayload %s", recorded, tc.want, got[1].Payload)
			}
		})
	}
}

// TestLLMResponseChatLoop checks chat_loop on the llm_response event of every
// provider path: true when the request carried segments, absent otherwise.
// Only the chat loop sets segments; TestNonChatLoopRequestsCarryNoSegments in
// the orchestrator covers session titles, summaries, confirmation narration,
// subprocesses and the planner.
func TestLLMResponseChatLoop(t *testing.T) {
	segs := SystemSegments{Fixed: "Fixed.\n", Conversation: "Conversation.\n", Round: "Round."}
	paths := []struct {
		name string
		call func(t *testing.T, sink *recordingEventSink, req *CompletionRequest)
	}{
		{"anthropic", func(t *testing.T, sink *recordingEventSink, req *CompletionRequest) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"id":"r1","model":"m","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer server.Close()
			p := NewAnthropicProvider("anthropic", server.URL, "key", nil,
				WithAnthropicSessionEventSink(sink), WithAnthropicPromptCache(true))
			if _, err := p.Complete(context.Background(), req); err != nil {
				t.Fatal(err)
			}
		}},
		{"openai", func(t *testing.T, sink *recordingEventSink, req *CompletionRequest) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"id":"c1","model":"m","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
			}))
			defer server.Close()
			if _, err := NewOpenAIProvider("openai", server.URL, "key", nil, WithOpenAISessionEventSink(sink)).
				Complete(context.Background(), req); err != nil {
				t.Fatal(err)
			}
		}},
		{"openai stream", func(t *testing.T, sink *recordingEventSink, req *CompletionRequest) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, streamResponse([]string{"ok"}, "stop", true))
			}))
			defer server.Close()
			stream, err := NewOpenAIProvider("openai", server.URL, "key", nil, WithOpenAISessionEventSink(sink)).
				Stream(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			drainStream(t, stream)
			_ = stream.Close()
		}},
	}
	for _, path := range paths {
		for _, segments := range []*SystemSegments{&segs, nil} {
			t.Run(fmt.Sprintf("%s/segments=%t", path.name, segments != nil), func(t *testing.T) {
				sink := &recordingEventSink{}
				path.call(t, sink, &CompletionRequest{
					Model:    "m",
					Messages: []Message{{Role: RoleSystem, Content: segs.Join()}, {Role: RoleUser, Content: "Hi"}},
					Segments: segments,
				})
				got := sink.snapshot()
				if len(got) != 2 || got[1].EventType != events.TypeLLMResponse {
					t.Fatalf("events = %v, want llm_request then llm_response", sink.types())
				}
				payload := string(got[1].Payload)
				if segments != nil && !strings.Contains(payload, `"chat_loop":true`) {
					t.Errorf("chat-loop call: chat_loop missing from %s", payload)
				}
				if segments == nil && strings.Contains(payload, `"chat_loop"`) {
					t.Errorf("call without segments: chat_loop present in %s", payload)
				}
			})
		}
	}
}
