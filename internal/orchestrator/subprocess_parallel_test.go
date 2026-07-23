package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/provider"
	"github.com/opentalon/opentalon/internal/state"
)

// taskEchoLLM is a concurrency-safe fake: it answers each sub-agent from that
// sub-agent's own task text (the last user message), so parallel output is
// deterministic regardless of goroutine scheduling. A task containing "boom"
// fails, to exercise per-task error isolation.
type taskEchoLLM struct{}

func (taskEchoLLM) Complete(_ context.Context, req *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	task := lastUserContentForTest(req)
	if strings.Contains(task, "boom") {
		return nil, fmt.Errorf("simulated failure")
	}
	return &provider.CompletionResponse{Content: "answer: " + task}, nil
}

func lastUserContentForTest(req *provider.CompletionRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == provider.RoleUser {
			return req.Messages[i].Content
		}
	}
	return ""
}

func parallelOrch(llm LLMClient, maxParallel int) *Orchestrator {
	return NewWithRules(llm, DefaultParser, NewToolRegistry(), state.NewMemoryStore(""), state.NewSessionStore(""),
		OrchestratorOpts{Subprocess: SubprocessConfig{
			Enabled: true, MaxDepth: 2, MaxIterations: 5, DefaultTimeout: 30 * time.Second, MaxParallel: maxParallel,
		}})
}

func parallelCall(tasksJSON string) ToolCall {
	return ToolCall{ID: "par-1", Plugin: "_subprocess", Action: "parallel", Args: map[string]string{"tasks": tasksJSON}}
}

func TestParseParallelRequest(t *testing.T) {
	tests := []struct {
		name    string
		tasks   string
		wantErr bool
		wantLen int
	}{
		{"missing", "", true, 0},
		{"invalid json", `{not an array}`, true, 0},
		{"empty array", `[]`, true, 0},
		{"entry missing task", `[{"task":"ok"},{"tools":"a"}]`, true, 0},
		{"over cap", `[` + strings.Repeat(`{"task":"x"},`, maxParallelTasks) + `{"task":"x"}]`, true, 0},
		{"valid", `[{"task":"a"},{"task":"b","tools":"search__query, math__calc","max_iterations":3}]`, false, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqs, err := parseParallelRequest(map[string]string{"tasks": tt.tasks})
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(reqs) != tt.wantLen {
				t.Fatalf("got %d reqs, want %d", len(reqs), tt.wantLen)
			}
			if tt.name == "valid" {
				if reqs[1].Task != "b" || len(reqs[1].AllowedTools) != 2 || reqs[1].MaxIterations != 3 {
					t.Errorf("second req parsed wrong: %+v", reqs[1])
				}
			}
		})
	}
}

func TestSubprocessParallel_RunsAllInOrder(t *testing.T) {
	orch := parallelOrch(taskEchoLLM{}, 4)
	exec := &subprocessExecutor{orch: orch}

	tr := exec.Execute(context.Background(), parallelCall(`[{"task":"alpha"},{"task":"bravo"},{"task":"charlie"}]`))
	if tr.Error != "" {
		t.Fatalf("unexpected error: %s", tr.Error)
	}

	// Deterministic in-task-order joining.
	i1 := strings.Index(tr.Content, "## Task 1: alpha")
	i2 := strings.Index(tr.Content, "## Task 2: bravo")
	i3 := strings.Index(tr.Content, "## Task 3: charlie")
	if i1 < 0 || i2 < 0 || i3 < 0 {
		t.Fatalf("missing task section(s):\n%s", tr.Content)
	}
	if !(i1 < i2 && i2 < i3) {
		t.Errorf("sections out of order: %d,%d,%d", i1, i2, i3)
	}
	for _, want := range []string{"answer: alpha", "answer: bravo", "answer: charlie"} {
		if !strings.Contains(tr.Content, want) {
			t.Errorf("content missing %q:\n%s", want, tr.Content)
		}
	}

	var structured []parallelTaskResult
	if err := json.Unmarshal([]byte(tr.StructuredContent), &structured); err != nil {
		t.Fatalf("structured content not valid JSON: %v", err)
	}
	if len(structured) != 3 || structured[0].Task != "alpha" || structured[0].Response != "answer: alpha" {
		t.Errorf("structured payload wrong: %+v", structured)
	}
}

func TestSubprocessParallel_ErrorIsolation(t *testing.T) {
	orch := parallelOrch(taskEchoLLM{}, 4)
	exec := &subprocessExecutor{orch: orch}

	tr := exec.Execute(context.Background(), parallelCall(`[{"task":"good-one"},{"task":"boom-task"},{"task":"good-two"}]`))
	if tr.Error != "" {
		t.Fatalf("batch should not fail wholesale: %s", tr.Error)
	}
	if !strings.Contains(tr.Content, "answer: good-one") || !strings.Contains(tr.Content, "answer: good-two") {
		t.Errorf("healthy tasks should still complete:\n%s", tr.Content)
	}
	if !strings.Contains(tr.Content, "## Task 2: boom-task") || !strings.Contains(tr.Content, "error:") {
		t.Errorf("failed task should surface an error section:\n%s", tr.Content)
	}
}

func TestSubprocessParallel_DepthLimit(t *testing.T) {
	orch := parallelOrch(taskEchoLLM{}, 4)
	exec := &subprocessExecutor{orch: orch}

	// Already at the depth cap → the parallel call spawns nothing.
	ctx := withSubprocessDepth(context.Background(), 2)
	tr := exec.Execute(ctx, parallelCall(`[{"task":"x"},{"task":"y"}]`))
	if tr.Error == "" || !strings.Contains(tr.Error, "depth limit") {
		t.Errorf("expected depth limit error, got err=%q content=%q", tr.Error, tr.Content)
	}
}

// barrierLLM blocks every call until release is closed, tracking the maximum
// number of simultaneous in-flight calls. Used to prove the concurrency cap.
type barrierLLM struct {
	inFlight int32
	maxSeen  int32
	release  chan struct{}
}

func (b *barrierLLM) Complete(_ context.Context, _ *provider.CompletionRequest) (*provider.CompletionResponse, error) {
	n := atomic.AddInt32(&b.inFlight, 1)
	for {
		old := atomic.LoadInt32(&b.maxSeen)
		if n <= old || atomic.CompareAndSwapInt32(&b.maxSeen, old, n) {
			break
		}
	}
	<-b.release
	atomic.AddInt32(&b.inFlight, -1)
	return &provider.CompletionResponse{Content: "done"}, nil
}

func TestSubprocessParallel_RespectsConcurrencyCap(t *testing.T) {
	const cap = 2
	llm := &barrierLLM{release: make(chan struct{})}
	orch := parallelOrch(llm, cap)
	exec := &subprocessExecutor{orch: orch}

	done := make(chan ToolResult, 1)
	go func() {
		done <- exec.Execute(context.Background(), parallelCall(`[{"task":"a"},{"task":"b"},{"task":"c"},{"task":"d"}]`))
	}()

	// Wait until the cap is saturated (exactly `cap` calls in flight).
	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(&llm.inFlight) < cap {
		if time.Now().After(deadline) {
			close(llm.release)
			t.Fatalf("only %d calls in flight; cap never saturated", atomic.LoadInt32(&llm.inFlight))
		}
	}
	close(llm.release)
	<-done

	if got := atomic.LoadInt32(&llm.maxSeen); got != cap {
		t.Errorf("max concurrent sub-agents = %d, want exactly %d (cap must be reached but never exceeded)", got, cap)
	}
}
