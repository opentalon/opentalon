package provider

import (
	"testing"
)

func TestFromConfigOpenAI(t *testing.T) {
	p, err := FromConfig(ProviderConfig{
		ID:     "openai",
		APIKey: "sk-test",
		API:    APIOpenAI,
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.ID() != "openai" {
		t.Errorf("id = %q", p.ID())
	}
	if _, ok := p.(*OpenAIProvider); !ok {
		t.Errorf("expected *OpenAIProvider, got %T", p)
	}
}

func TestFromConfigAnthropic(t *testing.T) {
	p, err := FromConfig(ProviderConfig{
		ID:     "anthropic",
		APIKey: "sk-ant-test",
		API:    APIAnthropic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.ID() != "anthropic" {
		t.Errorf("id = %q", p.ID())
	}
	if _, ok := p.(*AnthropicProvider); !ok {
		t.Errorf("expected *AnthropicProvider, got %T", p)
	}
}

func TestFromConfigDefaultIsOpenAI(t *testing.T) {
	p, err := FromConfig(ProviderConfig{
		ID:      "ollama",
		BaseURL: "http://localhost:11434/v1",
		API:     "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*OpenAIProvider); !ok {
		t.Errorf("empty API should default to OpenAI, got %T", p)
	}
}

func TestFromConfigUnknownAPI(t *testing.T) {
	_, err := FromConfig(ProviderConfig{
		ID:  "custom",
		API: "google-gemini",
	})
	if err == nil {
		t.Error("expected error for unknown API type")
	}
}

func TestFromConfigWithModels(t *testing.T) {
	models := []ModelInfo{
		{ID: "gpt-4o", ProviderID: "openai"},
		{ID: "gpt-4o-mini", ProviderID: "openai"},
	}
	p, err := FromConfig(ProviderConfig{
		ID:     "openai",
		APIKey: "key",
		API:    APIOpenAI,
		Models: models,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Models()) != 2 {
		t.Errorf("models = %d, want 2", len(p.Models()))
	}
}
