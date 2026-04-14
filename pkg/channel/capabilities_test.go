package channel

import (
	"context"
	"testing"
)

func TestWithCapabilitiesRoundtrip(t *testing.T) {
	caps := Capabilities{
		ID:                   "slack",
		Name:                 "Slack",
		Threads:              true,
		ResponseFormat:       FormatSlack,
		ResponseFormatPrompt: "custom prompt",
	}
	ctx := WithCapabilities(context.Background(), caps)
	got := CapabilitiesFromContext(ctx)

	if got.ID != caps.ID {
		t.Errorf("ID: got %q, want %q", got.ID, caps.ID)
	}
	if got.ResponseFormat != caps.ResponseFormat {
		t.Errorf("ResponseFormat: got %q, want %q", got.ResponseFormat, caps.ResponseFormat)
	}
	if got.ResponseFormatPrompt != caps.ResponseFormatPrompt {
		t.Errorf("ResponseFormatPrompt: got %q, want %q", got.ResponseFormatPrompt, caps.ResponseFormatPrompt)
	}
}

func TestCapabilitiesFromContextEmpty(t *testing.T) {
	got := CapabilitiesFromContext(context.Background())
	if got.ID != "" || got.ResponseFormat != "" || got.ResponseFormatPrompt != "" {
		t.Errorf("expected zero-value Capabilities, got %+v", got)
	}
}

func TestResponseFormatConstants(t *testing.T) {
	formats := []ResponseFormat{FormatText, FormatMarkdown, FormatSlack, FormatHTML, FormatTelegram, FormatTeams, FormatWhatsApp, FormatDiscord}
	seen := make(map[ResponseFormat]bool)
	for _, f := range formats {
		if f == "" {
			t.Errorf("ResponseFormat constant must not be empty")
		}
		if seen[f] {
			t.Errorf("duplicate ResponseFormat constant %q", f)
		}
		seen[f] = true
	}
}
