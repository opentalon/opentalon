package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

// ActionRunner executes a plugin action and returns the result content.
type ActionRunner interface {
	RunAction(ctx context.Context, plugin, action string, args map[string]string) (string, error)
}

// Notifier sends a message to a channel.
type Notifier interface {
	Notify(ctx context.Context, channelID, content string) error
}

// Job represents a scheduled job at runtime.
// Exactly one of Interval, Cron, or At must be set.
type Job struct {
	Name          string            `yaml:"name" json:"name"`
	Interval      string            `yaml:"interval,omitempty" json:"interval,omitempty"` // Go duration, e.g. "30m"
	Cron          string            `yaml:"cron,omitempty" json:"cron,omitempty"`         // 5-field cron expression, e.g. "0 9 * * *"
	At            string            `yaml:"at,omitempty" json:"at,omitempty"`             // RFC3339 UTC time for one-shot execution
	Action        string            `yaml:"action" json:"action"`
	Args          map[string]string `yaml:"args,omitempty" json:"args,omitempty"`
	NotifyChannel string            `yaml:"notify_channel,omitempty" json:"notify_channel,omitempty"`
	Paused        bool              `yaml:"paused,omitempty" json:"paused,omitempty"`
	Source        string            `yaml:"source,omitempty" json:"source,omitempty"`         // "config" or "dynamic"
	CreatedBy     string            `yaml:"created_by,omitempty" json:"created_by,omitempty"` // raw sender/actor ID used at creation
	EntityID      string            `yaml:"entity_id,omitempty" json:"entity_id,omitempty"`   // tenant identity (profile.EntityID) — empty when no profile system is configured
	Group         string            `yaml:"group,omitempty" json:"group,omitempty"`           // tenant group (profile.Group) — empty when no profile system is configured
}

// schedule computes successive fire times for a job.
type schedule interface {
	next(time.Time) time.Time
}

type intervalSchedule struct{ d time.Duration }

func (s intervalSchedule) next(t time.Time) time.Time { return t.Add(s.d) }

type cronSchedule struct{ s cron.Schedule }

func (c cronSchedule) next(t time.Time) time.Time { return c.s.Next(t) }

// oneShotSchedule fires exactly once at the given time. After firing, next()
// returns the zero Time to signal the runner to exit.
type oneShotSchedule struct {
	t     time.Time
	fired bool
}

func (s *oneShotSchedule) next(now time.Time) time.Time {
	if s.fired {
		return time.Time{}
	}
	s.fired = true
	return s.t
}

var (
	ErrConfigProtected = fmt.Errorf("config-defined jobs cannot be modified or removed")
	ErrNotAuthorized   = fmt.Errorf("not authorized — only designated approvers can manage scheduled jobs")
)

func (j *Job) parseDuration() (time.Duration, error) {
	return time.ParseDuration(j.Interval)
}

// schedule parses and validates the job's time spec. Exactly one of
// Interval, Cron, or At must be set.
func (j *Job) schedule() (schedule, error) {
	hasInterval := j.Interval != ""
	hasCron := j.Cron != ""
	hasAt := j.At != ""
	set := 0
	if hasInterval {
		set++
	}
	if hasCron {
		set++
	}
	if hasAt {
		set++
	}
	switch {
	case set > 1:
		return nil, fmt.Errorf("job %q: interval, cron, and at are mutually exclusive", j.Name)
	case set == 0:
		return nil, fmt.Errorf("job %q: one of interval, cron, or at must be set", j.Name)
	case hasInterval:
		d, err := time.ParseDuration(j.Interval)
		if err != nil {
			return nil, fmt.Errorf("job %q: invalid interval: %w", j.Name, err)
		}
		if d <= 0 {
			return nil, fmt.Errorf("job %q: interval must be positive", j.Name)
		}
		return intervalSchedule{d: d}, nil
	case hasCron:
		s, err := cron.ParseStandard(j.Cron)
		if err != nil {
			return nil, fmt.Errorf("job %q: invalid cron: %w", j.Name, err)
		}
		return cronSchedule{s: s}, nil
	default:
		t, err := time.Parse(time.RFC3339, j.At)
		if err != nil {
			return nil, fmt.Errorf("job %q: invalid at: %w", j.Name, err)
		}
		if !t.After(time.Now()) {
			return nil, fmt.Errorf("job %q: at %q is in the past", j.Name, j.At)
		}
		return &oneShotSchedule{t: t}, nil
	}
}

func (j *Job) parseAction() (plugin, action string, err error) {
	parts := strings.SplitN(j.Action, ".", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid action format %q, expected plugin.action", j.Action)
	}
	return parts[0], parts[1], nil
}

