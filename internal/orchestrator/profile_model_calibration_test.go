package orchestrator

import (
	"context"
	"math"
	"testing"

	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

type answeringLLM struct {
	model    string
	fallback bool
	ratio    float64
}

func (a answeringLLM) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	input := 1_000_000
	if a.ratio > 0 {
		input = int(a.ratio * float64(fitRequestToWindow(ctx, req, 0, 0, 1.0)))
	}
	return &provider.CompletionResponse{Model: a.model, Fallback: a.fallback, Content: "done",
		Usage: provider.Usage{InputTokens: input, OutputTokens: 1}}, nil
}

func TestCalibrationObservesTheAnsweringModel(t *testing.T) {
	cases := []struct {
		name         string
		profileModel string
		answeredBy   string
		fallback     bool
		learned      string
		untouched    string
	}{
		{"the selected model answers", "anthropic/claude-x", "claude-x-20261001", false, "claude-x", ""},
		{"the default model answers in its place", "anthropic/claude-x", "default-model", true, "", "claude-x"},
		{"no profile model", "", "default-model", false, "", "claude-x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sessions := state.NewSessionStore("")
			sessions.Create(state.SessionParams{ID: "s1"})
			o := NewWithRules(answeringLLM{model: tc.answeredBy, fallback: tc.fallback}, DefaultParser, NewToolRegistry(), state.NewMemoryStore(""), sessions, OrchestratorOpts{})
			ctx := profile.WithProfile(context.Background(), &profile.Profile{EntityID: "e1", Model: tc.profileModel})
			if _, err := o.Run(ctx, "s1", "Hi"); err != nil {
				t.Fatal(err)
			}
			if got := o.calibrators.factor(tc.learned); got != maxCalibrationFactor {
				t.Errorf("calibration of %q = %v, want %v from the answer", tc.learned, got, maxCalibrationFactor)
			}
			if got := o.calibrators.factor(tc.untouched); got != 1.0 {
				t.Errorf("calibration of %q = %v, want 1.0: that model did not answer", tc.untouched, got)
			}
		})
	}
}

func TestCalibrationAfterAFallback(t *testing.T) {
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	o := NewWithRules(answeringLLM{model: "default-model", fallback: true, ratio: 1.6}, DefaultParser, NewToolRegistry(),
		state.NewMemoryStore(""), sessions, OrchestratorOpts{})
	o.calibrators.observe("claude-x", 100, 200)
	ctx := profile.WithProfile(context.Background(), &profile.Profile{EntityID: "e1", Model: "anthropic/claude-x"})
	if _, err := o.Run(ctx, "s1", "Hi"); err != nil {
		t.Fatal(err)
	}
	if got := o.calibrators.factor(""); math.Abs(got-1.6) > 0.01 {
		t.Errorf("default's calibration = %v, want 1.6, the ratio the default's answer showed", got)
	}
	if got := o.calibrators.factor("claude-x"); got != maxCalibrationFactor {
		t.Errorf("calibration of claude-x = %v, want it unchanged at %v", got, maxCalibrationFactor)
	}
}
