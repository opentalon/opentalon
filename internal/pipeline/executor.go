package pipeline

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// StepRunnerFunc executes a single plugin command. The orchestrator provides an adapter
// that bridges its own ToolCall/ToolResult types to these pipeline-local types.
type StepRunnerFunc func(ctx context.Context, plugin, action string, args map[string]string) StepRunResult

// StepRunResult is the pipeline-local result of running a step.
type StepRunResult struct {
	Content string
	Error   string
}

// Executor runs a pipeline's steps sequentially with retry logic.
type Executor struct {
	runner StepRunnerFunc
	config PipelineConfig
}

// ExecutionResult holds the outcome of a full pipeline execution.
type ExecutionResult struct {
	Success bool
	Summary string
	Steps   []*ExecutedStep
}

// ExecutedStep records each attempt made during pipeline execution.
type ExecutedStep struct {
	Plugin  string
	Action  string
	Args    map[string]string
	Content string
	Error   string
}

// NewExecutor creates an executor with the given step runner and config.
func NewExecutor(runner StepRunnerFunc, config PipelineConfig) *Executor {
	return &Executor{runner: runner, config: config}
}

// Run executes all steps in the pipeline sequentially, respecting DependsOn ordering.
func (e *Executor) Run(ctx context.Context, p *Pipeline) (*ExecutionResult, error) {
	p.State = StateRunning
	result := &ExecutionResult{}

	completed := make(map[string]bool)
	anyFailed := false

	for _, step := range p.Steps {
		// Check dependencies
		depsMet := true
		for _, dep := range step.DependsOn {
			if !completed[dep] {
				depsMet = false
				break
			}
		}
		if !depsMet {
			step.State = StepSkipped
			step.Result = &StepResult{
				Success: false,
				Output:  "skipped: dependency not completed",
			}
			if e.config.FailFast {
				p.State = StateFailed
				result.Success = false
				result.Summary = buildSummary(p, false)
				return result, nil
			}
			continue
		}

		maxRetries := step.MaxRetries
		if maxRetries < 0 {
			maxRetries = e.config.MaxStepRetries
		}

		success := false
		step.State = StepRunning

		for attempt := 0; attempt <= maxRetries; attempt++ {
			step.Attempts = attempt + 1

			var stepCtx context.Context
			var cancel context.CancelFunc
			if e.config.StepTimeout > 0 {
				stepCtx, cancel = context.WithTimeout(ctx, e.config.StepTimeout)
			} else {
				stepCtx, cancel = context.WithCancel(ctx)
			}

			runResult := e.runner(stepCtx, step.Command.Plugin, step.Command.Action, step.Command.Args)
			cancel()

			result.Steps = append(result.Steps, &ExecutedStep{
				Plugin:  step.Command.Plugin,
				Action:  step.Command.Action,
				Args:    step.Command.Args,
				Content: runResult.Content,
				Error:   runResult.Error,
			})

			if runResult.Error == "" {
				step.State = StepSucceeded
				step.Result = &StepResult{
					Success: true,
					Output:  runResult.Content,
					Data:    map[string]any{"output": runResult.Content},
				}
				p.Context.Merge(step.ID, step.Result.Data)
				completed[step.ID] = true
				success = true
				break
			}

			// Last attempt failed
			if attempt == maxRetries {
				step.State = StepFailed
				step.Result = &StepResult{
					Success: false,
					Output:  runResult.Error,
				}
			}

			// Brief backoff before retry
			if attempt < maxRetries {
				select {
				case <-ctx.Done():
					step.State = StepFailed
					step.Result = &StepResult{
						Success: false,
						Output:  "context cancelled during retry backoff",
					}
					p.State = StateFailed
					result.Success = false
					result.Summary = buildSummary(p, false)
					return result, nil
				case <-time.After(time.Duration(attempt+1) * 500 * time.Millisecond):
				}
			}
		}

		if !success {
			anyFailed = true
			if e.config.FailFast {
				p.State = StateFailed
				result.Success = false
				result.Summary = buildSummary(p, false)
				return result, nil
			}
		}
	}

	if anyFailed {
		p.State = StateFailed
		result.Success = false
		result.Summary = buildSummary(p, false)
	} else {
		p.State = StateCompleted
		result.Success = true
		result.Summary = buildSummary(p, true)
	}
	return result, nil
}

func buildSummary(p *Pipeline, success bool) string {
	var sb strings.Builder
	if success {
		sb.WriteString("Pipeline completed successfully.\n\n")
	} else {
		sb.WriteString("Pipeline failed.\n\n")
	}
	for i, step := range p.Steps {
		status := string(step.State)
		sb.WriteString(fmt.Sprintf("%d. %s — %s", i+1, step.Name, status))
		if step.Result != nil && step.Result.Output != "" {
			output := step.Result.Output
			if len(output) > 200 {
				output = output[:200] + "..."
			}
			sb.WriteString(fmt.Sprintf(": %s", output))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}
