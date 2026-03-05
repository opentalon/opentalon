package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/opentalon/opentalon/internal/bundle"
	"github.com/opentalon/opentalon/internal/config"
	"github.com/opentalon/opentalon/internal/orchestrator"
	"github.com/opentalon/opentalon/internal/requestpkg"
	"gopkg.in/yaml.v3"
)

const (
	PluginName = "opentalon"

	ActionInstallSkill  = "install_skill"
	ActionShowConfig    = "show_config"
	ActionListCommands  = "list_commands"
	ActionSetPrompt     = "set_prompt"
	ActionClearSession  = "clear_session"
)

// Executor runs built-in opentalon actions (install_skill, show_config, list_commands, set_prompt, clear_session).
// It implements orchestrator.PluginExecutor.
type Executor struct {
	registry          *orchestrator.ToolRegistry
	sessions          orchestrator.SessionStoreInterface
	dataDir           string
	cfg               *config.Config
	runtimePromptPath string
}

// Capability returns the plugin capability for the opentalon built-in plugin.
func Capability() orchestrator.PluginCapability {
	return orchestrator.PluginCapability{
		Name:        PluginName,
		Description: "Built-in OpenTalon commands: install skill, show config, list commands, set prompt, clear session.",
		Actions: []orchestrator.Action{
			{Name: ActionInstallSkill, Description: "Install a skill from a GitHub URL (e.g. /install skill org/repo).", Parameters: []orchestrator.Parameter{{Name: "url", Description: "GitHub URL or org/repo", Required: true}, {Name: "ref", Description: "Branch or tag (default main)", Required: false}}},
			{Name: ActionShowConfig, Description: "Show current config (secrets redacted).", Parameters: nil},
			{Name: ActionListCommands, Description: "List available slash commands.", Parameters: nil},
			{Name: ActionSetPrompt, Description: "Set the editable runtime prompt.", Parameters: []orchestrator.Parameter{{Name: "text", Description: "Prompt text", Required: true}}},
			{Name: ActionClearSession, Description: "Clear the current session.", Parameters: nil, InjectContextArgs: []string{"session_id"}},
		},
	}
}

// NewExecutor builds the opentalon command executor.
func NewExecutor(
	registry *orchestrator.ToolRegistry,
	sessions orchestrator.SessionStoreInterface,
	dataDir string,
	cfg *config.Config,
	runtimePromptPath string,
) *Executor {
	return &Executor{
		registry:          registry,
		sessions:          sessions,
		dataDir:           dataDir,
		cfg:               cfg,
		runtimePromptPath: runtimePromptPath,
	}
}

// Execute implements orchestrator.PluginExecutor.
func (e *Executor) Execute(call orchestrator.ToolCall) orchestrator.ToolResult {
	switch call.Action {
	case ActionInstallSkill:
		return e.installSkill(context.Background(), call)
	case ActionShowConfig:
		return e.showConfig(call)
	case ActionListCommands:
		return e.listCommands(call)
	case ActionSetPrompt:
		return e.setPrompt(call)
	case ActionClearSession:
		return e.clearSession(call)
	default:
		return orchestrator.ToolResult{
			CallID: call.ID,
			Error:  fmt.Sprintf("unknown action %q", call.Action),
		}
	}
}

func (e *Executor) installSkill(ctx context.Context, call orchestrator.ToolCall) orchestrator.ToolResult {
	url := strings.TrimSpace(call.Args["url"])
	if url == "" {
		return orchestrator.ToolResult{CallID: call.ID, Error: "missing url"}
	}
	ref := strings.TrimSpace(call.Args["ref"])
	if ref == "" {
		ref = "main"
	}

	github, name := parseInstallURL(url)
	if github == "" || name == "" {
		return orchestrator.ToolResult{CallID: call.ID, Error: "could not parse url: use https://github.com/org/repo or org/repo"}
	}

	skillDir, err := bundle.EnsureSkillDir(ctx, e.dataDir, name, github, ref)
	if err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: fmt.Sprintf("ensure skill dir: %v", err)}
	}

	set, err := requestpkg.LoadSkillDir(skillDir)
	if err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: fmt.Sprintf("load skill: %v", err)}
	}

	if err := requestpkg.Register(e.registry, []requestpkg.Set{set}); err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: fmt.Sprintf("register skill: %v", err)}
	}

	if err := config.AppendInstalledSkill(e.dataDir, config.SkillEntry{Name: name, GitHub: github, Ref: ref}); err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: fmt.Sprintf("persist installed skills: %v", err)}
	}

	return orchestrator.ToolResult{
		CallID:  call.ID,
		Content: fmt.Sprintf("Installed skill %q. It is available immediately.", name),
	}
}

