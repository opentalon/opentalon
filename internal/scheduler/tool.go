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
					{Name: "args", Description: "JSON-encoded object passed as a string, e.g. args={\"issue_id\":\"XYZ\"}. Action-specific keys MUST go inside this object, NOT at top level (top-level unknown keys are rejected). Mutually exclusive with 'message'.", Required: false},
					{Name: "message", Description: "Shortcut for args={\"message\":\"...\"} — use this for reminder.say and similar message-only actions instead of JSON-encoding args", Required: false},
					{Name: "notify_channel", Description: "OMIT this parameter in almost all cases. Defaults to the caller's current channel (works for Telegram, Slack, Discord, or any other channel identically — no channel-specific format is required). Only set this when the user explicitly asks to deliver results somewhere other than the current conversation.", Required: false},
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
					{Name: "args", Description: "JSON-encoded object passed as a string, e.g. args={\"issue_id\":\"XYZ\"}. Action-specific keys MUST go inside this object, NOT at top level (top-level unknown keys are rejected). Omit if using 'message'.", Required: false},
				},
			},
			{
				Name:        "update_job",
				Description: "Update time spec (interval or cron) or notify channel of a dynamic job. Setting interval clears cron and vice versa. Config-defined jobs cannot be updated.",
				Parameters: []orchestrator.Parameter{
					{Name: "name", Description: "Job name to update", Required: true},
					{Name: "interval", Description: "New interval (optional, mutually exclusive with cron)", Required: false},
					{Name: "cron", Description: "New cron expression (optional, mutually exclusive with interval)", Required: false},
					{Name: "notify_channel", Description: "New notify channel (optional). Setting this to the current channel name (e.g. 'telegram', 'slack') also refreshes the job's stored conversation id to the caller's current one.", Required: false},
				},
			},
		},
	}
}

func (t *SchedulerTool) Execute(ctx context.Context, call orchestrator.ToolCall) orchestrator.ToolResult {
	switch call.Action {
	case "create_job":
		return t.createJob(ctx, call)
	case "list_jobs":
		return t.listJobs(ctx, call)
	case "delete_job":
		return t.deleteJob(ctx, call)
	case "pause_job":
		return t.pauseJob(call)
	case "resume_job":
		return t.resumeJob(call)
	case "update_job":
		return t.updateJob(ctx, call)
	case "remind_me":
		return t.remindMe(ctx, call)
	default:
		return orchestrator.ToolResult{
			CallID: call.ID,
			Error:  fmt.Sprintf("unknown scheduler action: %s", call.Action),
		}
	}
}

// callerIdentity captures who is invoking a scheduler tool action. It is
// derived from context (profile system → actor fallback) rather than trusted
// from LLM-supplied args, because the LLM is free to invent user_id values
// and will (in practice) invent ones that don't match the real caller —
// making jobs disappear from list_jobs and from permission checks.
type callerIdentity struct {
	entityID       string // profile EntityID; empty when profile system is off
	group          string // profile Group; empty when off
	channelID      string // channel plugin id (e.g. "telegram") — where to deliver notifications
	conversationID string // specific chat/room on channelID — required for delivery
	userID         string // stable ID for approver/limit checks and CreatedBy
}

func resolveCaller(ctx context.Context) (callerIdentity, error) {
	convID := actor.ConversationID(ctx)
	if p := profile.FromContext(ctx); p != nil && p.EntityID != "" {
		return callerIdentity{
			entityID:       p.EntityID,
			group:          p.Group,
			channelID:      p.ChannelID,
			conversationID: convID,
			userID:         p.EntityID,
		}, nil
	}
	actorID := actor.Actor(ctx)
	if actorID == "" {
		return callerIdentity{}, fmt.Errorf("missing user context")
	}
	parts := strings.SplitN(actorID, ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return callerIdentity{}, fmt.Errorf("malformed actor %q, expected channel_id:sender_id", actorID)
	}
	return callerIdentity{
		channelID:      parts[0],
		conversationID: convID,
		userID:         parts[1],
	}, nil
}

