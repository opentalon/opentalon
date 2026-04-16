package scheduler

import (
	"context"
	"strings"
	"testing"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/orchestrator"
	"github.com/opentalon/opentalon/internal/profile"
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

// testCtx returns a context with an actor ID in the shape the handler
// expects when no profile system is configured: "channel:sender", plus a
// synthetic conversation id so scheduler jobs created from this context
// carry enough state to be notifiable.
func testCtx(sender string) context.Context {
	ctx := actor.WithActor(context.Background(), "test-channel:"+sender)
	ctx = actor.WithConversationID(ctx, "test-conversation")
	return ctx
}

func TestToolCapability(t *testing.T) {
	tool := newTestTool(t)
	cap := tool.Capability()

	if cap.Name != ToolName {
		t.Errorf("name = %q, want %q", cap.Name, ToolName)
	}
	if len(cap.Actions) != 7 {
		t.Errorf("expected 7 actions, got %d", len(cap.Actions))
	}

	names := make(map[string]bool)
	for _, a := range cap.Actions {
		names[a.Name] = true
	}
	expected := []string{"create_job", "list_jobs", "delete_job", "pause_job", "resume_job", "update_job", "remind_me"}
	for _, n := range expected {
		if !names[n] {
			t.Errorf("missing action %q", n)
		}
	}
}

func TestToolCreateAndListJobs(t *testing.T) {
	tool := newTestTool(t)
	ctx := testCtx("diana")

	result := tool.Execute(ctx, orchestrator.ToolCall{
		ID:     "1",
		Plugin: ToolName,
		Action: "create_job",
		Args: map[string]string{
			"name":           "test-job",
			"interval":       "1h",
			"action":         "scan.run",
			"notify_channel": "slack",
		},
	})
	if result.Error != "" {
		t.Fatalf("create_job error: %s", result.Error)
	}
	if !strings.Contains(result.Content, "test-job") {
		t.Errorf("content = %q", result.Content)
	}

	listResult := tool.Execute(ctx, orchestrator.ToolCall{
		ID: "2", Plugin: ToolName, Action: "list_jobs",
		Args: map[string]string{"scope": "all"},
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

	tool.Execute(context.Background(), orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "create_job",
		Args: map[string]string{"name": "dup", "interval": "1h", "action": "a.b", "user_id": "diana"},
	})

	result := tool.Execute(context.Background(), orchestrator.ToolCall{
		ID: "2", Plugin: ToolName, Action: "create_job",
		Args: map[string]string{"name": "dup", "interval": "1h", "action": "a.b", "user_id": "diana"},
	})
	if result.Error == "" {
		t.Error("expected error for duplicate job")
	}
}

func TestToolCreateMissingFields(t *testing.T) {
	tool := newTestTool(t)

	result := tool.Execute(context.Background(), orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "create_job",
		Args: map[string]string{"name": "x"},
	})
	if result.Error == "" {
		t.Error("expected error for missing fields")
	}
}

func TestToolCreateWithArgs(t *testing.T) {
	tool := newTestTool(t)

	result := tool.Execute(testCtx("diana"), orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "create_job",
		Args: map[string]string{
			"name":     "args-job",
			"interval": "1h",
			"action":   "a.b",
			"args":     `{"key":"val"}`,
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

func TestToolCreateCapturesConversationID(t *testing.T) {
	tool := newTestTool(t)
	ctx := actor.WithActor(context.Background(), "telegram:sender-42")
	ctx = actor.WithConversationID(ctx, "chat-999")

	result := tool.Execute(ctx, orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "create_job",
		Args: map[string]string{"name": "conv-capture", "interval": "1h", "action": "a.b"},
	})
	if result.Error != "" {
		t.Fatalf("create_job error: %s", result.Error)
	}

	j, ok := tool.sched.GetJob("conv-capture")
	if !ok {
		t.Fatal("job not found")
	}
	if j.NotifyChannel != "telegram" {
		t.Errorf("notify_channel = %q, want telegram", j.NotifyChannel)
	}
	if j.NotifyConversationID != "chat-999" {
		t.Errorf("notify_conversation_id = %q, want chat-999", j.NotifyConversationID)
	}
}

// Regression: Haiku-class models routinely pass `message` as a top-level arg
// to create_job instead of wrapping it in `args`. The shortcut must map that
// to args={"message": ...} so reminder.say works on first fire.
func TestToolCreateJobMessageShortcut(t *testing.T) {
	tool := newTestTool(t)

	result := tool.Execute(testCtx("diana"), orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "create_job",
		Args: map[string]string{
			"name":     "lenin_quote_spam",
			"interval": "2m",
			"action":   "reminder.say",
			"message":  "There are decades where nothing happens; and there are weeks where decades happen. - Lenin",
		},
	})
	if result.Error != "" {
		t.Fatalf("create_job error: %s", result.Error)
	}

	j, ok := tool.sched.GetJob("lenin_quote_spam")
	if !ok {
		t.Fatal("job not found")
	}
	if got := j.Args["message"]; !strings.HasPrefix(got, "There are decades") {
		t.Errorf("args[message] = %q, want the Lenin quote", got)
	}
}

// Passing both 'message' and 'args' is ambiguous — error rather than guess.
func TestToolCreateJobMessageAndArgsConflict(t *testing.T) {
	tool := newTestTool(t)

	result := tool.Execute(testCtx("diana"), orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "create_job",
		Args: map[string]string{
			"name":     "conflict",
			"interval": "1h",
			"action":   "reminder.say",
			"message":  "hi",
			"args":     `{"message":"also hi"}`,
		},
	})
	if result.Error == "" {
		t.Fatal("expected mutual-exclusion error")
	}
	if !strings.Contains(result.Error, "mutually exclusive") {
		t.Errorf("error = %q, want mention of mutual exclusion", result.Error)
	}
}

// An empty 'message' must not hide a bad 'args' — parseArgsField still runs.
func TestToolCreateJobEmptyMessageFallsThroughToArgs(t *testing.T) {
	tool := newTestTool(t)

	result := tool.Execute(testCtx("diana"), orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "create_job",
		Args: map[string]string{
			"name":     "fall",
			"interval": "1h",
			"action":   "a.b",
			"message":  "",
			"args":     `{"key":"val"}`,
		},
	})
	if result.Error != "" {
		t.Fatalf("create_job error: %s", result.Error)
	}
	j, _ := tool.sched.GetJob("fall")
	if j.Args["key"] != "val" {
		t.Errorf("args = %v, want key=val", j.Args)
	}
}

