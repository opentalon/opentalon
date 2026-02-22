package scheduler

import (
	"encoding/json"
	"fmt"

	"github.com/opentalon/opentalon/internal/orchestrator"
)

const ToolName = "scheduler"

// SchedulerTool exposes the scheduler as a built-in tool plugin
// so the LLM can dynamically manage scheduled jobs via conversation.
type SchedulerTool struct {
	sched *Scheduler
}

func NewSchedulerTool(sched *Scheduler) *SchedulerTool {
	return &SchedulerTool{sched: sched}
}

func (t *SchedulerTool) Capability() orchestrator.PluginCapability {
	return orchestrator.PluginCapability{
		Name:        ToolName,
		Description: "Manage periodic scheduled jobs. Jobs run plugin actions at fixed intervals and optionally notify a channel with results.",
		Actions: []orchestrator.Action{
			{
				Name:        "create_job",
				Description: "Create a new scheduled job. Requires user approval before calling.",
				Parameters: []orchestrator.Parameter{
					{Name: "name", Description: "Unique job name (slug)", Required: true},
					{Name: "interval", Description: "Go duration string, e.g. 30m, 1h, 24h", Required: true},
					{Name: "action", Description: "Plugin action in format plugin.action", Required: true},
					{Name: "args", Description: "JSON object with action arguments", Required: false},
					{Name: "notify_channel", Description: "Channel ID to send results to", Required: false},
					{Name: "user_id", Description: "User requesting the job creation", Required: true},
				},
			},
			{
				Name:        "list_jobs",
				Description: "List all scheduled jobs with their status, source, and creator",
			},
			{
				Name:        "delete_job",
				Description: "Delete a dynamic scheduled job. Config-defined jobs cannot be deleted.",
				Parameters: []orchestrator.Parameter{
					{Name: "name", Description: "Job name to delete", Required: true},
					{Name: "user_id", Description: "User requesting the deletion", Required: true},
				},
			},
			{
				Name:        "pause_job",
				Description: "Pause a running scheduled job",
				Parameters: []orchestrator.Parameter{
					{Name: "name", Description: "Job name to pause", Required: true},
				},
			},
			{
				Name:        "resume_job",
				Description: "Resume a paused scheduled job",
				Parameters: []orchestrator.Parameter{
					{Name: "name", Description: "Job name to resume", Required: true},
				},
			},
			{
				Name:        "update_job",
				Description: "Update interval or notify channel of a dynamic job. Config-defined jobs cannot be updated.",
				Parameters: []orchestrator.Parameter{
					{Name: "name", Description: "Job name to update", Required: true},
					{Name: "interval", Description: "New interval (optional)", Required: false},
					{Name: "notify_channel", Description: "New notify channel (optional)", Required: false},
					{Name: "user_id", Description: "User requesting the update", Required: true},
				},
			},
		},
	}
}

func (t *SchedulerTool) Execute(call orchestrator.ToolCall) orchestrator.ToolResult {
	switch call.Action {
	case "create_job":
		return t.createJob(call)
	case "list_jobs":
		return t.listJobs(call)
	case "delete_job":
		return t.deleteJob(call)
	case "pause_job":
		return t.pauseJob(call)
	case "resume_job":
		return t.resumeJob(call)
	case "update_job":
		return t.updateJob(call)
	default:
		return orchestrator.ToolResult{
			CallID: call.ID,
			Error:  fmt.Sprintf("unknown scheduler action: %s", call.Action),
		}
	}
}

func (t *SchedulerTool) createJob(call orchestrator.ToolCall) orchestrator.ToolResult {
	name := call.Args["name"]
	interval := call.Args["interval"]
	action := call.Args["action"]
	notifyChannel := call.Args["notify_channel"]
	userID := call.Args["user_id"]

	if name == "" || interval == "" || action == "" {
		return orchestrator.ToolResult{
			CallID: call.ID,
			Error:  "name, interval, and action are required",
		}
	}

	var args map[string]string
	if raw, ok := call.Args["args"]; ok && raw != "" {
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			return orchestrator.ToolResult{
				CallID: call.ID,
				Error:  fmt.Sprintf("invalid args JSON: %v", err),
			}
		}
	}

	job := Job{
		Name:          name,
		Interval:      interval,
		Action:        action,
		Args:          args,
		NotifyChannel: notifyChannel,
	}

	if err := t.sched.AddJob(job, userID); err != nil {
		return orchestrator.ToolResult{
			CallID: call.ID,
			Error:  err.Error(),
		}
	}

	return orchestrator.ToolResult{
		CallID:  call.ID,
		Content: fmt.Sprintf("Job %q created: runs %s every %s", name, action, interval),
	}
}

func (t *SchedulerTool) listJobs(call orchestrator.ToolCall) orchestrator.ToolResult {
	jobs := t.sched.ListJobs()
	if len(jobs) == 0 {
		return orchestrator.ToolResult{
			CallID:  call.ID,
			Content: "No scheduled jobs.",
		}
	}

	data, err := json.Marshal(jobs)
	if err != nil {
		return orchestrator.ToolResult{
			CallID: call.ID,
			Error:  fmt.Sprintf("marshaling jobs: %v", err),
		}
	}

	return orchestrator.ToolResult{
		CallID:  call.ID,
		Content: string(data),
	}
}

func (t *SchedulerTool) deleteJob(call orchestrator.ToolCall) orchestrator.ToolResult {
	name := call.Args["name"]
	userID := call.Args["user_id"]
	if name == "" {
		return orchestrator.ToolResult{CallID: call.ID, Error: "name is required"}
	}
	if err := t.sched.RemoveJob(name, userID); err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: err.Error()}
	}
	return orchestrator.ToolResult{
		CallID:  call.ID,
		Content: fmt.Sprintf("Job %q deleted.", name),
	}
}

func (t *SchedulerTool) pauseJob(call orchestrator.ToolCall) orchestrator.ToolResult {
	name := call.Args["name"]
	if name == "" {
		return orchestrator.ToolResult{CallID: call.ID, Error: "name is required"}
	}
	if err := t.sched.PauseJob(name); err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: err.Error()}
	}
	return orchestrator.ToolResult{
		CallID:  call.ID,
		Content: fmt.Sprintf("Job %q paused.", name),
	}
}

func (t *SchedulerTool) resumeJob(call orchestrator.ToolCall) orchestrator.ToolResult {
	name := call.Args["name"]
	if name == "" {
		return orchestrator.ToolResult{CallID: call.ID, Error: "name is required"}
	}
	if err := t.sched.ResumeJob(name); err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: err.Error()}
	}
	return orchestrator.ToolResult{
		CallID:  call.ID,
		Content: fmt.Sprintf("Job %q resumed.", name),
	}
}

func (t *SchedulerTool) updateJob(call orchestrator.ToolCall) orchestrator.ToolResult {
	name := call.Args["name"]
	userID := call.Args["user_id"]
	if name == "" {
		return orchestrator.ToolResult{CallID: call.ID, Error: "name is required"}
	}

	var interval, notifyChannel *string
	if v, ok := call.Args["interval"]; ok && v != "" {
		interval = &v
	}
	if v, ok := call.Args["notify_channel"]; ok && v != "" {
		notifyChannel = &v
	}
	if interval == nil && notifyChannel == nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: "at least interval or notify_channel must be provided"}
	}

	if err := t.sched.UpdateJob(name, userID, interval, notifyChannel); err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: err.Error()}
	}
	return orchestrator.ToolResult{
		CallID:  call.ID,
		Content: fmt.Sprintf("Job %q updated.", name),
	}
}
