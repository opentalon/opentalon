package scheduler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opentalon/opentalon/internal/orchestrator"
)

type fakeRunner struct {
	mu      sync.Mutex
	calls   []runCall
	results map[string]string
	err     error
}

type runCall struct {
	Plugin string
	Action string
	Args   map[string]string
}

func (f *fakeRunner) RunAction(_ context.Context, plugin, action string, args map[string]string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, runCall{Plugin: plugin, Action: action, Args: args})
	if f.err != nil {
		return "", f.err
	}
	key := plugin + "." + action
	if r, ok := f.results[key]; ok {
		return r, nil
	}
	return "ok", nil
}

func (f *fakeRunner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type fakeNotifier struct {
	mu       sync.Mutex
	messages []notifyCall
}

type notifyCall struct {
	ChannelID string
	Content   string
}

func (n *fakeNotifier) Notify(_ context.Context, channelID, content string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.messages = append(n.messages, notifyCall{ChannelID: channelID, Content: content})
	return nil
}

func (n *fakeNotifier) messageCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.messages)
}

func TestJobParseDuration(t *testing.T) {
	j := Job{Interval: "30m"}
	d, err := j.parseDuration()
	if err != nil {
		t.Fatal(err)
	}
	if d != 30*time.Minute {
		t.Errorf("expected 30m, got %v", d)
	}

	j2 := Job{Interval: "bad"}
	_, err = j2.parseDuration()
	if err == nil {
		t.Error("expected error for bad interval")
	}
}

func TestJobParseAction(t *testing.T) {
	tests := []struct {
		action string
		plugin string
		act    string
		err    bool
	}{
		{"ipossum.check_violations", "ipossum", "check_violations", false},
		{"reports.generate", "reports", "generate", false},
		{"invalid", "", "", true},
		{".action", "", "", true},
		{"plugin.", "", "", true},
		{"", "", "", true},
	}

	for _, tc := range tests {
		j := Job{Action: tc.action}
		p, a, err := j.parseAction()
		if tc.err {
			if err == nil {
				t.Errorf("parseAction(%q): expected error", tc.action)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseAction(%q): %v", tc.action, err)
			continue
		}
		if p != tc.plugin || a != tc.act {
			t.Errorf("parseAction(%q) = (%q, %q), want (%q, %q)", tc.action, p, a, tc.plugin, tc.act)
		}
	}
}

func TestSchedulerTickExecution(t *testing.T) {
	runner := &fakeRunner{}
	s := New(runner, nil, "")

	err := s.Start([]Job{
		{Name: "fast", Interval: "50ms", Action: "test.ping"},
	})
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(180 * time.Millisecond)
	s.Stop()

	count := runner.callCount()
	if count < 2 {
		t.Errorf("expected at least 2 ticks in 180ms, got %d", count)
	}
}

func TestSchedulerNotification(t *testing.T) {
	runner := &fakeRunner{results: map[string]string{"test.ping": "pong"}}
	notifier := &fakeNotifier{}
	s := New(runner, notifier, "")

	err := s.Start([]Job{
		{Name: "notify-test", Interval: "50ms", Action: "test.ping", NotifyChannel: "slack"},
	})
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(120 * time.Millisecond)
	s.Stop()

	if notifier.messageCount() < 1 {
		t.Error("expected at least 1 notification")
	}

	notifier.mu.Lock()
	msg := notifier.messages[0]
	notifier.mu.Unlock()

	if msg.ChannelID != "slack" {
		t.Errorf("channel = %q, want slack", msg.ChannelID)
	}
}

func TestSchedulerDynamicCRUD(t *testing.T) {
	runner := &fakeRunner{}
	dir := t.TempDir()
	s := New(runner, nil, dir)
	if err := s.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	if err := s.AddJob(Job{Name: "dyn1", Interval: "1h", Action: "a.b"}, "user1"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddJob(Job{Name: "dyn2", Interval: "2h", Action: "c.d"}, "user1"); err != nil {
		t.Fatal(err)
	}

	jobs := s.ListJobs()
	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(jobs))
	}

	j, ok := s.GetJob("dyn1")
	if !ok {
		t.Fatal("dyn1 not found")
	}
	if j.Interval != "1h" {
		t.Errorf("interval = %q", j.Interval)
	}
	if j.Source != "dynamic" {
		t.Errorf("source = %q, want dynamic", j.Source)
	}
	if j.CreatedBy != "user1" {
		t.Errorf("created_by = %q, want user1", j.CreatedBy)
	}

	if err := s.RemoveJob("dyn1", "user1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.GetJob("dyn1"); ok {
		t.Error("dyn1 should have been removed")
	}

	err := s.AddJob(Job{Name: "dyn2", Interval: "1h", Action: "a.b"}, "user1")
	if err == nil {
		t.Error("expected duplicate error")
	}
}

