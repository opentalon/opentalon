package scheduler

import (
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/orchestrator"
)

func newTestTool(t *testing.T) *SchedulerTool {
	t.Helper()
	runner := &fakeRunner{}
	sched := New(runner, nil, "")
	if err := sched.Start(nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sched.Stop)
	return NewSchedulerTool(sched)
}

func TestToolCapability(t *testing.T) {
	tool := newTestTool(t)
	cap := tool.Capability()

	if cap.Name != ToolName {
		t.Errorf("name = %q, want %q", cap.Name, ToolName)
	}
	if len(cap.Actions) != 6 {
		t.Errorf("expected 6 actions, got %d", len(cap.Actions))
	}

	names := make(map[string]bool)
	for _, a := range cap.Actions {
		names[a.Name] = true
	}
	expected := []string{"create_job", "list_jobs", "delete_job", "pause_job", "resume_job", "update_job"}
	for _, n := range expected {
		if !names[n] {
			t.Errorf("missing action %q", n)
		}
	}
}

func TestToolCreateAndListJobs(t *testing.T) {
	tool := newTestTool(t)

	result := tool.Execute(orchestrator.ToolCall{
		ID:     "1",
		Plugin: ToolName,
		Action: "create_job",
		Args: map[string]string{
			"name":           "test-job",
			"interval":       "1h",
			"action":         "scan.run",
			"notify_channel": "slack",
			"user_id":        "diana",
		},
	})
	if result.Error != "" {
		t.Fatalf("create_job error: %s", result.Error)
	}
	if !strings.Contains(result.Content, "test-job") {
		t.Errorf("content = %q", result.Content)
	}

	listResult := tool.Execute(orchestrator.ToolCall{
		ID: "2", Plugin: ToolName, Action: "list_jobs",
	})
	if listResult.Error != "" {
		t.Fatalf("list_jobs error: %s", listResult.Error)
	}
	if !strings.Contains(listResult.Content, "test-job") {
		t.Errorf("list should contain test-job: %s", listResult.Content)
	}
}

func TestToolCreateDuplicate(t *testing.T) {
	tool := newTestTool(t)

	tool.Execute(orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "create_job",
		Args: map[string]string{"name": "dup", "interval": "1h", "action": "a.b", "user_id": "diana"},
	})

	result := tool.Execute(orchestrator.ToolCall{
		ID: "2", Plugin: ToolName, Action: "create_job",
		Args: map[string]string{"name": "dup", "interval": "1h", "action": "a.b", "user_id": "diana"},
	})
	if result.Error == "" {
		t.Error("expected error for duplicate job")
	}
}

func TestToolCreateMissingFields(t *testing.T) {
	tool := newTestTool(t)

	result := tool.Execute(orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "create_job",
		Args: map[string]string{"name": "x"},
	})
	if result.Error == "" {
		t.Error("expected error for missing fields")
	}
}

func TestToolCreateWithArgs(t *testing.T) {
	tool := newTestTool(t)

	result := tool.Execute(orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "create_job",
		Args: map[string]string{
			"name":     "args-job",
			"interval": "1h",
			"action":   "a.b",
			"args":     `{"key":"val"}`,
			"user_id":  "diana",
		},
	})
	if result.Error != "" {
		t.Fatalf("error: %s", result.Error)
	}

	j, ok := tool.sched.GetJob("args-job")
	if !ok {
		t.Fatal("job not found")
	}
	if j.Args["key"] != "val" {
		t.Errorf("args = %v", j.Args)
	}
}

func TestToolCreateBadArgsJSON(t *testing.T) {
	tool := newTestTool(t)

	result := tool.Execute(orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "create_job",
		Args: map[string]string{
			"name": "bad", "interval": "1h", "action": "a.b",
			"args": "{invalid",
		},
	})
	if result.Error == "" {
		t.Error("expected error for bad JSON")
	}
}