func TestToolCreateBadArgsJSON(t *testing.T) {
	tool := newTestTool(t)

	result := tool.Execute(context.Background(), orchestrator.ToolCall{
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
	ctx := testCtx("diana")

	tool.Execute(ctx, orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "create_job",
		Args: map[string]string{"name": "del", "interval": "1h", "action": "a.b"},
	})

	result := tool.Execute(ctx, orchestrator.ToolCall{
		ID: "2", Plugin: ToolName, Action: "delete_job",
		Args: map[string]string{"name": "del"},
	})
	if result.Error != "" {
		t.Errorf("delete error: %s", result.Error)
	}

	result = tool.Execute(ctx, orchestrator.ToolCall{
		ID: "3", Plugin: ToolName, Action: "delete_job",
		Args: map[string]string{"name": "del"},
	})
	if result.Error == "" {
		t.Error("expected error deleting nonexistent")
	}
}

func TestToolPauseResumeJob(t *testing.T) {
	tool := newTestTool(t)
	ctx := testCtx("diana")

	tool.Execute(ctx, orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "create_job",
		Args: map[string]string{"name": "pr", "interval": "1h", "action": "a.b"},
	})

	result := tool.Execute(ctx, orchestrator.ToolCall{
		ID: "2", Plugin: ToolName, Action: "pause_job",
		Args: map[string]string{"name": "pr"},
	})
	if result.Error != "" {
		t.Errorf("pause error: %s", result.Error)
	}

	result = tool.Execute(ctx, orchestrator.ToolCall{
		ID: "3", Plugin: ToolName, Action: "resume_job",
		Args: map[string]string{"name": "pr"},
	})
	if result.Error != "" {
		t.Errorf("resume error: %s", result.Error)
	}
}

func TestToolUpdateJob(t *testing.T) {
	tool := newTestTool(t)
	ctx := testCtx("diana")

	tool.Execute(ctx, orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "create_job",
		Args: map[string]string{"name": "upd", "interval": "1h", "action": "a.b"},
	})

	result := tool.Execute(ctx, orchestrator.ToolCall{
		ID: "2", Plugin: ToolName, Action: "update_job",
		Args: map[string]string{"name": "upd", "interval": "30m"},
	})
	if result.Error != "" {
		t.Errorf("update error: %s", result.Error)
	}

	j, _ := tool.sched.GetJob("upd")
	if j.Interval != "30m" {
		t.Errorf("interval = %q, want 30m", j.Interval)
	}
}

// Regression: when update_job changes notify_channel, the stored
// conversation id belongs to the old channel and is meaningless on the new
// one (e.g. a Slack channel id sent to Telegram). The tool layer must
// refresh it from the caller's current context, matching create_job.
func TestToolUpdateJobRefreshesConversationIDOnChannelChange(t *testing.T) {
	tool := newTestTool(t)

	// Create the job from Slack, conversation C-old.
	createCtx := actor.WithActor(context.Background(), "slack:diana")
	createCtx = actor.WithConversationID(createCtx, "C-old")
	res := tool.Execute(createCtx, orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "create_job",
		Args: map[string]string{"name": "cross", "interval": "1h", "action": "a.b"},
	})
	if res.Error != "" {
		t.Fatalf("create_job: %s", res.Error)
	}

	// Update from Telegram, conversation T-new — switch notify_channel.
	updateCtx := actor.WithActor(context.Background(), "telegram:diana")
	updateCtx = actor.WithConversationID(updateCtx, "T-new")
	res = tool.Execute(updateCtx, orchestrator.ToolCall{
		ID: "2", Plugin: ToolName, Action: "update_job",
		Args: map[string]string{"name": "cross", "notify_channel": "telegram"},
	})
	if res.Error != "" {
		t.Fatalf("update_job: %s", res.Error)
	}

	j, _ := tool.sched.GetJob("cross")
	if j.NotifyChannel != "telegram" {
		t.Errorf("notify_channel = %q, want telegram", j.NotifyChannel)
	}
	if j.NotifyConversationID != "T-new" {
		t.Errorf("notify_conversation_id = %q, want T-new (refreshed from new context)", j.NotifyConversationID)
	}
}

