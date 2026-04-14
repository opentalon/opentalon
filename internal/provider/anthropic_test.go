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
