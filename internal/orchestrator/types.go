package orchestrator

type Parameter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Required    bool   `yaml:"required"`
}

// Action describes one action a plugin supports.
// InjectContextArgs lists context arg names to inject from the request context (e.g. "session_id")
// before calling the executor. The orchestrator resolves them via ContextArgProviders.
// AuditLog, when true, causes the orchestrator to log each invocation (actor, plugin, action, args) for audit; no plugin or action names are hardcoded in the core.
type Action struct {
	Name              string      `yaml:"name"`
	Description       string      `yaml:"description"`
	Parameters        []Parameter `yaml:"parameters,omitempty"`
	InjectContextArgs []string    `yaml:"inject_context_args,omitempty"`
	AuditLog          bool        `yaml:"audit_log,omitempty"` // if true, log invocation for audit
}

type PluginCapability struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Actions     []Action `yaml:"actions"`
}

type ToolCall struct {
	ID     string            `yaml:"id"`
	Plugin string            `yaml:"plugin"`
	Action string            `yaml:"action"`
	Args   map[string]string `yaml:"args,omitempty"`
}

type ToolResult struct {
	CallID  string `yaml:"call_id"`
	Content string `yaml:"content"`
	Error   string `yaml:"error,omitempty"`
}

type WorkflowStep struct {
	Plugin string `yaml:"plugin"`
	Action string `yaml:"action"`
	Order  int    `yaml:"order"`
}

type Workflow struct {
	Trigger string         `yaml:"trigger"`
	Steps   []WorkflowStep `yaml:"steps"`
	Outcome string         `yaml:"outcome"`
}
