package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// StepRunnerFunc executes a single plugin command. The orchestrator provides an adapter
// that bridges its own ToolCall/ToolResult types to these pipeline-local types.
//
// Args carries typed values; the runner is responsible for the string conversion
// at the wire-level boundary (per-protocol — gRPC plugin proto declares args as
// map<string,string>, so the boundary is the natural place for typed-aware
// rendering — see pipelineArgsToWire in the orchestrator package).
type StepRunnerFunc func(ctx context.Context, plugin, action string, args map[string]any) StepRunResult

// StepRunResult is the pipeline-local result of running a step.
//
// StructuredContent, when non-empty, is the JSON-encoded structured payload
// returned by the underlying tool (MCP `structuredContent`, spec revision
// 2025-06+). The executor parses it for step-output substitution; tools
// without a structured channel leave it empty and substitution falls back to
// the text content.
type StepRunResult struct {
	Content           string
	StructuredContent string
	Error             string
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
//
// Args is captured POST-substitution so the trace shows the values that
// actually reached the runner — useful for understanding "what really got
// called" when a step fails. Unsubstituted placeholders never appear here:
// resolution happens before runner-dispatch and substitution failures take
// the step straight to StepFailed without an attempt entry.
type ExecutedStep struct {
	Plugin  string
	Action  string
	Args    map[string]any
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

		// Resolve {{stepN.output.<path>}} references in args against the
		// pipeline context BEFORE the first runner attempt. A failure here
		// (unresolved ref / malformed path) is a deterministic error of
		// the plan, not a transient one — fail the step immediately
		// without retrying. Tracked as one ExecutedStep so the trace
		// records the substitution failure.
		resolvedArgs, resolveErr := resolveArgs(step.Command.Args, p.Context)
		if resolveErr != nil {
			step.State = StepFailed
			step.Result = &StepResult{
				Success: false,
				Output:  resolveErr.Error(),
			}
			result.Steps = append(result.Steps, &ExecutedStep{
				Plugin: step.Command.Plugin,
				Action: step.Command.Action,
				Args:   step.Command.Args,
				Error:  resolveErr.Error(),
			})
			anyFailed = true
			if e.config.FailFast {
				p.State = StateFailed
				result.Success = false
				result.Summary = buildSummary(p, false)
				return result, nil
			}
			continue
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

			runResult := e.runner(stepCtx, step.Command.Plugin, step.Command.Action, resolvedArgs)
			cancel()

			result.Steps = append(result.Steps, &ExecutedStep{
				Plugin:  step.Command.Plugin,
				Action:  step.Command.Action,
				Args:    resolvedArgs,
				Content: runResult.Content,
				Error:   runResult.Error,
			})

			if runResult.Error == "" {
				step.State = StepSucceeded
				step.Result = &StepResult{
					Success: true,
					Output:  runResult.Content,
					Data:    map[string]any{"output": parseStepOutput(runResult)},
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

// parseStepOutput chooses what to expose as `<step>.output` to subsequent
// step references. The structured channel wins when it parses as JSON —
// references like `containers[0].id` only make sense against typed data.
// We fall back to the text content (string) when no structured payload is
// available; references that try to traverse it will fail with "expected
// object, got string", which is the right error to surface.
func parseStepOutput(r StepRunResult) any {
	if r.StructuredContent == "" {
		return r.Content
	}
	var parsed any
	if err := json.Unmarshal([]byte(r.StructuredContent), &parsed); err != nil {
		return r.Content
	}
	return parsed
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
		fmt.Fprintf(&sb, "%d. %s — %s", i+1, step.Name, status)
		if step.Result != nil && step.Result.Output != "" {
			output := step.Result.Output
			if len(output) > 200 {
				output = output[:200] + "..."
			}
			fmt.Fprintf(&sb, ": %s", output)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}
