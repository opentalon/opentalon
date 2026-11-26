package orchestrator

import (
	"context"
	"math"
	"testing"

	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

// answeringLLM answers as ref, charging ratio times the request's estimate
// (a million tokens without a ratio).
type answeringLLM struct {
	ref   provider.ModelRef
	ratio float64
}

func (a answeringLLM) Complete(ctx context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	input := 1_000_000
	if a.ratio > 0 {
		input = int(a.ratio * float64(estimateRequestTokens(req)))
	}
	return &provider.CompletionResponse{ModelRef: a.ref, Content: "done",
		Usage: provider.Usage{InputTokens: input, OutputTokens: 1}}, nil
}

// The provider's count teaches the estimate of the model that answered, not
// of the one the profile selected.
func TestCalibrationObservesTheAnsweringModel(t *testing.T) {
	for _, tc := range []struct {
		name               string
		answeredBy         provider.ModelRef
		learned, untouched string
	}{
		{"the selected model answers", "anthropic/claude-x", "anthropic/claude-x", "default/default-model"},
		{"the default model answers in its place", "default/default-model", "default/default-model", "anthropic/claude-x"},
		{"the answer names no model", "", "", "anthropic/claude-x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions := state.NewSessionStore("")
			sessions.Create(state.SessionParams{ID: "s1"})
			o := NewWithRules(answeringLLM{ref: tc.answeredBy}, DefaultParser, NewToolRegistry(), state.NewMemoryStore(""), sessions, OrchestratorOpts{})
			ctx := profile.WithProfile(context.Background(), &profile.Profile{EntityID: "e1", Model: "anthropic/claude-x"})
			if _, err := o.Run(ctx, "s1", "Hi"); err != nil {
				t.Fatal(err)
			}
			want := maxCalibrationFactor
			if tc.answeredBy == "" {
				want = 1.0
			}
			if got := o.calibrators.factor(tc.learned); got != want {
				t.Errorf("calibration of %q = %v, want %v", tc.learned, got, want)
			}
			if got := o.calibrators.factor(tc.untouched); got != 1.0 {
				t.Errorf("calibration of %q = %v, want 1.0: that model did not answer", tc.untouched, got)
			}
		})
	}
}

// The factor is fed the raw estimate: an answer at 1.6 times the estimate
// sets 1.6, whatever the factor of another model.
func TestCalibrationLearnsTheRatioOfTheRawEstimate(t *testing.T) {
	sessions := state.NewSessionStore("")
	sessions.Create(state.SessionParams{ID: "s1"})
	o := NewWithRules(answeringLLM{ref: "default/default-model", ratio: 1.6}, DefaultParser, NewToolRegistry(),
		state.NewMemoryStore(""), sessions, OrchestratorOpts{})
	o.calibrators.observe("anthropic/claude-x", 100, 200)
	ctx := profile.WithProfile(context.Background(), &profile.Profile{EntityID: "e1", Model: "anthropic/claude-x"})
	for range 2 {
		if _, err := o.Run(ctx, "s1", "Hi"); err != nil {
			t.Fatal(err)
		}
	}
	if got := o.calibrators.factor("default/default-model"); math.Abs(got-1.6) > 0.01 {
		t.Errorf("default's calibration = %v, want 1.6, the ratio the default's answers showed", got)
	}
	if got := o.calibrators.factor("anthropic/claude-x"); got != maxCalibrationFactor {
		t.Errorf("calibration of claude-x = %v, want it unchanged at %v", got, maxCalibrationFactor)
	}
}
