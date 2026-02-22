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
