package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
		if req.System != "You are helpful." {
			t.Errorf("system = %q", req.System)
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
		name       string
		mimeType   string
		data       []byte
		wantType   string
		wantText   string // non-empty only for text blocks
		wantSource bool   // true when a source block is expected
	}{
		{
			name:       "image becomes image block",
			mimeType:   "image/png",
			data:       []byte{0x89, 0x50, 0x4e, 0x47},
			wantType:   "image",
			wantSource: true,
		},
		{
			name:       "pdf becomes document block",
			mimeType:   "application/pdf",
			data:       []byte("%PDF-1.4"),
			wantType:   "document",
			wantSource: true,
		},
		{
			name:     "csv becomes text block",
			mimeType: "text/csv",
			data:     []byte("a,b,c\n1,2,3"),
			wantType: "text",
			wantText: "a,b,c\n1,2,3",
		},
		{
			name:     "plain text becomes text block",
			mimeType: "text/plain",
			data:     []byte("hello"),
			wantType: "text",
			wantText: "hello",
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
			// blocks[0] is the file, blocks[1] is the text message
			if len(blocks) != 2 {
				t.Fatalf("blocks = %d, want 2", len(blocks))
			}
			if blocks[0].Type != tc.wantType {
				t.Errorf("block type = %q, want %q", blocks[0].Type, tc.wantType)
			}
			if tc.wantSource && blocks[0].Source == nil {
				t.Error("expected source block, got nil")
			}
			if !tc.wantSource && blocks[0].Source != nil {
				t.Error("expected no source block, got one")
			}
			if tc.wantText != "" && blocks[0].Text != tc.wantText {
				t.Errorf("text = %q, want %q", blocks[0].Text, tc.wantText)
			}
			if tc.wantSource && blocks[0].Source != nil && blocks[0].Source.MediaType != tc.mimeType {
				t.Errorf("media_type = %q, want %q", blocks[0].Source.MediaType, tc.mimeType)
			}
		})
	}
}
