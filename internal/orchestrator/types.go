package orchestrator

type Parameter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Required    bool   `yaml:"required"`
}

type Action struct {
	Name        string      `yaml:"name"`
	Description string      `yaml:"description"`
	Parameters  []Parameter `yaml:"parameters,omitempty"`
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
