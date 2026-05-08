package pipeline

import (
	"strings"
	"testing"
	"time"
)

func TestNewPipeline(t *testing.T) {
	steps := []*Step{
		{ID: "1", Name: "Step 1", Command: &PluginCommand{Plugin: "p", Action: "a"}, State: StepPending},
	}
	p := NewPipeline(steps, DefaultConfig())

	if p.State != StateAwaitingConfirmation {
		t.Errorf("State = %q, want awaiting_confirmation", p.State)
	}
	if p.ID == "" {
		t.Error("expected non-empty ID")
	}
	if len(p.Steps) != 1 {
		t.Errorf("expected 1 step, got %d", len(p.Steps))
	}
	if p.Context == nil {
		t.Error("expected non-nil context")
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.MaxStepRetries != 3 {
		t.Errorf("MaxStepRetries = %d, want 3", cfg.MaxStepRetries)
	}
	if cfg.StepTimeout != 60*time.Second {
		t.Errorf("StepTimeout = %v, want 60s", cfg.StepTimeout)
	}
	if !cfg.FailFast {
		t.Error("FailFast should be true by default")
	}
}

func TestFormatForConfirmation(t *testing.T) {
	steps := []*Step{
		{
			ID:   "1",
			Name: "Get error details",
			Command: &PluginCommand{
				Plugin: "appsignal",
				Action: "get_error",
				Args:   map[string]any{"error_id": "123"},
			},
		},
		{
			ID:   "2",
			Name: "Create Jira issue",
			Command: &PluginCommand{
				Plugin: "jira",
				Action: "create_issue",
				Args:   map[string]any{"title": "Fix error"},
			},
			DependsOn: []string{"1"},
		},
	}
	p := NewPipeline(steps, DefaultConfig())
	text := p.FormatForConfirmation()

	if !strings.Contains(text, "Get error details") {
		t.Error("should contain step 1 name")
	}
	if !strings.Contains(text, "Create Jira issue") {
		t.Error("should contain step 2 name")
	}
	if !strings.Contains(text, "appsignal.get_error") {
		t.Error("should contain step 1 action")
	}
	if !strings.Contains(text, "jira.create_issue") {
		t.Error("should contain step 2 action")
	}
	if !strings.Contains(text, "(y)es") {
		t.Error("should mention (y)es to confirm")
	}
	if !strings.Contains(text, "(n)o") {
		t.Error("should mention (n)o to cancel")
	}
	if !strings.Contains(text, "1.") && !strings.Contains(text, "2.") {
		t.Error("should number the steps")
	}
}

func TestFormatForConfirmationNoDependsOn(t *testing.T) {
	steps := []*Step{
		{
			ID:      "1",
			Name:    "Simple step",
			Command: &PluginCommand{Plugin: "p", Action: "a"},
		},
	}
	p := NewPipeline(steps, DefaultConfig())
	text := p.FormatForConfirmation()

	if strings.Contains(text, "Depends on") {
		t.Error("should not show depends_on for steps without dependencies")
	}
}
