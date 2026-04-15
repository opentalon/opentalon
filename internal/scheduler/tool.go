package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/opentalon/opentalon/internal/actor"
	"github.com/opentalon/opentalon/internal/orchestrator"
	"github.com/opentalon/opentalon/internal/profile"
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
				Description: "Create a new scheduled job. Provide exactly one of interval or cron. Requires user approval before calling.",
				Parameters: []orchestrator.Parameter{
					{Name: "name", Description: "Unique job name (slug)", Required: true},
					{Name: "interval", Description: "Go duration string, e.g. 30m, 1h, 24h (mutually exclusive with cron)", Required: false},
					{Name: "cron", Description: "5-field cron expression, e.g. '0 9 * * *' (mutually exclusive with interval)", Required: false},
					{Name: "action", Description: "Plugin action in format plugin.action", Required: true},
					{Name: "args", Description: "JSON object with action arguments", Required: false},
					{Name: "notify_channel", Description: "Channel ID to send results to", Required: false},
					{Name: "user_id", Description: "User requesting the job creation", Required: true},
				},
			},
			{
				Name: "list_jobs",
				Description: "List scheduled jobs. By default returns only jobs owned by the current caller " +
					"(filtered by profile entity_id when the profile system is configured, otherwise by creator). " +
					"Pass scope=\"all\" to see every job — this is intended for approvers/admins.",
				Parameters: []orchestrator.Parameter{
					{Name: "scope", Description: "\"mine\" (default) or \"all\"", Required: false},
				},
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
				Name: "remind_me",
				Description: "Schedule a personal one-shot reminder for the current user. " +
					"Use this for prompts like 'remind me about X in N hours' or 'remind me at <time> to do Y'. " +
					"The 'at' argument MUST be an absolute RFC3339 timestamp in UTC (e.g. 2026-04-15T17:00:00Z) — convert the user's relative time yourself. " +
					"To deliver literal text back to the user, either set 'message' alone (preferred shortcut) or set action=\"reminder.say\" with args={\"message\":\"…\"}. " +
					"To run a real plugin action at the scheduled time, set 'action' to \"plugin.action\" and pass its arguments as a JSON object in 'args'. " +
					"Does NOT require approver permission — every user can set their own reminders.",
				Parameters: []orchestrator.Parameter{
					{Name: "at", Description: "Absolute RFC3339 UTC timestamp when the reminder should fire (e.g. 2026-04-15T17:00:00Z)", Required: true},
					{Name: "message", Description: "Literal text to deliver; shortcut for action=reminder.say", Required: false},
					{Name: "action", Description: "Plugin action in the form plugin.action (omit if using message)", Required: false},
					{Name: "args", Description: "JSON object with action arguments (omit if using message)", Required: false},
				},
			},
			{
				Name:        "update_job",
				Description: "Update time spec (interval or cron) or notify channel of a dynamic job. Setting interval clears cron and vice versa. Config-defined jobs cannot be updated.",
				Parameters: []orchestrator.Parameter{
					{Name: "name", Description: "Job name to update", Required: true},
					{Name: "interval", Description: "New interval (optional, mutually exclusive with cron)", Required: false},
					{Name: "cron", Description: "New cron expression (optional, mutually exclusive with interval)", Required: false},
					{Name: "notify_channel", Description: "New notify channel (optional)", Required: false},
					{Name: "user_id", Description: "User requesting the update", Required: true},
				},
			},
		},
	}
}

