package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/profile"
	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

type sleepExecutor struct{ delay time.Duration }

func (s *sleepExecutor) Execute(ctx context.Context, call ToolCall) ToolResult {
	select {
	case <-ctx.Done():
		return ToolResult{CallID: call.ID, Error: ctx.Err().Error()}
	case <-time.After(s.delay):
		return ToolResult{CallID: call.ID, Content: "slept"}
	}
}

type usageLLM struct {
	delay time.Duration
	model string
}

func (u *usageLLM) Complete(ctx context.Context, _ *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(u.delay):
		return &provider.CompletionResponse{Content: "answer", Model: u.model, Usage: provider.Usage{InputTokens: 11, OutputTokens: 3}}, nil
	}
}

func deadlineTestOrch(llm LLMClient, opts OrchestratorOpts) *Orchestrator {
	registry := NewToolRegistry()
	_ = registry.Register(PluginCapability{
		Name:    "slow",
		Actions: []Action{{Name: "wait"}},
	}, &sleepExecutor{delay: 300 * time.Millisecond})
	opts.Subprocess.Enabled = true
	return NewWithRules(llm, DefaultParser, registry, state.NewMemoryStore(""), state.NewSessionStore(""), opts)
}

func parallelTasksArg(n int) map[string]string {
	tasks := make([]map[string]string, n)
	for i := range tasks {
		tasks[i] = map[string]string{"task": fmt.Sprintf("task %d", i+1)}
	}
	raw, _ := json.Marshal(tasks)
	return map[string]string{"tasks": string(raw)}
}

func TestCallbackTimeoutDefaultsToThirtySeconds(t *testing.T) {
	orch := deadlineTestOrch(&fakeLLM{}, OrchestratorOpts{})
	if orch.guard.Timeout != 30*time.Second {
		t.Errorf("guard timeout = %s, want 30s", orch.guard.Timeout)
	}
	if orch.guard.MaxResponseBytes != 64*1024 {
		t.Errorf("guard max response bytes = %d, want 65536", orch.guard.MaxResponseBytes)
	}
}

func TestCallbackTimeoutCutsNormalCallback(t *testing.T) {
	orch := deadlineTestOrch(&fakeLLM{}, OrchestratorOpts{CallbackTimeout: 50 * time.Millisecond})
	_, err := orch.RunAction(context.Background(), "slow", "wait", nil)
	if err == nil || !strings.Contains(err.Error(), "timed out after 50ms") {
		t.Fatalf("err = %v, want timeout after 50ms", err)
	}
}

func TestCallbackTimeoutAllowsLongerNormalCallback(t *testing.T) {
	orch := deadlineTestOrch(&fakeLLM{}, OrchestratorOpts{CallbackTimeout: 2 * time.Second})
	content, err := orch.RunAction(context.Background(), "slow", "wait", nil)
	if err != nil || content != "slept" {
		t.Fatalf("content = %q, err = %v", content, err)
	}
}

func TestSubprocessRunOutlivesCallbackTimeout(t *testing.T) {
	orch := deadlineTestOrch(&usageLLM{delay: 150 * time.Millisecond}, OrchestratorOpts{
		CallbackTimeout: 50 * time.Millisecond,
		Subprocess:      SubprocessConfig{DefaultTimeout: 2 * time.Second},
	})
	content, err := orch.RunAction(context.Background(), "_subprocess", "run", map[string]string{"task": "think"})
	if err != nil {
		t.Fatalf("subprocess run cut by callback timeout: %v", err)
	}
	if content != "answer" {
		t.Errorf("content = %q", content)
	}
}

func TestSubprocessParallelOutlivesSingleTaskTimeout(t *testing.T) {
	orch := deadlineTestOrch(&usageLLM{delay: 80 * time.Millisecond}, OrchestratorOpts{
		CallbackTimeout: 20 * time.Millisecond,
		Subprocess:      SubprocessConfig{DefaultTimeout: 150 * time.Millisecond, MaxParallel: 1},
	})
	_, structured, err := orch.RunActionResult(context.Background(), "_subprocess", "parallel", parallelTasksArg(3))
	if err != nil {
		t.Fatalf("parallel cut early: %v", err)
	}
	var results []parallelTaskResult
	if err := json.Unmarshal([]byte(structured), &results); err != nil {
		t.Fatalf("structured content is not valid JSON: %v", err)
	}
	for i, r := range results {
		if r.Error != "" || r.Response != "answer" {
			t.Errorf("task %d = %+v", i+1, r)
		}
	}
}