func TestSchedulerPauseResume(t *testing.T) {
	runner := &fakeRunner{}
	s := New(runner, nil, "")
	if err := s.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	if err := s.AddJob(Job{Name: "pr1", Interval: "50ms", Action: "test.act"}, "user1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)

	before := runner.callCount()
	if before < 1 {
		t.Fatal("expected at least 1 call before pause")
	}

	if err := s.PauseJob("pr1"); err != nil {
		t.Fatal(err)
	}

	time.Sleep(120 * time.Millisecond)
	afterPause := runner.callCount()
	if afterPause != before {
		t.Errorf("expected no new calls after pause: before=%d after=%d", before, afterPause)
	}

	if err := s.ResumeJob("pr1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)

	afterResume := runner.callCount()
	if afterResume <= afterPause {
		t.Error("expected new calls after resume")
	}
}

func TestSchedulerPausedJobNotStarted(t *testing.T) {
	runner := &fakeRunner{}
	s := New(runner, nil, "")

	err := s.Start([]Job{
		{Name: "paused", Interval: "50ms", Action: "test.act", Paused: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(120 * time.Millisecond)
	s.Stop()

	if runner.callCount() != 0 {
		t.Errorf("paused job should not execute, got %d calls", runner.callCount())
	}
}

func TestSchedulerBadActionFormat(t *testing.T) {
	runner := &fakeRunner{}
	s := New(runner, nil, "")

	err := s.Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	err = s.AddJob(Job{Name: "bad", Interval: "1h", Action: "no-dot"}, "user1")
	if err == nil {
		t.Error("expected error for bad action format")
	}
}

func TestSchedulerBadInterval(t *testing.T) {
	runner := &fakeRunner{}
	s := New(runner, nil, "")

	err := s.Start(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	err = s.AddJob(Job{Name: "bad-int", Interval: "notaduration", Action: "a.b"}, "user1")
	if err == nil {
		t.Error("expected error for bad interval")
	}
}

func TestSchedulerEmptyName(t *testing.T) {
	runner := &fakeRunner{}
	s := New(runner, nil, "")
	if err := s.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	err := s.AddJob(Job{Name: "", Interval: "1h", Action: "a.b"}, "user1")
	if err == nil {
		t.Error("expected error for empty name")
	}
}

func TestSchedulerPersistence(t *testing.T) {
	dir := t.TempDir()
	runner := &fakeRunner{}

	s1 := New(runner, nil, dir)
	if err := s1.Start(nil); err != nil {
		t.Fatal(err)
	}
	if err := s1.AddJob(Job{Name: "persist1", Interval: "1h", Action: "p.a"}, "user1"); err != nil {
		t.Fatal(err)
	}
	if err := s1.AddJob(Job{Name: "persist2", Interval: "2h", Action: "q.b"}, "user1"); err != nil {
		t.Fatal(err)
	}
	s1.Stop()

	data, err := os.ReadFile(filepath.Join(dir, "scheduler", "jobs.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Error("persist file should not be empty")
	}

	s2 := New(runner, nil, dir)
	if err := s2.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer s2.Stop()

	jobs := s2.ListJobs()
	if len(jobs) != 2 {
		t.Errorf("expected 2 persisted jobs, got %d", len(jobs))
	}
}

func TestSchedulerStaticJobsNotPersisted(t *testing.T) {
	dir := t.TempDir()
	runner := &fakeRunner{}

	s := New(runner, nil, dir)
	if err := s.Start([]Job{
		{Name: "static1", Interval: "1h", Action: "a.b"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddJob(Job{Name: "dyn1", Interval: "1h", Action: "c.d"}, "user1"); err != nil {
		t.Fatal(err)
	}
	s.Stop()

	s2 := New(runner, nil, dir)
	if err := s2.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer s2.Stop()

	jobs := s2.ListJobs()
	if len(jobs) != 1 {
		t.Errorf("expected only 1 persisted dynamic job, got %d", len(jobs))
	}
	if jobs[0].Name != "dyn1" {
		t.Errorf("expected dyn1, got %s", jobs[0].Name)
	}
}

func TestSchedulerStopDrainsGoroutines(t *testing.T) {
	runner := &fakeRunner{}
	s := New(runner, nil, "")

	if err := s.Start([]Job{
		{Name: "drain1", Interval: "50ms", Action: "a.b"},
		{Name: "drain2", Interval: "50ms", Action: "c.d"},
	}); err != nil {
		t.Fatal(err)
	}

	time.Sleep(80 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		s.Stop()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() did not return within 2s — goroutine leak")
	}
}

func TestSchedulerUpdateJob(t *testing.T) {
	runner := &fakeRunner{}
	dir := t.TempDir()
	s := New(runner, nil, dir)
	if err := s.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	if err := s.AddJob(Job{Name: "upd1", Interval: "1h", Action: "a.b", NotifyChannel: "slack"}, "user1"); err != nil {
		t.Fatal(err)
	}

	newInterval := "30m"
	if err := s.UpdateJob("upd1", "user1", &newInterval, nil); err != nil {
		t.Fatal(err)
	}

	j, ok := s.GetJob("upd1")
	if !ok {
		t.Fatal("job not found after update")
	}
	if j.Interval != "30m" {
		t.Errorf("interval = %q, want 30m", j.Interval)
	}
	if j.NotifyChannel != "slack" {
		t.Error("notify_channel should be preserved")
	}

	newChan := "teams"
	if err := s.UpdateJob("upd1", "user1", nil, &newChan); err != nil {
		t.Fatal(err)
	}
	j, _ = s.GetJob("upd1")
	if j.NotifyChannel != "teams" {
		t.Errorf("notify_channel = %q, want teams", j.NotifyChannel)
	}
}

func TestSchedulerRunnerError(t *testing.T) {
	runner := &fakeRunner{err: fmt.Errorf("plugin crashed")}
	s := New(runner, nil, "")

	if err := s.Start([]Job{
		{Name: "err-test", Interval: "50ms", Action: "crash.now"},
	}); err != nil {
		t.Fatal(err)
	}

	time.Sleep(120 * time.Millisecond)
	s.Stop()

	if runner.callCount() < 1 {
		t.Error("runner should still be called even if it errors")
	}
}

func TestSchedulerResumeNonPaused(t *testing.T) {
	runner := &fakeRunner{}
	s := New(runner, nil, "")
	if err := s.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	if err := s.AddJob(Job{Name: "np1", Interval: "1h", Action: "a.b"}, "user1"); err != nil {
		t.Fatal(err)
	}

	err := s.ResumeJob("np1")
	if err == nil {
		t.Error("resuming a non-paused job should error")
	}
}

func TestSchedulerRemoveNonexistent(t *testing.T) {
	runner := &fakeRunner{}
	s := New(runner, nil, "")
	if err := s.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	if err := s.RemoveJob("ghost", "user1"); err == nil {
		t.Error("removing nonexistent job should error")
	}
}

// --- Config immutability tests ---

func TestSchedulerConfigJobCannotBeRemoved(t *testing.T) {
	runner := &fakeRunner{}
	s := New(runner, nil, "")
	if err := s.Start([]Job{
		{Name: "static1", Interval: "1h", Action: "a.b"},
	}); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	err := s.RemoveJob("static1", "admin")
	if err != ErrConfigProtected {
		t.Errorf("expected ErrConfigProtected, got %v", err)
	}

	_, ok := s.GetJob("static1")
	if !ok {
		t.Error("config job should still exist after failed removal")
	}
}

func TestSchedulerConfigJobCannotBeUpdated(t *testing.T) {
	runner := &fakeRunner{}
	s := New(runner, nil, "")
	if err := s.Start([]Job{
		{Name: "static1", Interval: "1h", Action: "a.b"},
	}); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	newInterval := "30m"
	err := s.UpdateJob("static1", "admin", &newInterval, nil)
	if err != ErrConfigProtected {
		t.Errorf("expected ErrConfigProtected, got %v", err)
	}

	j, _ := s.GetJob("static1")
	if j.Interval != "1h" {
		t.Errorf("config job interval should be unchanged, got %q", j.Interval)
	}
}

func TestSchedulerConfigJobCanBePaused(t *testing.T) {
	runner := &fakeRunner{}
	s := New(runner, nil, "")
	if err := s.Start([]Job{
		{Name: "static1", Interval: "50ms", Action: "a.b"},
	}); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	if err := s.PauseJob("static1"); err != nil {
		t.Fatalf("config jobs should be pausable: %v", err)
	}

	j, _ := s.GetJob("static1")
	if !j.Paused {
		t.Error("job should be paused")
	}

	if err := s.ResumeJob("static1"); err != nil {
		t.Fatalf("config jobs should be resumable: %v", err)
	}
}

// --- Approver enforcement tests ---

func TestSchedulerApproverRequired(t *testing.T) {
	runner := &fakeRunner{}
	s := NewWithPolicy(runner, nil, "", []string{"admin@co.com"}, 0)
	if err := s.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	err := s.AddJob(Job{Name: "j1", Interval: "1h", Action: "a.b"}, "random@co.com")
	if err != ErrNotAuthorized {
		t.Errorf("non-approver should be rejected, got %v", err)
	}

	err = s.AddJob(Job{Name: "j1", Interval: "1h", Action: "a.b"}, "admin@co.com")
	if err != nil {
		t.Fatalf("approver should be allowed: %v", err)
	}
}

func TestSchedulerApproverDeleteRequired(t *testing.T) {
	runner := &fakeRunner{}
	s := NewWithPolicy(runner, nil, "", []string{"admin@co.com"}, 0)
	if err := s.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	if err := s.AddJob(Job{Name: "j1", Interval: "1h", Action: "a.b"}, "admin@co.com"); err != nil {
		t.Fatal(err)
	}

	err := s.RemoveJob("j1", "random@co.com")
	if err != ErrNotAuthorized {
		t.Errorf("non-approver should not delete, got %v", err)
	}

	err = s.RemoveJob("j1", "admin@co.com")
	if err != nil {
		t.Fatalf("approver should delete: %v", err)
	}
}

func TestSchedulerApproverUpdateRequired(t *testing.T) {
	runner := &fakeRunner{}
	s := NewWithPolicy(runner, nil, "", []string{"admin@co.com"}, 0)
	if err := s.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	if err := s.AddJob(Job{Name: "j1", Interval: "1h", Action: "a.b"}, "admin@co.com"); err != nil {
		t.Fatal(err)
	}

	newInterval := "30m"
	err := s.UpdateJob("j1", "random@co.com", &newInterval, nil)
	if err != ErrNotAuthorized {
		t.Errorf("non-approver should not update, got %v", err)
	}

	err = s.UpdateJob("j1", "admin@co.com", &newInterval, nil)
	if err != nil {
		t.Fatalf("approver should update: %v", err)
	}
}

func TestSchedulerNoApproversAllowsEveryone(t *testing.T) {
	runner := &fakeRunner{}
	s := New(runner, nil, "")
	if err := s.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	err := s.AddJob(Job{Name: "j1", Interval: "1h", Action: "a.b"}, "anyone")
	if err != nil {
		t.Fatalf("with no approvers, anyone should be allowed: %v", err)
	}

	err = s.RemoveJob("j1", "anyone")
	if err != nil {
		t.Fatalf("with no approvers, anyone should delete: %v", err)
	}
}

func TestSchedulerMultipleApprovers(t *testing.T) {
	runner := &fakeRunner{}
	s := NewWithPolicy(runner, nil, "", []string{"admin@co.com", "ops@co.com"}, 0)
	if err := s.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	err := s.AddJob(Job{Name: "j1", Interval: "1h", Action: "a.b"}, "ops@co.com")
	if err != nil {
		t.Fatalf("second approver should work: %v", err)
	}
}

// --- Max jobs per user tests ---

func TestSchedulerMaxJobsPerUser(t *testing.T) {
	runner := &fakeRunner{}
	s := NewWithPolicy(runner, nil, "", nil, 2)
	if err := s.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	if err := s.AddJob(Job{Name: "j1", Interval: "1h", Action: "a.b"}, "diana"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddJob(Job{Name: "j2", Interval: "1h", Action: "a.b"}, "diana"); err != nil {
		t.Fatal(err)
	}

	err := s.AddJob(Job{Name: "j3", Interval: "1h", Action: "a.b"}, "diana")
	if err == nil {
		t.Error("expected error when exceeding max jobs per user")
	}
	if !strings.Contains(err.Error(), "job limit reached") {
		t.Errorf("expected limit error, got: %v", err)
	}

	err = s.AddJob(Job{Name: "j3", Interval: "1h", Action: "a.b"}, "bob")
	if err != nil {
		t.Fatalf("different user should not be limited: %v", err)
	}
}

func TestSchedulerMaxJobsZeroMeansUnlimited(t *testing.T) {
	runner := &fakeRunner{}
	s := NewWithPolicy(runner, nil, "", nil, 0)
	if err := s.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	for i := 0; i < 10; i++ {
		err := s.AddJob(Job{
			Name:     fmt.Sprintf("j%d", i),
			Interval: "1h",
			Action:   "a.b",
		}, "diana")
		if err != nil {
			t.Fatalf("unlimited should not fail at job %d: %v", i, err)
		}
	}
}

func TestSchedulerMaxJobsConfigJobsNotCounted(t *testing.T) {
	runner := &fakeRunner{}
	s := NewWithPolicy(runner, nil, "", nil, 1)
	if err := s.Start([]Job{
		{Name: "config1", Interval: "1h", Action: "a.b"},
		{Name: "config2", Interval: "1h", Action: "c.d"},
	}); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	err := s.AddJob(Job{Name: "dyn1", Interval: "1h", Action: "a.b"}, "diana")
	if err != nil {
		t.Fatalf("config jobs should not count toward user limit: %v", err)
	}
}

func TestSchedulerCreatedByTracked(t *testing.T) {
	runner := &fakeRunner{}
	s := New(runner, nil, "")
	if err := s.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	if err := s.AddJob(Job{Name: "owned", Interval: "1h", Action: "a.b"}, "diana@co.com"); err != nil {
		t.Fatal(err)
	}

	j, ok := s.GetJob("owned")
	if !ok {
		t.Fatal("job not found")
	}
	if j.CreatedBy != "diana@co.com" {
		t.Errorf("created_by = %q, want diana@co.com", j.CreatedBy)
	}
}

// --- Tool-level protection tests ---

func newTestToolWithPolicy(t *testing.T, approvers []string, maxPerUser int) *SchedulerTool {
	t.Helper()
	runner := &fakeRunner{}
	sched := NewWithPolicy(runner, nil, "", approvers, maxPerUser)
	if err := sched.Start(nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sched.Stop)
	return NewSchedulerTool(sched)
}

func newTestToolWithConfigJob(t *testing.T) *SchedulerTool {
	t.Helper()
	runner := &fakeRunner{}
	sched := New(runner, nil, "")
	if err := sched.Start([]Job{
		{Name: "config-job", Interval: "1h", Action: "a.b"},
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sched.Stop)
	return NewSchedulerTool(sched)
}

func TestToolDeleteConfigJobRejected(t *testing.T) {
	tool := newTestToolWithConfigJob(t)

	result := tool.Execute(orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "delete_job",
		Args: map[string]string{"name": "config-job", "user_id": "admin"},
	})
	if result.Error == "" {
		t.Error("deleting config job should fail")
	}
	if !strings.Contains(result.Error, "config-defined") {
		t.Errorf("error should mention config-defined: %s", result.Error)
	}
}

func TestToolUpdateConfigJobRejected(t *testing.T) {
	tool := newTestToolWithConfigJob(t)

	result := tool.Execute(orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "update_job",
		Args: map[string]string{"name": "config-job", "interval": "30m", "user_id": "admin"},
	})
	if result.Error == "" {
		t.Error("updating config job should fail")
	}
	if !strings.Contains(result.Error, "config-defined") {
		t.Errorf("error should mention config-defined: %s", result.Error)
	}
}

func TestToolApproverEnforced(t *testing.T) {
	tool := newTestToolWithPolicy(t, []string{"admin@co.com"}, 0)

	result := tool.Execute(orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "create_job",
		Args: map[string]string{
			"name": "j1", "interval": "1h", "action": "a.b",
			"user_id": "random@co.com",
		},
	})
	if result.Error == "" {
		t.Error("non-approver should be rejected")
	}
	if !strings.Contains(result.Error, "not authorized") {
		t.Errorf("error should mention authorization: %s", result.Error)
	}

	result = tool.Execute(orchestrator.ToolCall{
		ID: "2", Plugin: ToolName, Action: "create_job",
		Args: map[string]string{
			"name": "j1", "interval": "1h", "action": "a.b",
			"user_id": "admin@co.com",
		},
	})
	if result.Error != "" {
		t.Errorf("approver should succeed: %s", result.Error)
	}
}

func TestToolListShowsSourceAndCreator(t *testing.T) {
	tool := newTestToolWithConfigJob(t)

	tool.Execute(orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "create_job",
		Args: map[string]string{
			"name": "dyn1", "interval": "1h", "action": "a.b",
			"user_id": "diana",
		},
	})

	result := tool.Execute(orchestrator.ToolCall{
		ID: "2", Plugin: ToolName, Action: "list_jobs",
	})

	if !strings.Contains(result.Content, "config") {
		t.Errorf("list should contain config source, got: %s", result.Content)
	}
	if !strings.Contains(result.Content, "dynamic") {
		t.Errorf("list should contain dynamic source, got: %s", result.Content)
	}
	if !strings.Contains(result.Content, "diana") {
		t.Errorf("list should contain creator diana, got: %s", result.Content)
	}
}
