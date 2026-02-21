package config

import (
	"os"
	"testing"
)

const testYAML = `
models:
  providers:
    anthropic:
      api_key: "${ANTHROPIC_API_KEY}"
      api: anthropic-messages
    openai:
      api_key: "${OPENAI_API_KEY}"
      api: openai-completions
    ovh:
      base_url: "${OVH_BASE_URL}"
      api_key: "${OVH_API_KEY}"
      api: openai-completions
      models:
        - id: gpt-oss-120b
          name: GPT OSS 120B
          reasoning: true
          input: [text]
          context_window: 131072
          max_tokens: 131072
          cost:
            input: 0.08
            output: 0.44
    ollama:
      base_url: "http://localhost:11434/v1"
      api: openai-completions
      models:
        - id: llama3
          name: Llama 3 8B
          input: [text]
          context_window: 8192
          cost:
            input: 0
            output: 0
  catalog:
    anthropic/claude-haiku-4:
      alias: haiku
      weight: 90
    anthropic/claude-sonnet-4:
      alias: sonnet
      weight: 50
    anthropic/claude-opus-4-6:
      alias: opus
      weight: 10

routing:
  primary: anthropic/claude-haiku-4
  fallbacks:
    - anthropic/claude-sonnet-4
    - openai/gpt-5.2
    - anthropic/claude-opus-4-6
  pin:
    code: anthropic/claude-sonnet-4
  affinity:
    enabled: true
    store: ~/.opentalon/affinity.json
    decay_days: 30

auth:
  cooldowns:
    initial: "1m"
    max: "1h"
    multiplier: 5
    billing_max_hours: 24
`

func TestParseConfig(t *testing.T) {
	cfg, err := Parse([]byte(testYAML))
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.Models.Providers) != 4 {
		t.Errorf("expected 4 providers, got %d", len(cfg.Models.Providers))
	}

	ovh := cfg.Models.Providers["ovh"]
	if ovh.API != "openai-completions" {
		t.Errorf("ovh api = %q, want openai-completions", ovh.API)
	}
	if len(ovh.Models) != 1 {
		t.Fatalf("ovh models = %d, want 1", len(ovh.Models))
	}
	if ovh.Models[0].ID != "gpt-oss-120b" {
		t.Errorf("ovh model id = %q, want gpt-oss-120b", ovh.Models[0].ID)
	}
	if ovh.Models[0].Cost.Input != 0.08 {
		t.Errorf("ovh model cost.input = %f, want 0.08", ovh.Models[0].Cost.Input)
	}
	if ovh.Models[0].ContextWindow != 131072 {
		t.Errorf("ovh model context_window = %d, want 131072", ovh.Models[0].ContextWindow)
	}
}

func TestParseCatalog(t *testing.T) {
	cfg, err := Parse([]byte(testYAML))
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.Models.Catalog) != 3 {
		t.Errorf("expected 3 catalog entries, got %d", len(cfg.Models.Catalog))
	}

	haiku := cfg.Models.Catalog["anthropic/claude-haiku-4"]
	if haiku.Alias != "haiku" {
		t.Errorf("haiku alias = %q, want haiku", haiku.Alias)
	}
	if haiku.Weight != 90 {
		t.Errorf("haiku weight = %d, want 90", haiku.Weight)
	}

	opus := cfg.Models.Catalog["anthropic/claude-opus-4-6"]
	if opus.Weight != 10 {
		t.Errorf("opus weight = %d, want 10", opus.Weight)
	}
}

func TestParseRouting(t *testing.T) {
	cfg, err := Parse([]byte(testYAML))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Routing.Primary != "anthropic/claude-haiku-4" {
		t.Errorf("primary = %q, want anthropic/claude-haiku-4", cfg.Routing.Primary)
	}
	if len(cfg.Routing.Fallbacks) != 3 {
		t.Errorf("fallbacks = %d, want 3", len(cfg.Routing.Fallbacks))
	}
	if cfg.Routing.Pin["code"] != "anthropic/claude-sonnet-4" {
		t.Errorf("pin[code] = %q, want anthropic/claude-sonnet-4", cfg.Routing.Pin["code"])
	}
	if !cfg.Routing.Affinity.Enabled {
		t.Error("affinity should be enabled")
	}
	if cfg.Routing.Affinity.DecayDays != 30 {
		t.Errorf("decay_days = %d, want 30", cfg.Routing.Affinity.DecayDays)
	}
}

func TestEnvSubstitution(t *testing.T) {
	os.Setenv("ANTHROPIC_API_KEY", "sk-ant-test-123")
	os.Setenv("OVH_BASE_URL", "https://ovh.example.com/v1")
	os.Setenv("OVH_API_KEY", "ovh-key-456")
	defer func() {
		os.Unsetenv("ANTHROPIC_API_KEY")
		os.Unsetenv("OVH_BASE_URL")
		os.Unsetenv("OVH_API_KEY")
	}()

	cfg, err := Parse([]byte(testYAML))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Models.Providers["anthropic"].APIKey != "sk-ant-test-123" {
		t.Errorf("anthropic api_key = %q, want sk-ant-test-123", cfg.Models.Providers["anthropic"].APIKey)
	}
	if cfg.Models.Providers["ovh"].BaseURL != "https://ovh.example.com/v1" {
		t.Errorf("ovh base_url = %q, want https://ovh.example.com/v1", cfg.Models.Providers["ovh"].BaseURL)
	}
	if cfg.Models.Providers["ovh"].APIKey != "ovh-key-456" {
		t.Errorf("ovh api_key = %q, want ovh-key-456", cfg.Models.Providers["ovh"].APIKey)
	}
}

func TestEnvSubstitutionPreservesUnsetVars(t *testing.T) {
	os.Unsetenv("OPENAI_API_KEY")
	cfg, err := Parse([]byte(testYAML))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Models.Providers["openai"].APIKey != "${OPENAI_API_KEY}" {
		t.Errorf("unset env var should be preserved, got %q", cfg.Models.Providers["openai"].APIKey)
	}
}

func TestEnvSubstitutionLiteralURLs(t *testing.T) {
	cfg, err := Parse([]byte(testYAML))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Models.Providers["ollama"].BaseURL != "http://localhost:11434/v1" {
		t.Errorf("literal URL should not be modified, got %q", cfg.Models.Providers["ollama"].BaseURL)
	}
}

func TestParseInvalidYAML(t *testing.T) {
	_, err := Parse([]byte("{{invalid yaml"))
	if err == nil {
		t.Error("expected error for invalid YAML")
	}
}

func TestExpandEnv(t *testing.T) {
	os.Setenv("TEST_VAR", "hello")
	defer os.Unsetenv("TEST_VAR")

	tests := []struct {
		input string
		want  string
	}{
		{"${TEST_VAR}", "hello"},
		{"prefix-${TEST_VAR}-suffix", "prefix-hello-suffix"},
		{"${NONEXISTENT}", "${NONEXISTENT}"},
		{"no vars here", "no vars here"},
		{"", ""},
	}
	for _, tt := range tests {
		got := expandEnv(tt.input)
		if got != tt.want {
			t.Errorf("expandEnv(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