func TestToolDeleteJob(t *testing.T) {
	tool := newTestTool(t)

	tool.Execute(orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "create_job",
		Args: map[string]string{"name": "del", "interval": "1h", "action": "a.b", "user_id": "diana"},
	})

	result := tool.Execute(orchestrator.ToolCall{
		ID: "2", Plugin: ToolName, Action: "delete_job",
		Args: map[string]string{"name": "del", "user_id": "diana"},
	})
	if result.Error != "" {
		t.Errorf("delete error: %s", result.Error)
	}

	result = tool.Execute(orchestrator.ToolCall{
		ID: "3", Plugin: ToolName, Action: "delete_job",
		Args: map[string]string{"name": "del", "user_id": "diana"},
	})
	if result.Error == "" {
		t.Error("expected error deleting nonexistent")
	}
}

func TestToolPauseResumeJob(t *testing.T) {
	tool := newTestTool(t)

	tool.Execute(orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "create_job",
		Args: map[string]string{"name": "pr", "interval": "1h", "action": "a.b", "user_id": "diana"},
	})

	result := tool.Execute(orchestrator.ToolCall{
		ID: "2", Plugin: ToolName, Action: "pause_job",
		Args: map[string]string{"name": "pr"},
	})
	if result.Error != "" {
		t.Errorf("pause error: %s", result.Error)
	}

	result = tool.Execute(orchestrator.ToolCall{
		ID: "3", Plugin: ToolName, Action: "resume_job",
		Args: map[string]string{"name": "pr"},
	})
	if result.Error != "" {
		t.Errorf("resume error: %s", result.Error)
	}
}

func TestToolUpdateJob(t *testing.T) {
	tool := newTestTool(t)

	tool.Execute(orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "create_job",
		Args: map[string]string{"name": "upd", "interval": "1h", "action": "a.b", "user_id": "diana"},
	})

	result := tool.Execute(orchestrator.ToolCall{
		ID: "2", Plugin: ToolName, Action: "update_job",
		Args: map[string]string{"name": "upd", "interval": "30m", "user_id": "diana"},
	})
	if result.Error != "" {
		t.Errorf("update error: %s", result.Error)
	}

	j, _ := tool.sched.GetJob("upd")
	if j.Interval != "30m" {
		t.Errorf("interval = %q, want 30m", j.Interval)
	}
}

func TestToolUpdateNoFields(t *testing.T) {
	tool := newTestTool(t)

	tool.Execute(orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "create_job",
		Args: map[string]string{"name": "nf", "interval": "1h", "action": "a.b", "user_id": "diana"},
	})

	result := tool.Execute(orchestrator.ToolCall{
		ID: "2", Plugin: ToolName, Action: "update_job",
		Args: map[string]string{"name": "nf", "user_id": "diana"},
	})
	if result.Error == "" {
		t.Error("expected error when no update fields provided")
	}
}

func TestToolUnknownAction(t *testing.T) {
	tool := newTestTool(t)
	result := tool.Execute(orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "fly_to_moon",
	})
	if result.Error == "" {
		t.Error("expected error for unknown action")
	}
}

func TestToolListEmpty(t *testing.T) {
	tool := newTestTool(t)
	result := tool.Execute(orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "list_jobs",
	})
	if result.Error != "" {
		t.Errorf("error: %s", result.Error)
	}
	if !strings.Contains(result.Content, "No scheduled jobs") {
		t.Errorf("expected empty message, got %q", result.Content)
	}
}

func TestToolMissingName(t *testing.T) {
	tool := newTestTool(t)

	for _, action := range []string{"delete_job", "pause_job", "resume_job", "update_job"} {
		result := tool.Execute(orchestrator.ToolCall{
			ID: "1", Plugin: ToolName, Action: action,
			Args: map[string]string{},
		})
		if result.Error == "" {
			t.Errorf("%s should error with missing name", action)
		}
	}
}
