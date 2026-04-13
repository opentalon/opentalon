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
// UserOnly, when true, hides the action from the LLM system prompt and blocks it from being called via LLM-generated tool calls; it can only be invoked directly by the user (e.g. via RunAction).
type Action struct {
	Name              string      `yaml:"name"`
	Description       string      `yaml:"description"`
	Parameters        []Parameter `yaml:"parameters,omitempty"`
	InjectContextArgs []string    `yaml:"inject_context_args,omitempty"`
	AuditLog          bool        `yaml:"audit_log,omitempty"` // if true, log invocation for audit
	UserOnly          bool        `yaml:"user_only,omitempty"` // if true, hidden from LLM and blocked from LLM-sourced calls
}

type PluginCapability struct {
	Name                 string   `yaml:"name"`
	Description          string   `yaml:"description"`
	Actions              []Action `yaml:"actions"`
	AllowedGroups        []string `yaml:"allowed_groups,omitempty"`         // empty = unrestricted; when set, only listed groups can use this plugin
	SystemPromptAddition string   `yaml:"system_prompt_addition,omitempty"` // optional text appended to LLM system prompt when this plugin is loaded
}

type ToolCall struct {
	ID      string            `yaml:"id"`
	Plugin  string            `yaml:"plugin"`
	Action  string            `yaml:"action"`
	Args    map[string]string `yaml:"args,omitempty"`
	FromLLM bool              `yaml:"-"` // yaml:"-": must not be deserializable — prevents LLM from spoofing user-origin calls via YAML injection
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