func (t *SchedulerTool) createJob(ctx context.Context, call orchestrator.ToolCall) orchestrator.ToolResult {
	name := call.Args["name"]
	interval := call.Args["interval"]
	cronExpr := call.Args["cron"]
	action := call.Args["action"]
	notifyChannel := call.Args["notify_channel"]

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

	caller, err := resolveCaller(ctx)
	if err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: err.Error()}
	}
	if notifyChannel == "" {
		notifyChannel = caller.channelID
	}

	// 'message' is a shortcut for args={"message": ...}. Haiku-class models
	// routinely emit `message=...` at top level when scheduling reminder.say
	// (the schema of remind_me primes them for it). Without this shortcut the
	// stray arg is silently dropped, the job is persisted with empty args, and
	// it fails on first fire with "message is required".
	message := call.Args["message"]
	rawArgs := call.Args["args"]
	var args map[string]string
	switch {
	case message != "" && strings.TrimSpace(rawArgs) != "":
		return orchestrator.ToolResult{CallID: call.ID, Error: "'message' and 'args' are mutually exclusive — use one"}
	case message != "":
		args = map[string]string{"message": message}
	default:
		parsed, err := parseArgsField(rawArgs)
		if err != nil {
			return orchestrator.ToolResult{CallID: call.ID, Error: err.Error()}
		}
		args = parsed
	}

	job := Job{
		Name:                 name,
		Interval:             interval,
		Cron:                 cronExpr,
		Action:               action,
		Args:                 args,
		NotifyChannel:        notifyChannel,
		NotifyConversationID: caller.conversationID,
		EntityID:             caller.entityID,
		Group:                caller.group,
	}

	if err := t.sched.AddJob(job, caller.userID); err != nil {
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
		caller, err := resolveCaller(ctx)
		if err != nil {
			// No caller context: nothing to filter by.
			jobs = nil
		} else {
			jobs = t.sched.ListJobsForCaller(caller.entityID, caller.userID)
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

func (t *SchedulerTool) deleteJob(ctx context.Context, call orchestrator.ToolCall) orchestrator.ToolResult {
	name := call.Args["name"]
	if name == "" {
		return orchestrator.ToolResult{CallID: call.ID, Error: "name is required"}
	}
	caller, err := resolveCaller(ctx)
	if err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: err.Error()}
	}
	if err := t.sched.RemoveJob(name, caller.userID); err != nil {
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

func (t *SchedulerTool) updateJob(ctx context.Context, call orchestrator.ToolCall) orchestrator.ToolResult {
	name := call.Args["name"]
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

	caller, err := resolveCaller(ctx)
	if err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: err.Error()}
	}
	// If the caller is switching notify channels, the stored conversation id
	// belongs to the previous channel and is meaningless on the new one —
	// refresh it from the caller's current context (same rule as create_job).
	var notifyConversationID *string
	if notifyChannel != nil {
		cid := caller.conversationID
		notifyConversationID = &cid
	}
	if err := t.sched.UpdateJob(name, caller.userID, interval, cronExpr, notifyChannel, notifyConversationID); err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: err.Error()}
	}
	return orchestrator.ToolResult{
		CallID:  call.ID,
		Content: fmt.Sprintf("Job %q updated.", name),
	}
}

func (t *SchedulerTool) remindMe(ctx context.Context, call orchestrator.ToolCall) orchestrator.ToolResult {
	caller, err := resolveCaller(ctx)
	if err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: "remind_me: " + err.Error()}
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
		parsed, err := parseArgsField(call.Args["args"])
		if err != nil {
			return orchestrator.ToolResult{CallID: call.ID, Error: "remind_me: " + err.Error()}
		}
		args = parsed
	}

	// Sanitize caller.userID for job name (avoid ':' or spaces collisions).
	nameSafeSender := strings.NewReplacer(":", "_", " ", "_", "/", "_").Replace(caller.userID)
	// Nanosecond suffix guards against two reminders set in the same second.
	name := fmt.Sprintf("remind-%s-%d", nameSafeSender, time.Now().UnixNano())

	job := Job{
		Name:                 name,
		At:                   fireTime.UTC().Format(time.RFC3339),
		Action:               action,
		Args:                 args,
		NotifyChannel:        caller.channelID,
		NotifyConversationID: caller.conversationID,
		EntityID:             caller.entityID,
		Group:                caller.group,
	}
	if err := t.sched.AddPersonalJob(job, caller.userID); err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: err.Error()}
	}
	return orchestrator.ToolResult{
		CallID:  call.ID,
		Content: fmt.Sprintf("Reminder set for %s: will run %s.", job.At, action),
	}
}

// parseArgsField decodes the `args` tool parameter, which is expected as a
// JSON object serialized to a string (e.g. `{"issue_id":"XYZ"}`). It is
// intentionally lenient about empty or whitespace-only values because LLMs
// often pass those when they mean "no args". Any non-JSON value produces a
// detailed error that echoes the offending input so the model can self-correct.
func parseArgsField(raw string) (map[string]string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(trimmed), &args); err != nil {
		// Truncated or wrong format (e.g. Go's `map[k:v]` default stringification).
		// Surface the offending value so the model can recover on the next turn.
		return nil, fmt.Errorf("invalid args JSON %q: %w (expected a JSON object like {\"key\":\"value\"})", trimmed, err)
	}
	return args, nil
}
