package pipeline

import (
	"context"
	"strings"
	"testing"
	"time"
)

func successRunner(_ context.Context, plugin, action string, _ map[string]any) StepRunResult {
	return StepRunResult{Content: "executed " + plugin + "." + action}
}

func failRunner(_ context.Context, _, _ string, _ map[string]any) StepRunResult {
	return StepRunResult{Error: "step failed"}
}

func makeCountingRunner() (StepRunnerFunc, *int) {
	count := 0
	return func(_ context.Context, _, _ string, _ map[string]any) StepRunResult {
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
				Args:   map[string]any{},
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

// TestExecutorSubstitutesStepReferences exercises the end-to-end path:
// step1 returns structured JSON, step2's args reference it via
// {{step1.output.<path>}}, executor resolves before dispatching step2.
//
// Mirrors the Tesla-reservation flow that motivated the feature: list-
// containers + list-items + schedule-item with two cross-step references.
func TestExecutorSubstitutesStepReferences(t *testing.T) {
	captured := make(map[string]map[string]any)
	runner := func(_ context.Context, plugin, action string, args map[string]any) StepRunResult {
		captured[action] = args
		switch action {
		case "list-containers":
			return StepRunResult{
				Content:           "Containers: 1 total",
				StructuredContent: `{"containers":[{"id":170910,"name":"Berlin Garage"}]}`,
			}
		case "list-items":
			return StepRunResult{
				Content:           "Items: 1 total",
				StructuredContent: `{"items":[{"id":2004556,"name":"Tesla in NYC"}]}`,
			}
		case "schedule-item":
			return StepRunResult{Content: "scheduled"}
		}
		return StepRunResult{Error: "unknown action: " + action}
	}

	steps := []*Step{
		{ID: "step1", Name: "Find container", Command: &PluginCommand{Plugin: "timly", Action: "list-containers", Args: map[string]any{"query": "name:Berlin"}}, MaxRetries: -1, State: StepPending},
		{ID: "step2", Name: "Find item", Command: &PluginCommand{Plugin: "timly", Action: "list-items", Args: map[string]any{"query": "name:Tesla"}}, MaxRetries: -1, State: StepPending},
		{
			ID: "step3", Name: "Schedule",
			Command: &PluginCommand{
				Plugin: "timly", Action: "schedule-item",
				Args: map[string]any{
					"container_id": "{{step1.output.containers[0].id}}",
					"item_id":      "{{step2.output.items[0].id}}",
					"start_date":   "2026-06-01T00:00:00",
				},
			},
			DependsOn: []string{"step1", "step2"}, MaxRetries: -1, State: StepPending,
		},
	}
	p := NewPipeline(steps, PipelineConfig{MaxStepRetries: 0, StepTimeout: 10 * time.Second, FailFast: true})

	res, err := NewExecutor(runner, p.Config).Run(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Success {
		t.Fatalf("expected success, got summary: %s", res.Summary)
	}

	step3 := captured["schedule-item"]
	if step3 == nil {
		t.Fatal("schedule-item never received args")
	}
	// Solo placeholders preserve the resolved value's type — a numeric id
	// stays float64 (from the JSON unmarshal), NOT the literal placeholder
	// string. The wire-boundary stringification (orchestrator) is the only
	// layer that collapses to string, so types stay correct mid-pipeline.
	if id, ok := step3["container_id"].(float64); !ok || id != 170910 {
		t.Errorf("container_id = %v (%T), want float64(170910)", step3["container_id"], step3["container_id"])
	}
	if id, ok := step3["item_id"].(float64); !ok || id != 2004556 {
		t.Errorf("item_id = %v (%T), want float64(2004556)", step3["item_id"], step3["item_id"])
	}
	if step3["start_date"] != "2026-06-01T00:00:00" {
		t.Errorf("start_date mutated: %v", step3["start_date"])
	}
}

// TestExecutorRecordsSubstitutedArgsInTrace asserts that ExecutedStep.Args
// captures the POST-substitution values, not the pre-substitution literals.
// This is the trace the orchestrator records into the session and the
// debug log — operators reading "what really got called" need the
// resolved values.
func TestExecutorRecordsSubstitutedArgsInTrace(t *testing.T) {
	runner := func(_ context.Context, _, action string, _ map[string]any) StepRunResult {
		if action == "step1" {
			return StepRunResult{StructuredContent: `{"id":42}`}
		}
		return StepRunResult{Content: "ok"}
	}
	steps := []*Step{
		{ID: "step1", Command: &PluginCommand{Plugin: "p", Action: "step1", Args: map[string]any{}}, MaxRetries: -1, State: StepPending},
		{ID: "step2", Command: &PluginCommand{Plugin: "p", Action: "step2", Args: map[string]any{"id": "{{step1.output.id}}"}}, DependsOn: []string{"step1"}, MaxRetries: -1, State: StepPending},
	}
	p := NewPipeline(steps, PipelineConfig{MaxStepRetries: 0, StepTimeout: 10 * time.Second, FailFast: true})

	res, _ := NewExecutor(runner, p.Config).Run(context.Background(), p)
	if !res.Success {
		t.Fatal("expected success")
	}
	// ExecutedStep[1] is step2 — its recorded Args should hold the
	// resolved value, not the placeholder.
	if got, ok := res.Steps[1].Args["id"].(float64); !ok || got != 42 {
		t.Errorf("ExecutedStep.Args[id] = %v (%T), want resolved float64(42)", res.Steps[1].Args["id"], res.Steps[1].Args["id"])
	}
}

// TestExecutorFailsFastOnUnresolvedReference ensures a malformed/dangling
// reference is treated as a permanent error of the plan: the step fails
// without entering the retry loop, and on FailFast the pipeline halts. No
// runner attempt is recorded — substitution is pre-dispatch.
func TestExecutorFailsFastOnUnresolvedReference(t *testing.T) {
	calls := 0
	runner := func(_ context.Context, _, _ string, _ map[string]any) StepRunResult {
		calls++
		return StepRunResult{Content: "should not be called"}
	}
	steps := []*Step{
		{
			ID:         "step1",
			Command:    &PluginCommand{Plugin: "p", Action: "a", Args: map[string]any{"x": "{{step99.output.id}}"}},
			MaxRetries: 5, // would retry 5 times for a transient error
			State:      StepPending,
		},
	}
	p := NewPipeline(steps, PipelineConfig{MaxStepRetries: 5, StepTimeout: 10 * time.Second, FailFast: true})

	res, _ := NewExecutor(runner, p.Config).Run(context.Background(), p)
	if res.Success {
		t.Fatal("expected failure")
	}
	if calls != 0 {
		t.Errorf("runner called %d time(s) on unresolved reference; expected 0 (substitution must be pre-dispatch)", calls)
	}
	if !strings.Contains(res.Summary, "produced no output") {
		t.Errorf("summary should explain unresolved-reference; got: %s", res.Summary)
	}
}

// TestExecutorContextStoresStructuredOutput asserts that the parsed
// structured content (not the text content) lands in PipelineContext for
// downstream substitution. This is the difference between "step1.output =
// 'Containers: 1 total'" (useless for path traversal) and "step1.output =
// {containers: [...]}" (traversable).
func TestExecutorContextStoresStructuredOutput(t *testing.T) {
	runner := func(_ context.Context, _, _ string, _ map[string]any) StepRunResult {
		return StepRunResult{
			Content:           "human-readable summary",
			StructuredContent: `{"id":7,"name":"x"}`,
		}
	}
	steps := []*Step{{ID: "step1", Command: &PluginCommand{Plugin: "p", Action: "a", Args: map[string]any{}}, MaxRetries: -1, State: StepPending}}
	p := NewPipeline(steps, PipelineConfig{MaxStepRetries: 0, StepTimeout: 10 * time.Second, FailFast: true})

	if _, err := NewExecutor(runner, p.Config).Run(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	v, ok := p.Context.Get("step1", "output")
	if !ok {
		t.Fatal("missing step1.output")
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected parsed object in context, got %T (%v)", v, v)
	}
	if m["id"].(float64) != 7 {
		t.Errorf("step1.output.id = %v, want 7", m["id"])
	}
}
