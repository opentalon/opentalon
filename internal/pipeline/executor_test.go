package pipeline

import (
	"context"
	"strings"
	"testing"
	"time"
)

func successRunner(_ context.Context, plugin, action string, _ map[string]string) StepRunResult {
	return StepRunResult{Content: "executed " + plugin + "." + action}
}

func failRunner(_ context.Context, _, _ string, _ map[string]string) StepRunResult {
	return StepRunResult{Error: "step failed"}
}

func makeCountingRunner() (StepRunnerFunc, *int) {
	count := 0
	return func(_ context.Context, _, _ string, _ map[string]string) StepRunResult {
		count++
		if count <= 2 {
			return StepRunResult{Error: "transient error"}
		}
		return StepRunResult{Content: "success on attempt " + string(rune('0'+count))}
	}, &count
}

func makeSteps(names ...string) []*Step {
	steps := make([]*Step, len(names))
	for i, name := range names {
		steps[i] = &Step{
			ID:   name,
			Name: "Step " + name,
			Command: &PluginCommand{
				Plugin: "test",
				Action: name,
				Args:   map[string]string{},
			},
			State:      StepPending,
			MaxRetries: -1,
		}
	}
	return steps
}

func TestExecutorAllStepsSucceed(t *testing.T) {
	steps := makeSteps("a", "b", "c")
	p := NewPipeline(steps, PipelineConfig{MaxStepRetries: 0, StepTimeout: 10 * time.Second, FailFast: true})

	executor := NewExecutor(successRunner, p.Config)
	result, err := executor.Run(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Error("expected success")
	}
	if p.State != StateCompleted {
		t.Errorf("pipeline state = %q, want completed", p.State)
	}
	if len(result.Steps) != 3 {
		t.Errorf("expected 3 executed steps, got %d", len(result.Steps))
	}
	for _, step := range p.Steps {
		if step.State != StepSucceeded {
			t.Errorf("step %s state = %q, want succeeded", step.ID, step.State)
		}
	}
}

func TestExecutorFailFast(t *testing.T) {
	steps := makeSteps("a", "b")
	p := NewPipeline(steps, PipelineConfig{MaxStepRetries: 0, StepTimeout: 10 * time.Second, FailFast: true})

	executor := NewExecutor(failRunner, p.Config)
	result, err := executor.Run(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if result.Success {
		t.Error("expected failure")
	}
	if p.State != StateFailed {
		t.Errorf("pipeline state = %q, want failed", p.State)
	}
	// Only first step should have been attempted
	if len(result.Steps) != 1 {
		t.Errorf("expected 1 executed step (fail-fast), got %d", len(result.Steps))
	}
	if p.Steps[0].State != StepFailed {
		t.Errorf("step a state = %q, want failed", p.Steps[0].State)
	}
	// Step b should still be pending (never reached)
	if p.Steps[1].State != StepPending {
		t.Errorf("step b state = %q, want pending", p.Steps[1].State)
	}
}

func TestExecutorRetryThenSucceed(t *testing.T) {
	runner, count := makeCountingRunner()
	steps := makeSteps("a")
	p := NewPipeline(steps, PipelineConfig{MaxStepRetries: 3, StepTimeout: 10 * time.Second, FailFast: true})

	executor := NewExecutor(runner, p.Config)
	result, err := executor.Run(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Success {
		t.Error("expected success after retries")
	}
	if *count != 3 {
		t.Errorf("expected 3 attempts, got %d", *count)
	}
	if p.Steps[0].Attempts != 3 {
		t.Errorf("step attempts = %d, want 3", p.Steps[0].Attempts)
	}
	// Should have 3 executed steps (2 failures + 1 success)
	if len(result.Steps) != 3 {
		t.Errorf("expected 3 executed steps, got %d", len(result.Steps))
	}
}

func TestExecutorRetryExhausted(t *testing.T) {
	steps := makeSteps("a")
	p := NewPipeline(steps, PipelineConfig{MaxStepRetries: 1, StepTimeout: 10 * time.Second, FailFast: true})

	executor := NewExecutor(failRunner, p.Config)
	result, err := executor.Run(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if result.Success {
		t.Error("expected failure after retries exhausted")
	}
	if p.Steps[0].State != StepFailed {
		t.Errorf("step state = %q, want failed", p.Steps[0].State)
	}
	// 1 initial + 1 retry = 2 attempts
	if p.Steps[0].Attempts != 2 {
		t.Errorf("step attempts = %d, want 2", p.Steps[0].Attempts)
	}
}

func TestExecutorDependencySkipped(t *testing.T) {
	steps := makeSteps("a", "b")
	steps[1].DependsOn = []string{"a"}
	p := NewPipeline(steps, PipelineConfig{MaxStepRetries: 0, StepTimeout: 10 * time.Second, FailFast: false})

	executor := NewExecutor(failRunner, p.Config)
	result, err := executor.Run(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if result.Success {
		t.Error("expected failure")
	}
	// Step a fails, step b should be skipped because dep not met
	if p.Steps[1].State != StepSkipped {
		t.Errorf("step b state = %q, want skipped", p.Steps[1].State)
	}
}

func TestExecutorContextMerge(t *testing.T) {
	steps := makeSteps("a", "b")
	steps[1].DependsOn = []string{"a"}
	p := NewPipeline(steps, PipelineConfig{MaxStepRetries: 0, StepTimeout: 10 * time.Second, FailFast: true})

	executor := NewExecutor(successRunner, p.Config)
	_, err := executor.Run(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}

	val, ok := p.Context.Get("a", "output")
	if !ok {
		t.Fatal("expected step a output in context")
	}
	if !strings.Contains(val.(string), "executed") {
		t.Errorf("context value = %v", val)
	}
}

func TestExecutorSummaryContent(t *testing.T) {
	steps := makeSteps("a")
	p := NewPipeline(steps, PipelineConfig{MaxStepRetries: 0, StepTimeout: 10 * time.Second, FailFast: true})

	executor := NewExecutor(successRunner, p.Config)
	result, _ := executor.Run(context.Background(), p)

	if !strings.Contains(result.Summary, "successfully") {
		t.Errorf("summary should mention success: %q", result.Summary)
	}
	if !strings.Contains(result.Summary, "Step a") {
		t.Errorf("summary should list step name: %q", result.Summary)
	}
}

func TestExecutorFailedSummary(t *testing.T) {
	steps := makeSteps("a")
	p := NewPipeline(steps, PipelineConfig{MaxStepRetries: 0, StepTimeout: 10 * time.Second, FailFast: true})

	executor := NewExecutor(failRunner, p.Config)
	result, _ := executor.Run(context.Background(), p)

	if !strings.Contains(result.Summary, "failed") {
		t.Errorf("summary should mention failure: %q", result.Summary)
	}
}
