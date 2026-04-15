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
		Description: "Deliver a literal text reminder back to the user. Used internally by the scheduler's remind_me action.",
		Actions: []orchestrator.Action{
			{
				Name:        "say",
				Description: "Return the provided message verbatim.",
				Parameters: []orchestrator.Parameter{
					{Name: "message", Description: "The message to deliver", Required: true},
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
