package provider

import (
	"fmt"

	"github.com/opentalon/opentalon/internal/state/store/events/emit"
)

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
//
// EventSink is the structured session-event sink. It is also currently
// consumed only by the OpenAI-compatible provider; the Anthropic
// provider will adopt it in a follow-up. Always-on by design — there is
// no per-session resolver — so a non-nil sink enables capture for every
// LLM call. A nil sink disables it.
type ProviderConfig struct {
	ID           string
	BaseURL      string
	APIKey       string
	API          string
	Models       []ModelInfo
	DebugSink    DebugEventSink
	DebugResolve DebugContextResolver
	EventSink    emit.Sink
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
		if cfg.EventSink != nil {
			opts = append(opts, WithOpenAISessionEventSink(cfg.EventSink))
		}
		return NewOpenAIProvider(cfg.ID, cfg.BaseURL, cfg.APIKey, cfg.Models, opts...), nil
	case APIAnthropic:
		return NewAnthropicProvider(cfg.ID, cfg.BaseURL, cfg.APIKey, cfg.Models), nil
	default:
		return nil, fmt.Errorf("unknown api type %q for provider %q (supported: %s, %s)",
			cfg.API, cfg.ID, APIOpenAI, APIAnthropic)
	}
}
