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

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/orchestrator"
	"github.com/opentalon/opentalon/internal/profile"
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
		{"content.check_violations", "content", "check_violations", false},
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
	if err := s.UpdateJob("upd1", "user1", &newInterval, nil, nil); err != nil {
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
	if err := s.UpdateJob("upd1", "user1", nil, nil, &newChan); err != nil {
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
	err := s.UpdateJob("static1", "admin", &newInterval, nil, nil)
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
	err := s.UpdateJob("j1", "random@co.com", &newInterval, nil, nil)
	if err != ErrNotAuthorized {
		t.Errorf("non-approver should not update, got %v", err)
	}

	err = s.UpdateJob("j1", "admin@co.com", &newInterval, nil, nil)
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

func TestJobScheduleValidation(t *testing.T) {
	tests := []struct {
		name    string
		job     Job
		wantErr string
	}{
		{"no spec", Job{Name: "j"}, "one of interval, cron, or at"},
		{"both set", Job{Name: "j", Interval: "1h", Cron: "* * * * *"}, "mutually exclusive"},
		{"bad interval", Job{Name: "j", Interval: "nope"}, "invalid interval"},
		{"zero interval", Job{Name: "j", Interval: "0s"}, "must be positive"},
		{"bad cron", Job{Name: "j", Cron: "not a cron"}, "invalid cron"},
		{"good interval", Job{Name: "j", Interval: "30m"}, ""},
		{"good cron", Job{Name: "j", Cron: "0 9 * * *"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.job.schedule()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestSchedulerCronExecution(t *testing.T) {
	runner := &fakeRunner{}
	s := New(runner, nil, "")

	// robfig/cron v3 supports "@every Xs" shorthand via ParseStandard's descriptor
	// fallback; but to avoid that edge case, use the plain 5-field form with
	// seconds granularity is not possible. Validate via addJob only.
	if err := s.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	if err := s.AddJob(Job{Name: "daily", Cron: "0 9 * * *", Action: "a.b"}, "user1"); err != nil {
		t.Fatalf("valid cron job rejected: %v", err)
	}
	j, ok := s.GetJob("daily")
	if !ok || j.Cron != "0 9 * * *" {
		t.Error("cron job not stored correctly")
	}
}

func TestSchedulerAddJobRejectsBadCron(t *testing.T) {
	s := New(&fakeRunner{}, nil, "")
	if err := s.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	err := s.AddJob(Job{Name: "bad", Cron: "garbage", Action: "a.b"}, "user1")
	if err == nil {
		t.Error("expected error for bad cron")
	}
}

func TestSchedulerAddJobRejectsBothSpecs(t *testing.T) {
	s := New(&fakeRunner{}, nil, "")
	if err := s.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	err := s.AddJob(Job{Name: "both", Interval: "1h", Cron: "0 * * * *", Action: "a.b"}, "user1")
	if err == nil {
		t.Error("expected error when both interval and cron are set")
	}
}

func TestSchedulerUpdateSwitchIntervalToCron(t *testing.T) {
	s := New(&fakeRunner{}, nil, t.TempDir())
	if err := s.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	if err := s.AddJob(Job{Name: "swap", Interval: "1h", Action: "a.b"}, "u"); err != nil {
		t.Fatal(err)
	}
	newCron := "0 9 * * *"
	if err := s.UpdateJob("swap", "u", nil, &newCron, nil); err != nil {
		t.Fatal(err)
	}
	j, _ := s.GetJob("swap")
	if j.Cron != "0 9 * * *" {
		t.Errorf("cron = %q, want 0 9 * * *", j.Cron)
	}
	if j.Interval != "" {
		t.Errorf("interval should be cleared, got %q", j.Interval)
	}
}

func TestSchedulerUpdateRejectsBadCron(t *testing.T) {
	s := New(&fakeRunner{}, nil, t.TempDir())
	if err := s.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	if err := s.AddJob(Job{Name: "j", Interval: "1h", Action: "a.b"}, "u"); err != nil {
		t.Fatal(err)
	}
	bad := "not a cron"
	if err := s.UpdateJob("j", "u", nil, &bad, nil); err == nil {
		t.Error("expected error for bad cron in update")
	}
	// original job should be unchanged
	j, _ := s.GetJob("j")
	if j.Interval != "1h" {
		t.Errorf("interval should be preserved on failed update, got %q", j.Interval)
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

// --- One-shot (at) tests ---

func TestSchedulerOneShotFiresOnceAndRemoves(t *testing.T) {
	runner := &fakeRunner{}
	s := New(runner, nil, "")
	if err := s.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	at := time.Now().Add(80 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	if err := s.AddPersonalJob(Job{Name: "once", At: at, Action: "a.b"}, "u"); err != nil {
		t.Fatal(err)
	}

	time.Sleep(300 * time.Millisecond)

	if runner.callCount() != 1 {
		t.Errorf("expected exactly 1 call, got %d", runner.callCount())
	}
	if _, ok := s.GetJob("once"); ok {
		t.Error("one-shot should have been removed after firing")
	}
}

func TestSchedulerOneShotPastDueRejected(t *testing.T) {
	s := New(&fakeRunner{}, nil, "")
	if err := s.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	err := s.AddPersonalJob(Job{Name: "past", At: past, Action: "a.b"}, "u")
	if err == nil || !strings.Contains(err.Error(), "in the past") {
		t.Errorf("expected past-due rejection, got %v", err)
	}
}

func TestSchedulerOneShotMutualExclusion(t *testing.T) {
	tests := []Job{
		{Name: "j", Interval: "1h", At: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
		{Name: "j", Cron: "0 9 * * *", At: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
		{Name: "j", Interval: "1h", Cron: "0 9 * * *", At: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
	}
	for i, tc := range tests {
		if _, err := tc.schedule(); err == nil {
			t.Errorf("case %d: expected mutual-exclusion error", i)
		}
	}
}

func TestSchedulerOneShotPersistedAndReloaded(t *testing.T) {
	dir := t.TempDir()
	at := time.Now().Add(200 * time.Millisecond).UTC().Format(time.RFC3339Nano)

	s1 := New(&fakeRunner{}, nil, dir)
	if err := s1.Start(nil); err != nil {
		t.Fatal(err)
	}
	if err := s1.AddPersonalJob(Job{Name: "later", At: at, Action: "a.b"}, "u"); err != nil {
		t.Fatal(err)
	}
	s1.Stop()

	// Reload — at is still in the future; should fire.
	runner2 := &fakeRunner{}
	s2 := New(runner2, nil, dir)
	if err := s2.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer s2.Stop()

	if _, ok := s2.GetJob("later"); !ok {
		t.Fatal("reloaded job missing before fire time")
	}

	time.Sleep(400 * time.Millisecond)
	if runner2.callCount() != 1 {
		t.Errorf("reloaded one-shot should fire once, got %d calls", runner2.callCount())
	}
	if _, ok := s2.GetJob("later"); ok {
		t.Error("reloaded one-shot should have been removed after firing")
	}
}

func TestSchedulerOneShotExpiredOnReload(t *testing.T) {
	dir := t.TempDir()

	// Seed jobs.yaml directly with a past-due one-shot alongside a valid job.
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	contents := []byte(fmt.Sprintf(`- name: ghost
  at: %q
  action: a.b
  source: dynamic
  created_by: u
- name: keep
  at: %q
  action: a.b
  source: dynamic
  created_by: u
`, past, future))
	if err := os.MkdirAll(filepath.Join(dir, "scheduler"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scheduler", "jobs.yaml"), contents, 0600); err != nil {
		t.Fatal(err)
	}

	s := New(&fakeRunner{}, nil, dir)
	if err := s.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	if _, ok := s.GetJob("ghost"); ok {
		t.Error("expired one-shot should not be loaded")
	}
	if _, ok := s.GetJob("keep"); !ok {
		t.Error("valid one-shot should still be loaded")
	}

	// And the on-disk file should have been pruned.
	data, err := os.ReadFile(filepath.Join(dir, "scheduler", "jobs.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "ghost") {
		t.Error("jobs.yaml should have been re-persisted without expired one-shot")
	}
}

// --- AddPersonalJob tests ---

func TestAddPersonalJobBypassesApprover(t *testing.T) {
	s := NewWithPolicy(&fakeRunner{}, nil, "", []string{"admin@co.com"}, 0)
	if err := s.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	err := s.AddPersonalJob(Job{Name: "p1", Interval: "1h", Action: "a.b"}, "random@co.com")
	if err != nil {
		t.Errorf("personal job should bypass approver: %v", err)
	}
}

func TestAddPersonalJobEnforcesMaxPerUser(t *testing.T) {
	s := NewWithPolicy(&fakeRunner{}, nil, "", nil, 2)
	if err := s.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	if err := s.AddPersonalJob(Job{Name: "p1", Interval: "1h", Action: "a.b"}, "diana"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddPersonalJob(Job{Name: "p2", Interval: "1h", Action: "a.b"}, "diana"); err != nil {
		t.Fatal(err)
	}
	err := s.AddPersonalJob(Job{Name: "p3", Interval: "1h", Action: "a.b"}, "diana")
	if err == nil || !strings.Contains(err.Error(), "job limit") {
		t.Errorf("expected job limit error, got %v", err)
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

	ctx := actor.WithActor(context.Background(), "slack:admin")
	result := tool.Execute(ctx, orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "delete_job",
		Args: map[string]string{"name": "config-job"},
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

	ctx := actor.WithActor(context.Background(), "slack:admin")
	result := tool.Execute(ctx, orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "update_job",
		Args: map[string]string{"name": "config-job", "interval": "30m"},
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

	ctxRandom := actor.WithActor(context.Background(), "slack:random@co.com")
	result := tool.Execute(ctxRandom, orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "create_job",
		Args: map[string]string{
			"name": "j1", "interval": "1h", "action": "a.b",
		},
	})
	if result.Error == "" {
		t.Error("non-approver should be rejected")
	}
	if !strings.Contains(result.Error, "not authorized") {
		t.Errorf("error should mention authorization: %s", result.Error)
	}

	ctxAdmin := actor.WithActor(context.Background(), "slack:admin@co.com")
	result = tool.Execute(ctxAdmin, orchestrator.ToolCall{
		ID: "2", Plugin: ToolName, Action: "create_job",
		Args: map[string]string{
			"name": "j1", "interval": "1h", "action": "a.b",
		},
	})
	if result.Error != "" {
		t.Errorf("approver should succeed: %s", result.Error)
	}
}

// --- remind_me tool tests ---

func TestToolRemindMeMessageShortcut(t *testing.T) {
	tool := newTestToolWithPolicy(t, nil, 0)

	at := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	ctx := actor.WithActor(context.Background(), "slack:U123")
	res := tool.Execute(ctx, orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "remind_me",
		Args: map[string]string{"at": at, "message": "you promised poems"},
	})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}

	jobs := tool.sched.ListJobs()
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	j := jobs[0]
	if j.Action != "reminder.say" {
		t.Errorf("action = %q, want reminder.say", j.Action)
	}
	if j.Args["message"] != "you promised poems" {
		t.Errorf("message arg = %q", j.Args["message"])
	}
	if j.NotifyChannel != "slack" {
		t.Errorf("notify_channel = %q, want slack", j.NotifyChannel)
	}
	if j.CreatedBy != "U123" {
		t.Errorf("created_by = %q, want U123", j.CreatedBy)
	}
	if j.At == "" {
		t.Error("at should be set")
	}
}

func TestToolRemindMePluginAction(t *testing.T) {
	tool := newTestToolWithPolicy(t, nil, 0)

	at := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	ctx := actor.WithActor(context.Background(), "slack:alice")
	res := tool.Execute(ctx, orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "remind_me",
		Args: map[string]string{
			"at":     at,
			"action": "jira.get_issue",
			"args":   `{"issue_id":"XYZ"}`,
		},
	})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	j := tool.sched.ListJobs()[0]
	if j.Action != "jira.get_issue" {
		t.Errorf("action = %q", j.Action)
	}
	if j.Args["issue_id"] != "XYZ" {
		t.Errorf("issue_id = %q", j.Args["issue_id"])
	}
}

func TestToolRemindMeRequiresActor(t *testing.T) {
	tool := newTestToolWithPolicy(t, nil, 0)
	at := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	res := tool.Execute(context.Background(), orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "remind_me",
		Args: map[string]string{"at": at, "message": "x"},
	})
	if res.Error == "" || !strings.Contains(res.Error, "user context") {
		t.Errorf("expected user-context error, got %q", res.Error)
	}
}

func TestToolRemindMeRejectsPast(t *testing.T) {
	tool := newTestToolWithPolicy(t, nil, 0)
	ctx := actor.WithActor(context.Background(), "slack:alice")
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	res := tool.Execute(ctx, orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "remind_me",
		Args: map[string]string{"at": past, "message": "x"},
	})
	if res.Error == "" || !strings.Contains(res.Error, "past") {
		t.Errorf("expected past-time error, got %q", res.Error)
	}
}

func TestToolRemindMeBypassesApprover(t *testing.T) {
	tool := newTestToolWithPolicy(t, []string{"admin@co.com"}, 0)

	at := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	ctx := actor.WithActor(context.Background(), "slack:random@co.com")
	res := tool.Execute(ctx, orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "remind_me",
		Args: map[string]string{"at": at, "message": "x"},
	})
	if res.Error != "" {
		t.Errorf("remind_me should bypass approver: %s", res.Error)
	}
}

func TestToolRemindMePopulatesEntityAndGroup(t *testing.T) {
	tool := newTestToolWithPolicy(t, nil, 0)

	at := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	ctx := actor.WithActor(context.Background(), "ent-42")
	ctx = profile.WithProfile(ctx, &profile.Profile{
		EntityID:  "ent-42",
		Group:     "team-a",
		ChannelID: "slack",
	})
	res := tool.Execute(ctx, orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "remind_me",
		Args: map[string]string{"at": at, "message": "hi"},
	})
	if res.Error != "" {
		t.Fatalf("unexpected error: %s", res.Error)
	}
	jobs := tool.sched.ListJobs()
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	j := jobs[0]
	if j.EntityID != "ent-42" {
		t.Errorf("entity_id = %q, want ent-42", j.EntityID)
	}
	if j.Group != "team-a" {
		t.Errorf("group = %q, want team-a", j.Group)
	}
	if j.NotifyChannel != "slack" {
		t.Errorf("notify_channel = %q, want slack (from profile)", j.NotifyChannel)
	}
	if j.CreatedBy != "ent-42" {
		t.Errorf("created_by = %q, want ent-42", j.CreatedBy)
	}
}

func TestToolListJobsMineScope(t *testing.T) {
	tool := newTestToolWithPolicy(t, nil, 0)

	// Two reminders for two different tenants.
	at := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	for _, entity := range []string{"ent-a", "ent-b"} {
		ctx := actor.WithActor(context.Background(), entity)
		ctx = profile.WithProfile(ctx, &profile.Profile{EntityID: entity, ChannelID: "slack"})
		res := tool.Execute(ctx, orchestrator.ToolCall{
			ID: "1", Plugin: ToolName, Action: "remind_me",
			Args: map[string]string{"at": at, "message": "hi " + entity},
		})
		if res.Error != "" {
			t.Fatalf("remind_me for %s: %s", entity, res.Error)
		}
	}

	// Default scope: ent-a sees only its own reminder.
	ctxA := profile.WithProfile(context.Background(), &profile.Profile{EntityID: "ent-a"})
	res := tool.Execute(ctxA, orchestrator.ToolCall{ID: "1", Plugin: ToolName, Action: "list_jobs"})
	if res.Error != "" {
		t.Fatalf("list_jobs: %s", res.Error)
	}
	if !strings.Contains(res.Content, "hi ent-a") {
		t.Errorf("ent-a should see own reminder: %s", res.Content)
	}
	if strings.Contains(res.Content, "hi ent-b") {
		t.Errorf("ent-a should NOT see ent-b reminder: %s", res.Content)
	}

	// scope=all: both visible.
	resAll := tool.Execute(ctxA, orchestrator.ToolCall{
		ID: "2", Plugin: ToolName, Action: "list_jobs",
		Args: map[string]string{"scope": "all"},
	})
	if !strings.Contains(resAll.Content, "hi ent-a") || !strings.Contains(resAll.Content, "hi ent-b") {
		t.Errorf("scope=all should show both, got: %s", resAll.Content)
	}
}

func TestToolListJobsMineFallbackToCreator(t *testing.T) {
	tool := newTestToolWithPolicy(t, nil, 0)

	// No profile, use channel:sender actor.
	at := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	ctxA := actor.WithActor(context.Background(), "slack:alice")
	ctxB := actor.WithActor(context.Background(), "slack:bob")
	for _, pair := range []struct {
		ctx context.Context
		msg string
	}{
		{ctxA, "alice-msg"}, {ctxB, "bob-msg"},
	} {
		res := tool.Execute(pair.ctx, orchestrator.ToolCall{
			ID: "1", Plugin: ToolName, Action: "remind_me",
			Args: map[string]string{"at": at, "message": pair.msg},
		})
		if res.Error != "" {
			t.Fatalf("%s: %s", pair.msg, res.Error)
		}
	}

	res := tool.Execute(ctxA, orchestrator.ToolCall{ID: "1", Plugin: ToolName, Action: "list_jobs"})
	if !strings.Contains(res.Content, "alice-msg") {
		t.Errorf("alice should see own reminder: %s", res.Content)
	}
	if strings.Contains(res.Content, "bob-msg") {
		t.Errorf("alice should NOT see bob reminder: %s", res.Content)
	}
}

func TestSchedulerListJobsByEntity(t *testing.T) {
	s := New(&fakeRunner{}, nil, "")
	if err := s.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	_ = s.AddPersonalJob(Job{Name: "a1", Interval: "1h", Action: "a.b", EntityID: "ent-a"}, "sender-a")
	_ = s.AddPersonalJob(Job{Name: "a2", Interval: "1h", Action: "a.b", EntityID: "ent-a"}, "sender-a")
	_ = s.AddPersonalJob(Job{Name: "b1", Interval: "1h", Action: "a.b", EntityID: "ent-b"}, "sender-b")

	got := s.ListJobsByEntity("ent-a")
	if len(got) != 2 {
		t.Errorf("expected 2 jobs for ent-a, got %d", len(got))
	}
	gotB := s.ListJobsByEntity("ent-b")
	if len(gotB) != 1 {
		t.Errorf("expected 1 job for ent-b, got %d", len(gotB))
	}
	gotNone := s.ListJobsByEntity("ent-ghost")
	if len(gotNone) != 0 {
		t.Errorf("expected 0 for ghost, got %d", len(gotNone))
	}
}

// Regression: a job created via create_job must be visible to its creator
// under the default "mine" scope (the bug where list_jobs said "no jobs"
// right after a successful create_job).
func TestToolCreateJobVisibleToCreator(t *testing.T) {
	tool := newTestToolWithPolicy(t, nil, 0)
	ctx := actor.WithActor(context.Background(), "slack:diana")

	res := tool.Execute(ctx, orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "create_job",
		Args: map[string]string{"name": "rubaiyat", "cron": "*/5 * * * *", "action": "poem.emit"},
	})
	if res.Error != "" {
		t.Fatalf("create_job: %s", res.Error)
	}

	list := tool.Execute(ctx, orchestrator.ToolCall{ID: "2", Plugin: ToolName, Action: "list_jobs"})
	if !strings.Contains(list.Content, "rubaiyat") {
		t.Errorf("creator must see own job, got: %s", list.Content)
	}
}

// Regression: jobs persisted before EntityID existed (empty EntityID,
// non-empty CreatedBy) must still be retrievable by the caller who created
// them, otherwise they appear to vanish after an upgrade.
func TestListJobsForCallerLegacyFallback(t *testing.T) {
	s := New(&fakeRunner{}, nil, "")
	if err := s.Start(nil); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	// Legacy job: no EntityID, CreatedBy only.
	_ = s.AddPersonalJob(Job{Name: "legacy", Interval: "1h", Action: "a.b"}, "diana")
	// Modern job: EntityID set.
	_ = s.AddPersonalJob(Job{Name: "modern", Interval: "1h", Action: "a.b", EntityID: "ent-diana"}, "diana")

	// Profile-era caller: ent-diana with userID diana sees both.
	got := s.ListJobsForCaller("ent-diana", "diana")
	if len(got) != 2 {
		t.Errorf("expected 2 jobs (legacy + modern), got %d: %+v", len(got), got)
	}

	// Only userID known: still sees both (modern matches via CreatedBy fallback).
	got = s.ListJobsForCaller("", "diana")
	if len(got) != 2 {
		t.Errorf("no-profile caller should see both, got %d", len(got))
	}

	// Different entity + different user: sees nothing.
	got = s.ListJobsForCaller("ent-other", "bob")
	if len(got) != 0 {
		t.Errorf("unrelated caller should see nothing, got %d", len(got))
	}
}

func TestToolListShowsSourceAndCreator(t *testing.T) {
	tool := newTestToolWithConfigJob(t)

	ctx := actor.WithActor(context.Background(), "slack:diana")
	tool.Execute(ctx, orchestrator.ToolCall{
		ID: "1", Plugin: ToolName, Action: "create_job",
		Args: map[string]string{
			"name": "dyn1", "interval": "1h", "action": "a.b",
		},
	})

	result := tool.Execute(ctx, orchestrator.ToolCall{
		ID: "2", Plugin: ToolName, Action: "list_jobs",
		Args: map[string]string{"scope": "all"},
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
