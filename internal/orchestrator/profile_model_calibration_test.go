package orchestrator

import (
	"context"
	"testing"

	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

// answeringLLM answers every request as model, charging far more input
// tokens than any estimate; fallback marks the answer as the default model's
// for a request for another model.
type answeringLLM struct {
	model    string
	fallback bool
}

func (a answeringLLM) Complete(context.Context, *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	return &provider.CompletionResponse{Model: a.model, Fallback: a.fallback, Content: "done",
		Usage: provider.Usage{InputTokens: 1_000_000, OutputTokens: 1}}, nil
}

// TestCalibrationObservesTheAnsweringModel: the token count of an answer
// corrects the estimate of the model that answered, under the key its
// requests look it up by: the requested model's, also when its API reports a
// longer id, and the default's ("") when the default chain answers a request
// that named another model, whose correction stays as it was.
func TestCalibrationObservesTheAnsweringModel(t *testing.T) {
	cases := []struct {
		name         string
		profileModel string
		answeredBy   string
		fallback     bool
		learned      string // the calibration key the answer corrects
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
