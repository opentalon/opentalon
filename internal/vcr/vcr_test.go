package vcr

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/prompts"
	"github.com/opentalon/opentalon/internal/provider"
)

func writeCassette(t *testing.T, c Cassette) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPlayerReplaySequence(t *testing.T) {
	c := Cassette{
		PromptHash: prompts.Hash(),
		RecordedAt: time.Now(),
		Model:      "test-model",
		Interactions: []Interaction{
			{Response: &provider.CompletionResponse{Content: "first"}},
			{Response: &provider.CompletionResponse{Content: "second"}},
		},
	}
	path := writeCassette(t, c)
	p, err := NewPlayer(path)
	if err != nil {
		t.Fatal(err)
	}

	r1, err := p.Complete(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Content != "first" {
		t.Errorf("interaction 0: got %q, want %q", r1.Content, "first")
	}

	r2, err := p.Complete(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Content != "second" {
		t.Errorf("interaction 1: got %q, want %q", r2.Content, "second")
	}
}

func TestPlayerExhaustedError(t *testing.T) {
	c := Cassette{
		PromptHash:   prompts.Hash(),
		RecordedAt:   time.Now(),
		Interactions: []Interaction{{Response: &provider.CompletionResponse{Content: "only"}}},
	}
	path := writeCassette(t, c)
	p, _ := NewPlayer(path)
	_, _ = p.Complete(context.Background(), nil) // consume the one interaction
	_, err := p.Complete(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error on exhausted cassette")
	}
}

func TestPlayerStaleCassetteError(t *testing.T) {
	c := Cassette{
		PromptHash:   "0000000000000000000000000000000000000000000000000000000000000000",
		RecordedAt:   time.Now(),
		Interactions: []Interaction{{Response: &provider.CompletionResponse{Content: "x"}}},
	}
	path := writeCassette(t, c)
	_, err := NewPlayer(path)
	if err == nil {
		t.Fatal("expected stale cassette error")
	}
}

func TestPlayerMissingFile(t *testing.T) {
	_, err := NewPlayer("/nonexistent/path/cassette.json")
	if err == nil {
		t.Fatal("expected error for missing cassette")
	}
}

func TestRecorderSaveAndReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recorded.json")

	stub := &stubLLM{responses: []string{"hello", "world"}}
	rec := NewRecorder(stub, path, "test-model")

	req := &provider.CompletionRequest{Messages: []provider.Message{{Role: "user", Content: "hi"}}}
	if _, err := rec.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, err := rec.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}

	// Replay the saved cassette.
	p, err := NewPlayer(path)
	if err != nil {
		t.Fatal(err)
	}
	r1, _ := p.Complete(context.Background(), nil)
	r2, _ := p.Complete(context.Background(), nil)
	if r1.Content != "hello" || r2.Content != "world" {
		t.Errorf("replay mismatch: got %q, %q", r1.Content, r2.Content)
	}
}

type stubLLM struct {
	responses []string
	pos       int
}

func (s *stubLLM) Complete(_ context.Context, _ *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	resp := s.responses[s.pos]
	s.pos++
	return &provider.CompletionResponse{Content: resp}, nil
}