func (t *SchedulerTool) Execute(ctx context.Context, call orchestrator.ToolCall) orchestrator.ToolResult {
	switch call.Action {
	case "create_job":
		return t.createJob(call)
	case "list_jobs":
		return t.listJobs(ctx, call)
	case "delete_job":
		return t.deleteJob(call)
	case "pause_job":
		return t.pauseJob(call)
	case "resume_job":
		return t.resumeJob(call)
	case "update_job":
		return t.updateJob(call)
	case "remind_me":
		return t.remindMe(ctx, call)
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
	cronExpr := call.Args["cron"]
	action := call.Args["action"]
	notifyChannel := call.Args["notify_channel"]
	userID := call.Args["user_id"]

	if name == "" || action == "" {
		return orchestrator.ToolResult{
			CallID: call.ID,
			Error:  "name and action are required",
		}
	}
	if (interval == "") == (cronExpr == "") {
		return orchestrator.ToolResult{
			CallID: call.ID,
			Error:  "exactly one of interval or cron is required",
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
		Cron:          cronExpr,
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

	when := "every " + interval
	if cronExpr != "" {
		when = "on cron " + cronExpr
	}
	return orchestrator.ToolResult{
		CallID:  call.ID,
		Content: fmt.Sprintf("Job %q created: runs %s %s", name, action, when),
	}
}

func (t *SchedulerTool) listJobs(ctx context.Context, call orchestrator.ToolCall) orchestrator.ToolResult {
	scope := strings.TrimSpace(call.Args["scope"])
	var jobs []Job
	switch scope {
	case "", "mine":
		if p := profile.FromContext(ctx); p != nil && p.EntityID != "" {
			jobs = t.sched.ListJobsByEntity(p.EntityID)
		} else if actorID := actor.Actor(ctx); actorID != "" {
			// No profile system: fall back to channel:sender → CreatedBy match.
			parts := strings.SplitN(actorID, ":", 2)
			sender := actorID
			if len(parts) == 2 {
				sender = parts[1]
			}
			jobs = t.sched.ListJobsByCreator(sender)
		} else {
			jobs = nil
		}
	case "all":
		jobs = t.sched.ListJobs()
	default:
		return orchestrator.ToolResult{CallID: call.ID, Error: fmt.Sprintf("invalid scope %q, expected \"mine\" or \"all\"", scope)}
	}
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

	var interval, cronExpr, notifyChannel *string
	if v, ok := call.Args["interval"]; ok && v != "" {
		interval = &v
	}
	if v, ok := call.Args["cron"]; ok && v != "" {
		cronExpr = &v
	}
	if v, ok := call.Args["notify_channel"]; ok && v != "" {
		notifyChannel = &v
	}
	if interval != nil && cronExpr != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: "interval and cron are mutually exclusive"}
	}
	if interval == nil && cronExpr == nil && notifyChannel == nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: "at least one of interval, cron, or notify_channel must be provided"}
	}

	if err := t.sched.UpdateJob(name, userID, interval, cronExpr, notifyChannel); err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: err.Error()}
	}
	return orchestrator.ToolResult{
		CallID:  call.ID,
		Content: fmt.Sprintf("Job %q updated.", name),
	}
}

func (t *SchedulerTool) remindMe(ctx context.Context, call orchestrator.ToolCall) orchestrator.ToolResult {
	actorID := actor.Actor(ctx)
	if actorID == "" {
		return orchestrator.ToolResult{CallID: call.ID, Error: "remind_me requires user context"}
	}

	// Resolve owner identity. Two modes:
	//   - Profile system configured: actor is the profile EntityID; channel/
	//     sender come from profile.FromContext which also gives us the group.
	//   - No profile: actor is "channel_id:sender_id" and entity/group are empty.
	var channelID, senderID, entityID, group string
	if p := profile.FromContext(ctx); p != nil {
		entityID = p.EntityID
		group = p.Group
		channelID = p.ChannelID
		senderID = p.EntityID
	} else {
		parts := strings.SplitN(actorID, ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return orchestrator.ToolResult{CallID: call.ID, Error: fmt.Sprintf("remind_me: malformed actor %q, expected channel_id:sender_id", actorID)}
		}
		channelID, senderID = parts[0], parts[1]
	}

	at := strings.TrimSpace(call.Args["at"])
	if at == "" {
		return orchestrator.ToolResult{CallID: call.ID, Error: "remind_me: 'at' is required (RFC3339 UTC)"}
	}
	fireTime, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: fmt.Sprintf("remind_me: invalid 'at' %q: %v", at, err)}
	}
	if !fireTime.After(time.Now()) {
		return orchestrator.ToolResult{CallID: call.ID, Error: fmt.Sprintf("remind_me: 'at' %q is in the past", at)}
	}

	action := strings.TrimSpace(call.Args["action"])
	message := call.Args["message"]
	var args map[string]string

	switch {
	case action == "" && message != "":
		action = "reminder.say"
		args = map[string]string{"message": message}
	case action == "" && message == "":
		return orchestrator.ToolResult{CallID: call.ID, Error: "remind_me: provide either 'message' or 'action'"}
	case action != "":
		if raw, ok := call.Args["args"]; ok && raw != "" {
			if err := json.Unmarshal([]byte(raw), &args); err != nil {
				return orchestrator.ToolResult{CallID: call.ID, Error: fmt.Sprintf("remind_me: invalid args JSON: %v", err)}
			}
		}
	}

	// Sanitize senderID for job name (avoid ':' or spaces collisions).
	nameSafeSender := strings.NewReplacer(":", "_", " ", "_", "/", "_").Replace(senderID)
	// Nanosecond suffix guards against two reminders set in the same second.
	name := fmt.Sprintf("remind-%s-%d", nameSafeSender, time.Now().UnixNano())

	job := Job{
		Name:          name,
		At:            fireTime.UTC().Format(time.RFC3339),
		Action:        action,
		Args:          args,
		NotifyChannel: channelID,
		EntityID:      entityID,
		Group:         group,
	}
	if err := t.sched.AddPersonalJob(job, senderID); err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: err.Error()}
	}
	return orchestrator.ToolResult{
		CallID:  call.ID,
		Content: fmt.Sprintf("Reminder set for %s: will run %s.", job.At, action),
	}
}