func TestSubprocessCallbackDeadline(t *testing.T) {
	const m = subprocessDeadlineMargin
	cases := []struct {
		name        string
		callback    time.Duration
		timeout     time.Duration
		maxParallel int
		action      string
		args        map[string]string
		want        time.Duration
	}{
		{"run uses subprocess timeout", 30 * time.Second, 60 * time.Second, 0, "run", map[string]string{"task": "x"}, 60*time.Second + m},
		{"run defaults timeout to 60s", 30 * time.Second, 0, 0, "run", map[string]string{"task": "x"}, 60*time.Second + m},
		{"callback timeout wins when larger", 10 * time.Minute, 60 * time.Second, 4, "parallel", parallelTasksArg(16), 10 * time.Minute},
		{"16 tasks at 4", 30 * time.Second, 60 * time.Second, 4, "parallel", parallelTasksArg(16), 4*60*time.Second + m},
		{"16 tasks at 8", 30 * time.Second, 60 * time.Second, 8, "parallel", parallelTasksArg(16), 2*60*time.Second + m},
		{"5 tasks at 4 rounds up", 30 * time.Second, 60 * time.Second, 4, "parallel", parallelTasksArg(5), 2*60*time.Second + m},
		{"4 tasks at 4 is one batch", 30 * time.Second, 60 * time.Second, 4, "parallel", parallelTasksArg(4), 60*time.Second + m},
		{"max_parallel unset defaults to 4", 30 * time.Second, 60 * time.Second, 0, "parallel", parallelTasksArg(16), 4*60*time.Second + m},
		{"max_parallel above cap clamps to 8", 30 * time.Second, 60 * time.Second, 20, "parallel", parallelTasksArg(16), 2*60*time.Second + m},
		{"invalid tasks counts as one batch", 30 * time.Second, 60 * time.Second, 4, "parallel", map[string]string{"tasks": "not json"}, 60*time.Second + m},
		{"too many tasks counts as one batch", 30 * time.Second, 60 * time.Second, 4, "parallel", parallelTasksArg(17), 60*time.Second + m},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orch := deadlineTestOrch(&fakeLLM{}, OrchestratorOpts{
				CallbackTimeout: tc.callback,
				Subprocess:      SubprocessConfig{DefaultTimeout: tc.timeout, MaxParallel: tc.maxParallel},
			})
			got := orch.callDeadline(ToolCall{Plugin: "_subprocess", Action: tc.action, Args: tc.args})
			if got != tc.want {
				t.Errorf("deadline = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestCallDeadlineNonSubprocessUsesCallbackTimeout(t *testing.T) {
	orch := deadlineTestOrch(&fakeLLM{}, OrchestratorOpts{CallbackTimeout: 45 * time.Second})
	if got := orch.callDeadline(ToolCall{Plugin: "slow", Action: "wait"}); got != 45*time.Second {
		t.Errorf("deadline = %s, want 45s", got)
	}
}

func TestSubprocessUsageRecordedPerEntity(t *testing.T) {
	usage := &usageSpy{}
	orch := deadlineTestOrch(&usageLLM{model: "m-1"}, OrchestratorOpts{UsageRecorder: usage})
	ctx := profile.WithProfile(context.Background(), &profile.Profile{EntityID: "ent1"})
	ctx = actor.WithSessionID(ctx, "sess1")

	if _, err := orch.RunAction(ctx, "_subprocess", "parallel", parallelTasksArg(3)); err != nil {
		t.Fatal(err)
	}
	if len(usage.calls) != 3 {
		t.Fatalf("recorded %d usage rows, want 3: %+v", len(usage.calls), usage.calls)
	}
	for _, c := range usage.calls {
		if c.entityID != "ent1" || c.sessionID != "sess1" || c.modelID != "m-1" || c.in != 11 || c.out != 3 {
			t.Errorf("usage row = %+v", c)
		}
	}
}

func TestSubprocessUsageWithoutIdentityIsUnattributed(t *testing.T) {
	usage := &usageSpy{}
	orch := deadlineTestOrch(&usageLLM{model: "m-1"}, OrchestratorOpts{UsageRecorder: usage})

	if _, err := orch.RunAction(context.Background(), "_subprocess", "run", map[string]string{"task": "x"}); err != nil {
		t.Fatal(err)
	}
	if len(usage.calls) != 1 || usage.calls[0].entityID != unattributedEntity {
		t.Fatalf("usage = %+v, want one row under %s", usage.calls, unattributedEntity)
	}
}

func TestSubprocessUsageFailedLLMCallRecordsNothing(t *testing.T) {
	usage := &usageSpy{}
	orch := deadlineTestOrch(&usageLLM{delay: time.Second}, OrchestratorOpts{
		UsageRecorder: usage,
		Subprocess:    SubprocessConfig{DefaultTimeout: 20 * time.Millisecond},
	})
	ctx := profile.WithProfile(context.Background(), &profile.Profile{EntityID: "ent1"})

	if _, err := orch.RunAction(ctx, "_subprocess", "run", map[string]string{"task": "x"}); err == nil {
		t.Fatal("expected subprocess timeout")
	}
	if len(usage.calls) != 0 {
		t.Errorf("usage = %+v, want none", usage.calls)
	}
}

func TestMaxResponseBytesOptionApplies(t *testing.T) {
	orch := deadlineTestOrch(&usageLLM{}, OrchestratorOpts{MaxResponseBytes: 50})
	_, _, err := orch.RunActionResult(context.Background(), "_subprocess", "parallel", parallelTasksArg(3))
	if err == nil || !strings.Contains(err.Error(), "max_response_bytes") {
		t.Fatalf("err = %v, want structured content over max_response_bytes", err)
	}
}

func TestMaxResponseBytesLargeEnoughKeepsStructuredJSON(t *testing.T) {
	orch := deadlineTestOrch(&usageLLM{}, OrchestratorOpts{MaxResponseBytes: 1 << 20})
	_, structured, err := orch.RunActionResult(context.Background(), "_subprocess", "parallel", parallelTasksArg(16))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid([]byte(structured)) {
		t.Errorf("structured content is not valid JSON: %q", structured)
	}
}
