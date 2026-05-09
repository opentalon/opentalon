package provider

import "fmt"

const (
	APIOpenAI    = "openai-completions"
	APIAnthropic = "anthropic-messages"
)

// ProviderConfig mirrors config.ProviderConfig to avoid circular imports.
//
// DebugSink and DebugResolve are optional and currently consumed only by the
// OpenAI-compatible provider (the Anthropic provider has no raw HTTP
// capture today). Wire both from main.go to enable per-session /debug
// capture; leaving either nil disables capture entirely.
type ProviderConfig struct {
	ID           string
	BaseURL      string
	APIKey       string
	API          string
	Models       []ModelInfo
	DebugSink    DebugEventSink
	DebugResolve DebugContextResolver
}

// FromConfig creates a Provider from a config entry. The api field
// determines which wire format to use:
//   - "openai-completions"  -> OpenAI-compatible (OpenAI, OVH, Ollama, vLLM, etc.)
//   - "anthropic-messages"  -> Anthropic Messages API
func FromConfig(cfg ProviderConfig) (Provider, error) {
	switch cfg.API {
	case APIOpenAI, "":
		opts := []OpenAIOption{}
		if cfg.DebugSink != nil {
			opts = append(opts, WithOpenAIDebugSink(cfg.DebugSink))
		}
		if cfg.DebugResolve != nil {
			opts = append(opts, WithOpenAIDebugResolver(cfg.DebugResolve))
		}
		return NewOpenAIProvider(cfg.ID, cfg.BaseURL, cfg.APIKey, cfg.Models, opts...), nil
	case APIAnthropic:
		return NewAnthropicProvider(cfg.ID, cfg.BaseURL, cfg.APIKey, cfg.Models), nil
	default:
		return nil, fmt.Errorf("unknown api type %q for provider %q (supported: %s, %s)",
			cfg.API, cfg.ID, APIOpenAI, APIAnthropic)
	}
}
