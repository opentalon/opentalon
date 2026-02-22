package provider

import "fmt"

const (
	APIOpenAI    = "openai-completions"
	APIAnthropic = "anthropic-messages"
)

// ProviderConfig mirrors config.ProviderConfig to avoid circular imports.
type ProviderConfig struct {
	ID      string
	BaseURL string
	APIKey  string
	API     string
	Models  []ModelInfo
}

// FromConfig creates a Provider from a config entry. The api field
// determines which wire format to use:
//   - "openai-completions"  -> OpenAI-compatible (OpenAI, OVH, Ollama, vLLM, etc.)
//   - "anthropic-messages"  -> Anthropic Messages API
func FromConfig(cfg ProviderConfig) (Provider, error) {
	switch cfg.API {
	case APIOpenAI, "":
		return NewOpenAIProvider(cfg.ID, cfg.BaseURL, cfg.APIKey, cfg.Models), nil
	case APIAnthropic:
		return NewAnthropicProvider(cfg.ID, cfg.BaseURL, cfg.APIKey, cfg.Models), nil
	default:
		return nil, fmt.Errorf("unknown api type %q for provider %q (supported: %s, %s)",
			cfg.API, cfg.ID, APIOpenAI, APIAnthropic)
	}
}
