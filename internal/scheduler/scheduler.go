package scheduler

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

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
type Job struct {
	Name          string            `yaml:"name" json:"name"`
	Interval      string            `yaml:"interval" json:"interval"`
	Action        string            `yaml:"action" json:"action"`
	Args          map[string]string `yaml:"args,omitempty" json:"args,omitempty"`
	NotifyChannel string            `yaml:"notify_channel,omitempty" json:"notify_channel,omitempty"`
	Paused        bool              `yaml:"paused,omitempty" json:"paused,omitempty"`
	Source        string            `yaml:"source,omitempty" json:"source,omitempty"`         // "config" or "dynamic"
	CreatedBy     string            `yaml:"created_by,omitempty" json:"created_by,omitempty"` // user who created the job
}

var (
	ErrConfigProtected = fmt.Errorf("config-defined jobs cannot be modified or removed")
	ErrNotAuthorized   = fmt.Errorf("not authorized — only designated approvers can manage scheduled jobs")
)

func (j *Job) parseDuration() (time.Duration, error) {
	return time.ParseDuration(j.Interval)
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
			log.Printf("scheduler: skipping static job %q: %v", staticJobs[i].Name, err)
		}
	}

	dynamicJobs, err := s.loadDynamic()
	if err != nil {
		log.Printf("scheduler: loading dynamic jobs: %v", err)
	}
	for _, j := range dynamicJobs {
		j.Source = "dynamic"
		if err := s.addJobLocked(j); err != nil {
			log.Printf("scheduler: skipping dynamic job %q: %v", j.Name, err)
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

// UpdateJob updates the interval and/or notify channel of an existing job.
// Config-defined jobs cannot be updated.
func (s *Scheduler) UpdateJob(name, userID string, interval, notifyChannel *string) error {
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

	if interval != nil {
		rj.job.Interval = *interval
	}
	if notifyChannel != nil {
		rj.job.NotifyChannel = *notifyChannel
	}
	rj.job.Paused = false
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
	if _, err := job.parseDuration(); err != nil {
		return fmt.Errorf("invalid interval for job %q: %w", job.Name, err)
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
	dur, err := job.parseDuration()
	if err != nil {
		log.Printf("scheduler: job %q invalid interval: %v", job.Name, err)
		return
	}

	ticker := time.NewTicker(dur)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.executeJob(rj)
		}
	}
}

func (s *Scheduler) executeJob(rj *runningJob) {
	job := s.snapshotJob(rj)

	plugin, action, err := job.parseAction()
	if err != nil {
		log.Printf("scheduler: job %q bad action: %v", job.Name, err)
		return
	}

	result, err := s.runner.RunAction(s.ctx, plugin, action, job.Args)
	if err != nil {
		log.Printf("scheduler: job %q execution error: %v", job.Name, err)
		return
	}

	if job.NotifyChannel != "" && s.notifier != nil {
		msg := fmt.Sprintf("[scheduled: %s] %s", job.Name, result)
		if err := s.notifier.Notify(s.ctx, job.NotifyChannel, msg); err != nil {
			log.Printf("scheduler: job %q notify error: %v", job.Name, err)
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
