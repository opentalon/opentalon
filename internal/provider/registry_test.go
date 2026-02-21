package provider

import (
	"context"
	"testing"
)

type stubProvider struct {
	id     string
	models []ModelInfo
}

func (s *stubProvider) ID() string { return s.id }
func (s *stubProvider) Complete(_ context.Context, _ *CompletionRequest) (*CompletionResponse, error) {
	return &CompletionResponse{Content: "stub"}, nil
}
func (s *stubProvider) Stream(_ context.Context, _ *CompletionRequest) (ResponseStream, error) {
	return nil, nil
}
func (s *stubProvider) Models() []ModelInfo { return s.models }
func (s *stubProvider) SupportsFeature(_ Feature) bool { return false }

func TestRegistryRegisterAndGet(t *testing.T) {
	reg := NewRegistry()
	p := &stubProvider{id: "anthropic"}

	if err := reg.Register(p); err != nil {
		t.Fatal(err)
	}

	got, err := reg.Get("anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID() != "anthropic" {
		t.Errorf("got %q, want %q", got.ID(), "anthropic")
	}
}

func TestRegistryDuplicateRegister(t *testing.T) {
	reg := NewRegistry()
	p := &stubProvider{id: "anthropic"}
	_ = reg.Register(p)

	err := reg.Register(p)
	if err == nil {
		t.Error("expected error on duplicate register")
	}
}

func TestRegistryGetNotFound(t *testing.T) {
	reg := NewRegistry()
	_, err := reg.Get("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent provider")
	}
}

func TestRegistryGetForModel(t *testing.T) {
	reg := NewRegistry()
	p := &stubProvider{id: "openai"}
	_ = reg.Register(p)

	ref := NewModelRef("openai", "gpt-5.2")
	got, err := reg.GetForModel(ref)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID() != "openai" {
		t.Errorf("got %q, want %q", got.ID(), "openai")
	}
}

func TestRegistryList(t *testing.T) {
	reg := NewRegistry()
	_ = reg.Register(&stubProvider{id: "a"})
	_ = reg.Register(&stubProvider{id: "b"})

	list := reg.List()
	if len(list) != 2 {
		t.Errorf("got %d providers, want 2", len(list))
	}
}

func TestRegistryDeregister(t *testing.T) {
	reg := NewRegistry()
	_ = reg.Register(&stubProvider{id: "temp"})
	reg.Deregister("temp")

	_, err := reg.Get("temp")
	if err == nil {
		t.Error("expected error after deregister")
	}
}
