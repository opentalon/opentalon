package provider

import "testing"

func TestModelRefParsing(t *testing.T) {
	tests := []struct {
		input    string
		provider string
		model    string
		valid    bool
	}{
		{"anthropic/claude-opus-4-6", "anthropic", "claude-opus-4-6", true},
		{"openai/gpt-5.2", "openai", "gpt-5.2", true},
		{"ovh/gpt-oss-120b", "ovh", "gpt-oss-120b", true},
		{"ollama/llama3", "ollama", "llama3", true},
		{"invalid", "", "invalid", false},
		{"", "", "", false},
		{"a/b/c", "a", "b/c", true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			ref := ModelRef(tt.input)
			if got := ref.Provider(); got != tt.provider {
				t.Errorf("Provider() = %q, want %q", got, tt.provider)
			}
			if got := ref.Model(); got != tt.model {
				t.Errorf("Model() = %q, want %q", got, tt.model)
			}
			if got := ref.Valid(); got != tt.valid {
				t.Errorf("Valid() = %v, want %v", got, tt.valid)
			}
		})
	}
}

func TestNewModelRef(t *testing.T) {
	ref := NewModelRef("anthropic", "claude-opus-4-6")
	if ref.String() != "anthropic/claude-opus-4-6" {
		t.Errorf("got %q, want %q", ref.String(), "anthropic/claude-opus-4-6")
	}
}

func TestParseModelRef(t *testing.T) {
	ref, err := ParseModelRef("anthropic/claude-opus-4-6")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Provider() != "anthropic" || ref.Model() != "claude-opus-4-6" {
		t.Errorf("unexpected ref: %s", ref)
	}

	_, err = ParseModelRef("invalid")
	if err == nil {
		t.Error("expected error for invalid ref")
	}
}

func TestModelInfoRef(t *testing.T) {
	info := ModelInfo{ID: "claude-opus-4-6", ProviderID: "anthropic"}
	if got := info.Ref().String(); got != "anthropic/claude-opus-4-6" {
		t.Errorf("Ref() = %q, want %q", got, "anthropic/claude-opus-4-6")
	}
}

func TestModelInfoSupportsFeature(t *testing.T) {
	info := ModelInfo{Features: []Feature{FeatureStreaming, FeatureReasoning}}
	if !info.SupportsFeature(FeatureStreaming) {
		t.Error("expected SupportsFeature(streaming) = true")
	}
	if info.SupportsFeature(FeatureImages) {
		t.Error("expected SupportsFeature(images) = false")
	}
}