type runningJob struct {
	job    Job
	cancel context.CancelFunc
}

// Scheduler manages periodic background jobs.
type Scheduler struct {
	mu       sync.RWMutex
	jobs     map[string]*runningJob
	runner   ActionRunner
	notifier Notifier
	dataDir  string

	approvers      map[string]bool
	maxJobsPerUser int

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func New(runner ActionRunner, notifier Notifier, dataDir string) *Scheduler {
	return NewWithPolicy(runner, notifier, dataDir, nil, 0)
}

// NewWithPolicy creates a scheduler with governance rules.
// approvers: if non-empty, only listed users can create/delete/update dynamic jobs.
// maxPerUser: if > 0, limits dynamic jobs per user.
func NewWithPolicy(runner ActionRunner, notifier Notifier, dataDir string, approvers []string, maxPerUser int) *Scheduler {
	ctx, cancel := context.WithCancel(context.Background())
	aMap := make(map[string]bool, len(approvers))
	for _, a := range approvers {
		aMap[a] = true
	}
	return &Scheduler{
		jobs:           make(map[string]*runningJob),
		runner:         runner,
		notifier:       notifier,
		dataDir:        dataDir,
		approvers:      aMap,
		maxJobsPerUser: maxPerUser,
		ctx:            ctx,
		cancel:         cancel,
	}
}

func (s *Scheduler) isApprover(userID string) bool {
	if len(s.approvers) == 0 {
		return true
	}
	return s.approvers[userID]
}

func (s *Scheduler) countUserJobs(userID string) int {
	count := 0
	for _, rj := range s.jobs {
		if rj.job.Source == "dynamic" && rj.job.CreatedBy == userID {
			count++
		}
	}
	return count
}

// Start loads static jobs and persisted dynamic jobs, then launches goroutines.
func (s *Scheduler) Start(staticJobs []Job) error {
	for i := range staticJobs {
		staticJobs[i].Source = "config"
		if err := s.addJobLocked(staticJobs[i]); err != nil {
			slog.Warn("skipping static job", "component", "scheduler", "job", staticJobs[i].Name, "error", err)
		}
	}

	dynamicJobs, err := s.loadDynamic()
	if err != nil {
		slog.Warn("loading dynamic jobs failed", "component", "scheduler", "error", err)
	}
	rejected := 0
	for _, j := range dynamicJobs {
		j.Source = "dynamic"
		if err := s.addJobLocked(j); err != nil {
			slog.Warn("skipping dynamic job", "component", "scheduler", "job", j.Name, "error", err)
			rejected++
		}
	}
	// Re-persist so rejected (e.g. past-due one-shots) are pruned from disk.
	if rejected > 0 {
		if err := s.persistDynamic(); err != nil {
			slog.Warn("persist after load pruning failed", "component", "scheduler", "error", err)
		}
	}

	return nil
}

// Stop cancels all job goroutines and waits for them to drain.
func (s *Scheduler) Stop() {
	s.cancel()
	s.wg.Wait()
}

// AddJob creates a new dynamic job at runtime.
func (s *Scheduler) AddJob(job Job, userID string) error {
	if !s.isApprover(userID) {
		return ErrNotAuthorized
	}
	job.Source = "dynamic"
	job.CreatedBy = userID

	if s.maxJobsPerUser > 0 {
		s.mu.RLock()
		count := s.countUserJobs(userID)
		s.mu.RUnlock()
		if count >= s.maxJobsPerUser {
			return fmt.Errorf("job limit reached: user %q already has %d jobs (max %d)", userID, count, s.maxJobsPerUser)
		}
	}

	if err := s.addJobLocked(job); err != nil {
		return err
	}
	return s.persistDynamic()
}

// AddPersonalJob creates a dynamic job on behalf of userID without the approver
// gate. Intended for personal actions (e.g. reminders) that users set for
// themselves. maxJobsPerUser is still enforced.
func (s *Scheduler) AddPersonalJob(job Job, userID string) error {
	if userID == "" {
		return fmt.Errorf("user_id required for personal job")
	}
	job.Source = "dynamic"
	job.CreatedBy = userID

	if s.maxJobsPerUser > 0 {
		s.mu.RLock()
		count := s.countUserJobs(userID)
		s.mu.RUnlock()
		if count >= s.maxJobsPerUser {
			return fmt.Errorf("job limit reached: user %q already has %d jobs (max %d)", userID, count, s.maxJobsPerUser)
		}
	}

	if err := s.addJobLocked(job); err != nil {
		return err
	}
	return s.persistDynamic()
}

// RemoveJob stops and removes a job by name. Config-defined jobs cannot be removed.
func (s *Scheduler) RemoveJob(name, userID string) error {
	if !s.isApprover(userID) {
		return ErrNotAuthorized
	}
	s.mu.Lock()
	rj, ok := s.jobs[name]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("job %q not found", name)
	}
	if rj.job.Source == "config" {
		s.mu.Unlock()
		return ErrConfigProtected
	}
	rj.cancel()
	delete(s.jobs, name)
	s.mu.Unlock()

	return s.persistDynamic()
}