func parseInstallURL(url string) (github, name string) {
	url = strings.TrimSpace(url)
	// https://github.com/org/repo or https://github.com/org/repo.git
	if strings.HasPrefix(url, "https://github.com/") {
		rest := strings.TrimPrefix(url, "https://github.com/")
		rest = strings.TrimSuffix(rest, ".git")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) == 2 {
			return parts[0] + "/" + parts[1], parts[1]
		}
		return "", ""
	}
	if strings.HasPrefix(url, "git@github.com:") {
		rest := strings.TrimPrefix(url, "git@github.com:")
		rest = strings.TrimSuffix(rest, ".git")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) == 2 {
			return parts[0] + "/" + parts[1], parts[1]
		}
		return "", ""
	}
	// org/repo
	if strings.Contains(url, "/") {
		parts := strings.SplitN(url, "/", 2)
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			name = parts[1]
			return parts[0] + "/" + parts[1], name
		}
	}
	return "", ""
}

func (e *Executor) showConfig(_ orchestrator.ToolCall) orchestrator.ToolResult {
	if e.cfg == nil {
		return orchestrator.ToolResult{Content: "(config not available)"}
	}
	// Redact secrets for display
	redacted := redactConfig(e.cfg)
	data, err := yaml.Marshal(redacted)
	if err != nil {
		return orchestrator.ToolResult{Error: fmt.Sprintf("marshal config: %v", err)}
	}
	return orchestrator.ToolResult{Content: string(data)}
}

func (e *Executor) listCommands(_ orchestrator.ToolCall) orchestrator.ToolResult {
	const msg = `/install skill <url> [ref] — Install a skill from a GitHub URL (or org/repo). Optional ref defaults to main.
/show config — Show current config (secrets redacted).
/commands — List available commands (this message).
/set prompt <text> — Set the editable runtime prompt; applies to the next message.
/clear or /new — Clear the current session.`
	return orchestrator.ToolResult{Content: msg}
}

func (e *Executor) setPrompt(call orchestrator.ToolCall) orchestrator.ToolResult {
	text := strings.TrimSpace(call.Args["text"])
	if e.runtimePromptPath == "" {
		return orchestrator.ToolResult{CallID: call.ID, Error: "runtime prompt path not configured"}
	}
	dir := filepath.Dir(e.runtimePromptPath)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: fmt.Sprintf("create dir: %v", err)}
	}
	if err := os.WriteFile(e.runtimePromptPath, []byte(text), 0600); err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: fmt.Sprintf("write prompt file: %v", err)}
	}
	return orchestrator.ToolResult{
		CallID:  call.ID,
		Content: "Prompt updated. It will apply to the next message.",
	}
}

func (e *Executor) clearSession(call orchestrator.ToolCall) orchestrator.ToolResult {
	sessionID := call.Args["session_id"]
	if sessionID == "" {
		return orchestrator.ToolResult{CallID: call.ID, Error: "session_id not set (internal error)"}
	}
	if err := e.sessions.Delete(sessionID); err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: fmt.Sprintf("delete session: %v", err)}
	}
	e.sessions.Create(sessionID)
	return orchestrator.ToolResult{
		CallID:  call.ID,
		Content: "Session cleared.",
	}
}

// redactConfig returns a copy of config with API keys and secrets redacted for display.
func redactConfig(c *config.Config) *config.Config {
	if c == nil {
		return nil
	}
	out := *c
	if out.Models.Providers == nil {
		return &out
	}
	provs := make(map[string]config.ProviderConfig)
	for name, p := range out.Models.Providers {
		p2 := p
		if p2.APIKey != "" {
			p2.APIKey = "[redacted]"
		}
		provs[name] = p2
	}
	out.Models.Providers = provs
	return &out
}