// Counterpart to the above: an update that leaves notify_channel alone must
// NOT touch the stored conversation id, even if the caller is in a different
// conversation at update time.
func TestToolUpdateJobPreservesConversationIDWhenChannelUnchanged(t *testing.T) {
	tool := newTestTool(t)

	createCtx := actor.WithActor(context.Background(), "slack:diana")
	createCtx = actor.WithConversationID(createCtx, "C-original")
	res := tool.Execute(createCtx, orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "create_job",
		Args: map[string]string{"name": "keep", "interval": "1h", "action": "a.b"},
	})
	if res.Error != "" {
		t.Fatalf("create_job: %s", res.Error)
	}

	updateCtx := actor.WithActor(context.Background(), "slack:diana")
	updateCtx = actor.WithConversationID(updateCtx, "C-elsewhere")
	res = tool.Execute(updateCtx, orchestrator.ToolCall{
		ID: "2", Plugin: ToolName, Action: "update_job",
		Args: map[string]string{"name": "keep", "interval": "30m"},
	})
	if res.Error != "" {
		t.Fatalf("update_job: %s", res.Error)
	}

	j, _ := tool.sched.GetJob("keep")
	if j.NotifyConversationID != "C-original" {
		t.Errorf("notify_conversation_id = %q, want C-original (should not change)", j.NotifyConversationID)
	}
}

// Covers the profile branch of resolveCaller — the actor-fallback branch is
// covered by TestToolCreateCapturesConversationID.
func TestResolveCallerProfileBranchReadsConversationID(t *testing.T) {
	ctx := profile.WithProfile(context.Background(), &profile.Profile{
		EntityID:  "ent-7",
		Group:     "team-a",
		ChannelID: "slack",
	})
	ctx = actor.WithConversationID(ctx, "C-from-profile")

	caller, err := resolveCaller(ctx)
	if err != nil {
		t.Fatalf("resolveCaller: %v", err)
	}
	if caller.entityID != "ent-7" {
		t.Errorf("entityID = %q, want ent-7", caller.entityID)
	}
	if caller.channelID != "slack" {
		t.Errorf("channelID = %q, want slack", caller.channelID)
	}
	if caller.conversationID != "C-from-profile" {
		t.Errorf("conversationID = %q, want C-from-profile", caller.conversationID)
	}
	if caller.userID != "ent-7" {
		t.Errorf("userID = %q, want ent-7 (profile uses EntityID)", caller.userID)
	}
}

func TestToolUpdateNoFields(t *testing.T) {
	tool := newTestTool(t)

	tool.Execute(context.Background(), orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "create_job",
		Args: map[string]string{"name": "nf", "interval": "1h", "action": "a.b", "user_id": "diana"},
	})

	result := tool.Execute(context.Background(), orchestrator.ToolCall{
		ID: "2", Plugin: ToolName, Action: "update_job",
		Args: map[string]string{"name": "nf", "user_id": "diana"},
	})
	if result.Error == "" {
		t.Error("expected error when no update fields provided")
	}
}

func TestToolUnknownAction(t *testing.T) {
	tool := newTestTool(t)
	result := tool.Execute(context.Background(), orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "fly_to_moon",
	})
	if result.Error == "" {
		t.Error("expected error for unknown action")
	}
}

func TestToolListEmpty(t *testing.T) {
	tool := newTestTool(t)
	result := tool.Execute(context.Background(), orchestrator.ToolCall{
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
		result := tool.Execute(context.Background(), orchestrator.ToolCall{
			ID: "1", Plugin: ToolName, Action: action,
			Args: map[string]string{},
		})
		if result.Error == "" {
			t.Errorf("%s should error with missing name", action)
		}
	}
}

func TestParseArgsField(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    map[string]string
		wantErr bool
	}{
		{"empty", "", nil, false},
		{"whitespace", "   \n\t  ", nil, false},
		{"valid object", `{"issue_id":"XYZ"}`, map[string]string{"issue_id": "XYZ"}, false},
		{"empty object", `{}`, map[string]string{}, false},
		{"go map format", `map[issue_id:XYZ]`, nil, true},
		{"truncated json", `{"issue_id":`, nil, true},
		{"not an object", `"bare string"`, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseArgsField(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if len(got) != len(tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("got[%q] = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}
