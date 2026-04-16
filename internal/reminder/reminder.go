// Package reminder provides a trivial built-in plugin whose "say" action
// echoes a literal message. It exists so the scheduler's remind_me tool can
// schedule a one-shot reminder that simply delivers text back to the user
// without depending on any external plugin.
package reminder

import (
	"context"
	"fmt"

	"github.com/opentalon/opentalon/internal/orchestrator"
)

const ToolName = "reminder"

// Tool is the built-in reminder plugin.
type Tool struct{}

func NewTool() *Tool { return &Tool{} }

func Capability() orchestrator.PluginCapability {
	return orchestrator.PluginCapability{
		Name:        ToolName,
		Description: "Deliver a literal text message to the user. Use this as the 'action' when scheduling a recurring or one-shot delivery of static text (e.g. a quote, a reminder, a status ping) via scheduler.create_job or scheduler.remind_me.",
		Actions: []orchestrator.Action{
			{
				Name:        "say",
				Description: "Deliver the given text to the user verbatim. Pair with scheduler.create_job (interval or cron) for recurring messages, or with scheduler.remind_me for a single future message. The scheduler routes the result back to the caller's current channel and conversation automatically.",
				Parameters: []orchestrator.Parameter{
					{Name: "message", Description: "The text to deliver", Required: true},
				},
			},
		},
	}
}

func (t *Tool) Execute(_ context.Context, call orchestrator.ToolCall) orchestrator.ToolResult {
	if call.Action != "say" {
		return orchestrator.ToolResult{CallID: call.ID, Error: fmt.Sprintf("unknown reminder action: %s", call.Action)}
	}
	msg := call.Args["message"]
	if msg == "" {
		return orchestrator.ToolResult{CallID: call.ID, Error: "message is required"}
	}
	return orchestrator.ToolResult{CallID: call.ID, Content: msg}
}