// PauseJob pauses a running job.
func (s *Scheduler) PauseJob(name string) error {
	s.mu.Lock()
	rj, ok := s.jobs[name]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("job %q not found", name)
	}
	rj.cancel()
	rj.job.Paused = true
	s.mu.Unlock()

	return s.persistDynamic()
}

// ResumeJob resumes a paused job.
func (s *Scheduler) ResumeJob(name string) error {
	s.mu.Lock()
	rj, ok := s.jobs[name]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("job %q not found", name)
	}
	if !rj.job.Paused {
		s.mu.Unlock()
		return fmt.Errorf("job %q is not paused", name)
	}
	rj.job.Paused = false
	s.mu.Unlock()

	s.startTicker(rj)
	return s.persistDynamic()
}

// UpdateJob updates the time spec (interval or cron) and/or notify channel
// of an existing job. Setting interval clears cron and vice versa.
// Config-defined jobs cannot be updated.
func (s *Scheduler) UpdateJob(name, userID string, interval, cronExpr, notifyChannel *string) error {
	if !s.isApprover(userID) {
		return ErrNotAuthorized
	}
	s.mu.Lock()
	rj, ok := s.jobs[name]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("job %q not found", name)
	}
	if rj.job.Source == "config" {
		s.mu.Unlock()
		return ErrConfigProtected
	}

	patched := rj.job
	if interval != nil {
		patched.Interval = *interval
		patched.Cron = ""
	}
	if cronExpr != nil {
		patched.Cron = *cronExpr
		patched.Interval = ""
	}
	if notifyChannel != nil {
		patched.NotifyChannel = *notifyChannel
	}
	if interval != nil || cronExpr != nil {
		if _, err := patched.schedule(); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	patched.Paused = false

	rj.cancel()
	rj.job = patched
	s.mu.Unlock()

	s.startTicker(rj)
	return s.persistDynamic()
}

// ListJobs returns all registered jobs.
func (s *Scheduler) ListJobs() []Job {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Job, 0, len(s.jobs))
	for _, rj := range s.jobs {
		out = append(out, rj.job)
	}
	return out
}

// ListJobsByEntity returns dynamic jobs owned by the given entity (profile
// identity). Useful for showing a caller just their own scheduled reminders.
func (s *Scheduler) ListJobsByEntity(entityID string) []Job {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Job, 0)
	for _, rj := range s.jobs {
		if rj.job.EntityID == entityID {
			out = append(out, rj.job)
		}
	}
	return out
}

// ListJobsByCreator returns dynamic jobs whose CreatedBy matches. Used when
// profile verification is not configured and the only identity is the raw
// sender ID.
func (s *Scheduler) ListJobsByCreator(userID string) []Job {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Job, 0)
	for _, rj := range s.jobs {
		if rj.job.CreatedBy == userID {
			out = append(out, rj.job)
		}
	}
	return out
}

// ListJobsForCaller returns jobs owned by the given caller, matching EITHER
// EntityID (when set) OR CreatedBy (as a fallback). The fallback exists for
// two reasons:
//   - jobs persisted before the EntityID field existed still need to be
//     retrievable by their owner.
//   - deployments without the profile system have empty EntityID by design;
//     CreatedBy is their only identity.
//
// Either argument may be empty, in which case only the other is matched.
func (s *Scheduler) ListJobsForCaller(entityID, userID string) []Job {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Job, 0)
	for _, rj := range s.jobs {
		if entityID != "" && rj.job.EntityID == entityID {
			out = append(out, rj.job)
			continue
		}
		if userID != "" && rj.job.CreatedBy == userID {
			out = append(out, rj.job)
		}
	}
	return out
}

// GetJob returns a job by name.
func (s *Scheduler) GetJob(name string) (Job, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rj, ok := s.jobs[name]
	if !ok {
		return Job{}, false
	}
	return rj.job, true
}

