package plugin

// RunObserver may be implemented by a Handler to receive post-run events from
// the orchestrator. When the host completes a run that involved tool calls, it
// calls OnRunComplete on every plugin whose Handler implements this interface.
// Plugins that do not implement it are silently skipped.
type RunObserver interface {
	OnRunComplete(event RunCompleteEvent)
}

// RunCompleteEvent describes a completed orchestrator run.
type RunCompleteEvent struct {
	SessionID   string
	ActorID     string
	UserMessage string
	Response    string
	ToolCalls   []ToolCallEntry
}

// ToolCallEntry is one tool call + result pair from a completed run.
type ToolCallEntry struct {
	Plugin        string
	Action        string
	Args          map[string]string
	ResultContent string
	ResultError   string
}
