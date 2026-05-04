package plugin

import (
	"context"
	"log/slog"
	"time"

	"github.com/opentalon/opentalon/internal/orchestrator"
	"github.com/opentalon/opentalon/proto/pluginpb"
)

// RunNotifier implements orchestrator.RunCompleteNotifier by broadcasting
// events to all loaded plugin clients via OnRunComplete.
type RunNotifier struct {
	manager *Manager
}

// NewRunNotifier creates a notifier backed by the given plugin manager.
func NewRunNotifier(manager *Manager) *RunNotifier {
	return &RunNotifier{manager: manager}
}

// NotifyRunComplete sends the run-complete event to all loaded plugins.
// Each call is made with a short timeout; failures are logged but do not block.
func (n *RunNotifier) NotifyRunComplete(ctx context.Context, event orchestrator.RunCompleteEvent) {
	pbEvent := eventToProto(event)
	for _, client := range n.manager.Clients() {
		c := client // capture
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := c.OnRunComplete(callCtx, pbEvent); err != nil {
			slog.Warn("run complete notification failed", "plugin", c.Name(), "error", err)
		}
		cancel()
	}
}

func eventToProto(e orchestrator.RunCompleteEvent) *pluginpb.RunCompleteEvent {
	entries := make([]*pluginpb.ToolCallEntry, len(e.ToolCalls))
	for i, tc := range e.ToolCalls {
		var resultContent, resultError string
		if i < len(e.Results) {
			resultContent = e.Results[i].Content
			resultError = e.Results[i].Error
		}
		entries[i] = &pluginpb.ToolCallEntry{
			Plugin:        tc.Plugin,
			Action:        tc.Action,
			Args:          tc.Args,
			ResultContent: resultContent,
			ResultError:   resultError,
		}
	}
	return &pluginpb.RunCompleteEvent{
		SessionId:   e.SessionID,
		ActorId:     e.ActorID,
		UserMessage: e.UserMessage,
		Response:    e.Response,
		ToolCalls:   entries,
	}
}
