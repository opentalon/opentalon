package pipeline

import (
	"fmt"
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
		sb.WriteString(fmt.Sprintf("%d. **%s**\n", i+1, step.Name))
		if step.Command != nil {
			sb.WriteString(fmt.Sprintf("   Action: `%s.%s`\n", step.Command.Plugin, step.Command.Action))
			if len(step.Command.Args) > 0 {
				sb.WriteString("   Args: ")
				parts := make([]string, 0, len(step.Command.Args))
				for k, v := range step.Command.Args {
					parts = append(parts, fmt.Sprintf("%s=%s", k, v))
				}
				sb.WriteString(strings.Join(parts, ", "))
				sb.WriteString("\n")
			}
		}
		if len(step.DependsOn) > 0 {
			sb.WriteString(fmt.Sprintf("   Depends on: %s\n", strings.Join(step.DependsOn, ", ")))
		}
	}
	sb.WriteString("\nProceed? (y)es / (n)o")
	return sb.String()
}