func (s *Scheduler) addJobLocked(job Job) error {
	if job.Name == "" {
		return fmt.Errorf("job name is required")
	}
	if _, err := job.schedule(); err != nil {
		return err
	}
	if _, _, err := job.parseAction(); err != nil {
		return err
	}

	s.mu.Lock()
	if _, exists := s.jobs[job.Name]; exists {
		s.mu.Unlock()
		return fmt.Errorf("job %q already exists", job.Name)
	}

	jobCtx, jobCancel := context.WithCancel(s.ctx)
	rj := &runningJob{
		job:    job,
		cancel: jobCancel,
	}
	s.jobs[job.Name] = rj
	s.mu.Unlock()

	if !job.Paused {
		s.wg.Add(1)
		go s.runJob(jobCtx, rj)
	} else {
		_ = jobCancel
	}

	return nil
}

func (s *Scheduler) startTicker(rj *runningJob) {
	jobCtx, jobCancel := context.WithCancel(s.ctx)

	s.mu.Lock()
	rj.cancel = jobCancel
	s.mu.Unlock()

	s.wg.Add(1)
	go s.runJob(jobCtx, rj)
}

func (s *Scheduler) snapshotJob(rj *runningJob) Job {
	s.mu.RLock()
	j := rj.job
	s.mu.RUnlock()
	return j
}

func (s *Scheduler) runJob(ctx context.Context, rj *runningJob) {
	defer s.wg.Done()

	job := s.snapshotJob(rj)
	sch, err := job.schedule()
	if err != nil {
		slog.Warn("job invalid schedule", "component", "scheduler", "job", job.Name, "error", err)
		return
	}

	for {
		now := time.Now()
		fireAt := sch.next(now)
		if fireAt.IsZero() {
			// schedule has no more fires (one-shot already fired)
			s.removeOneShot(job.Name)
			return
		}
		wait := fireAt.Sub(now)
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			s.executeJob(rj)
		}
	}
}

// removeOneShot removes a one-shot job from the registry after it has fired,
// and re-persists the dynamic jobs file.
func (s *Scheduler) removeOneShot(name string) {
	s.mu.Lock()
	rj, ok := s.jobs[name]
	if !ok {
		s.mu.Unlock()
		return
	}
	isDynamic := rj.job.Source == "dynamic"
	delete(s.jobs, name)
	s.mu.Unlock()
	if isDynamic {
		if err := s.persistDynamic(); err != nil {
			slog.Warn("persist after one-shot removal failed", "component", "scheduler", "job", name, "error", err)
		}
	}
}

func (s *Scheduler) executeJob(rj *runningJob) {
	job := s.snapshotJob(rj)

	plugin, action, err := job.parseAction()
	if err != nil {
		slog.Warn("job bad action", "component", "scheduler", "job", job.Name, "error", err)
		return
	}

	result, err := s.runner.RunAction(s.ctx, plugin, action, job.Args)
	if err != nil {
		slog.Warn("job execution failed", "component", "scheduler", "job", job.Name, "error", err)
		return
	}

	if job.NotifyChannel != "" && s.notifier != nil {
		msg := fmt.Sprintf("[scheduled: %s] %s", job.Name, result)
		if err := s.notifier.Notify(s.ctx, job.NotifyChannel, msg); err != nil {
			slog.Warn("job notify failed", "component", "scheduler", "job", job.Name, "error", err)
		}
	}
}

func (s *Scheduler) persistPath() string {
	return filepath.Join(s.dataDir, "scheduler", "jobs.yaml")
}

func (s *Scheduler) persistDynamic() error {
	if s.dataDir == "" {
		return nil
	}

	s.mu.RLock()
	var dynamicJobs []Job
	for _, rj := range s.jobs {
		if rj.job.Source == "dynamic" {
			dynamicJobs = append(dynamicJobs, rj.job)
		}
	}
	s.mu.RUnlock()

	dir := filepath.Dir(s.persistPath())
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("creating scheduler dir: %w", err)
	}

	data, err := yaml.Marshal(dynamicJobs)
	if err != nil {
		return fmt.Errorf("marshaling jobs: %w", err)
	}

	return os.WriteFile(s.persistPath(), data, 0600)
}

func (s *Scheduler) loadDynamic() ([]Job, error) {
	if s.dataDir == "" {
		return nil, nil
	}

	data, err := os.ReadFile(s.persistPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading jobs file: %w", err)
	}

	var jobs []Job
	if err := yaml.Unmarshal(data, &jobs); err != nil {
		return nil, fmt.Errorf("parsing jobs file: %w", err)
	}
	return jobs, nil
}
