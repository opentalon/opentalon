package pipeline

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// PipelineState represents the lifecycle state of a pipeline.
type PipelineState string

const (
	StatePlanned              PipelineState = "planned"
	StateAwaitingConfirmation PipelineState = "awaiting_confirmation"
	StateRunning              PipelineState = "running"
	StateCompleted            PipelineState = "completed"
	StateFailed               PipelineState = "failed"
	StateRejected             PipelineState = "rejected"
)

// PipelineConfig holds execution settings for a pipeline.
type PipelineConfig struct {
	MaxStepRetries int
	StepTimeout    time.Duration
	FailFast       bool
}

// DefaultConfig returns sensible defaults for pipeline execution.
func DefaultConfig() PipelineConfig {
	return PipelineConfig{
		MaxStepRetries: 3,
		StepTimeout:    60 * time.Second,
		FailFast:       true,
	}
}

// Pipeline is a planned sequence of steps to execute.
type Pipeline struct {
	ID        string
	Steps     []*Step
	State     PipelineState
	Config    PipelineConfig
	Context   *PipelineContext
	CreatedAt time.Time
}

// NewPipeline creates a pipeline from planner-produced steps with the given config.
func NewPipeline(steps []*Step, cfg PipelineConfig) *Pipeline {
	return &Pipeline{
		ID:        fmt.Sprintf("pipeline-%d", time.Now().UnixNano()),
		Steps:     steps,
		State:     StateAwaitingConfirmation,
		Config:    cfg,
		Context:   NewContext(),
		CreatedAt: time.Now(),
	}
}

// FormatForConfirmation renders a human-readable plan for user approval.
func (p *Pipeline) FormatForConfirmation() string {
	var sb strings.Builder
	sb.WriteString("I've created a plan with the following steps:\n\n")
	for i, step := range p.Steps {
		fmt.Fprintf(&sb, "%d. **%s**\n", i+1, step.Name)
		if step.Command != nil {
			fmt.Fprintf(&sb, "   Action: `%s.%s`\n", step.Command.Plugin, step.Command.Action)
			if len(step.Command.Args) > 0 {
				sb.WriteString("   Args: ")
				// Stable order so the confirmation message is deterministic
				// across renders (Go map iteration is randomised).
				keys := make([]string, 0, len(step.Command.Args))
				for k := range step.Command.Args {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				parts := make([]string, 0, len(keys))
				for _, k := range keys {
					parts = append(parts, fmt.Sprintf("%s=%v", k, step.Command.Args[k]))
				}
				sb.WriteString(strings.Join(parts, ", "))
				sb.WriteString("\n")
			}
		}
		if len(step.DependsOn) > 0 {
			fmt.Fprintf(&sb, "   Depends on: %s\n", strings.Join(step.DependsOn, ", "))
		}
	}
	sb.WriteString("\nProceed? (y)es / (n)o")
	return sb.String()
}
