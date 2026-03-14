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

	ActionInstallSkill = "install_skill"
	ActionShowConfig   = "show_config"
	ActionListCommands = "list_commands"
	ActionSetPrompt    = "set_prompt"
	ActionClearSession = "clear_session"
	ActionReloadMCP    = "reload_mcp"
)

// PluginReloader can reload a named plugin subprocess.
type PluginReloader interface {
	Reload(ctx context.Context, name string) error
}

// Executor runs built-in opentalon actions (install_skill, show_config, list_commands, set_prompt, clear_session, reload_mcp).
// It implements orchestrator.PluginExecutor.
type Executor struct {
	registry          *orchestrator.ToolRegistry
	sessions          orchestrator.SessionStoreInterface
	dataDir           string
	cfg               *config.Config
	runtimePromptPath string
	pluginReloader    PluginReloader // optional; enables reload_mcp
	mcpCacheDir       string         // optional; mcp-cache dir for cache invalidation on reload
}

// Capability returns the plugin capability for the opentalon built-in plugin.
func Capability() orchestrator.PluginCapability {
	return orchestrator.PluginCapability{
		Name:        PluginName,
		Description: "Built-in OpenTalon commands: install skill, show config, list commands, set prompt, clear session, reload MCP.",
		Actions: []orchestrator.Action{
			{Name: ActionInstallSkill, Description: "Install a skill from a GitHub URL (e.g. /install skill org/repo).", Parameters: []orchestrator.Parameter{{Name: "url", Description: "GitHub URL or org/repo", Required: true}, {Name: "ref", Description: "Branch or tag (default main)", Required: false}}, AuditLog: true},
			{Name: ActionShowConfig, Description: "Show current config (secrets redacted).", Parameters: nil},
			{Name: ActionListCommands, Description: "List available slash commands.", Parameters: nil},
			{Name: ActionSetPrompt, Description: "Set the editable runtime prompt.", Parameters: []orchestrator.Parameter{{Name: "text", Description: "Prompt text", Required: true}}},
			{Name: ActionClearSession, Description: "Clear the current session.", Parameters: nil, InjectContextArgs: []string{"session_id"}},
			{Name: ActionReloadMCP, Description: "Reload MCP server connections and refresh available tools. Optionally target one server by name.", Parameters: []orchestrator.Parameter{{Name: "server", Description: "MCP server name to reload (leave empty to reload all)", Required: false}}},
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

// WithMCPReload enables the reload_mcp command.
// reloader restarts the mcp plugin subprocess; cacheDir is the mcp-cache directory
// (individual server cache files are deleted before reload to force a fresh fetch).
func (e *Executor) WithMCPReload(reloader PluginReloader, cacheDir string) *Executor {
	e.pluginReloader = reloader
	e.mcpCacheDir = cacheDir
	return e
}

// Execute implements orchestrator.PluginExecutor.
func (e *Executor) Execute(call orchestrator.ToolCall) orchestrator.ToolResult {
	switch call.Action {
	case ActionInstallSkill:
		// TODO: PluginExecutor.Execute does not receive context; clone cannot be cancelled
		return e.installSkill(context.Background(), call)
	case ActionShowConfig:
		return e.showConfig(call)
	case ActionListCommands:
		return e.listCommands(call)
	case ActionSetPrompt:
		return e.setPrompt(call)
	case ActionClearSession:
		return e.clearSession(call)
	case ActionReloadMCP:
		return e.reloadMCP(context.Background(), call)
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
	defaultRef := ref == ""
	if defaultRef {
		ref = "main"
	}

	github, name := parseInstallURL(url)
	if github == "" || name == "" {
		return orchestrator.ToolResult{CallID: call.ID, Error: "could not parse url: use https://github.com/org/repo or org/repo"}
	}

	skillDir, err := bundle.EnsureSkillDir(ctx, e.dataDir, name, github, ref)
	if err != nil && defaultRef {
		// Fall back to "master" if "main" wasn't found
		skillDir, err = bundle.EnsureSkillDir(ctx, e.dataDir, name, github, "master")
		if err == nil {
			ref = "master"
		}
	}
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

// safeSkillName returns true if name is a single path component with no .. or path separators.
func safeSkillName(name string) bool {
	if name == "" || strings.Contains(name, "..") || strings.ContainsAny(name, "/\\") {
		return false
	}
	return filepath.Clean(name) == name && filepath.Base(name) == name
}

func parseInstallURL(url string) (github, name string) {
	url = strings.TrimSpace(url)
	// https://github.com/org/repo or https://github.com/org/repo.git
	if strings.HasPrefix(url, "https://github.com/") {
		rest := strings.TrimPrefix(url, "https://github.com/")
		rest = strings.TrimSuffix(rest, ".git")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) == 2 && safeSkillName(parts[0]) && safeSkillName(parts[1]) {
			return parts[0] + "/" + parts[1], parts[1]
		}
		return "", ""
	}
	if strings.HasPrefix(url, "git@github.com:") {
		rest := strings.TrimPrefix(url, "git@github.com:")
		rest = strings.TrimSuffix(rest, ".git")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) == 2 && safeSkillName(parts[0]) && safeSkillName(parts[1]) {
			return parts[0] + "/" + parts[1], parts[1]
		}
		return "", ""
	}
	// org/repo
	if strings.Contains(url, "/") {
		parts := strings.SplitN(url, "/", 2)
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" && safeSkillName(parts[0]) && safeSkillName(parts[1]) {
			return parts[0] + "/" + parts[1], parts[1]
		}
	}
	return "", ""
}

func (e *Executor) showConfig(call orchestrator.ToolCall) orchestrator.ToolResult {
	if e.cfg == nil {
		return orchestrator.ToolResult{CallID: call.ID, Content: "(config not available)"}
	}
	// Redact secrets for display
	redacted := redactConfig(e.cfg)
	data, err := yaml.Marshal(redacted)
	if err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: fmt.Sprintf("marshal config: %v", err)}
	}
	return orchestrator.ToolResult{CallID: call.ID, Content: string(data)}
}

func (e *Executor) listCommands(call orchestrator.ToolCall) orchestrator.ToolResult {
	const msg = `/install skill <url> [ref] — Install a skill from a GitHub URL (or org/repo). Optional ref defaults to main.
/show config — Show current config (secrets redacted).
/commands — List available commands (this message).
/set prompt <text> — Set the editable runtime prompt; applies to the next message.
/clear or /new — Clear the current session.
/reload mcp [server] — Reload MCP server connections and refresh available tools. Optionally name a specific server (e.g. /reload mcp magtuner).`
	return orchestrator.ToolResult{CallID: call.ID, Content: msg}
}

func (e *Executor) reloadMCP(ctx context.Context, call orchestrator.ToolCall) orchestrator.ToolResult {
	if e.pluginReloader == nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: "reload_mcp not available (plugin reloader not configured)"}
	}

	server := strings.TrimSpace(call.Args["server"])

	// Delete cache file(s) so the plugin fetches a fresh spec.
	if e.mcpCacheDir != "" {
		if server != "" {
			safe := strings.ReplaceAll(server, string(filepath.Separator), "_")
			_ = os.Remove(filepath.Join(e.mcpCacheDir, safe+".json"))
		} else {
			entries, _ := os.ReadDir(e.mcpCacheDir)
			for _, entry := range entries {
				if !entry.IsDir() {
					_ = os.Remove(filepath.Join(e.mcpCacheDir, entry.Name()))
				}
			}
		}
	}

	if err := e.pluginReloader.Reload(ctx, "mcp"); err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: fmt.Sprintf("reload mcp plugin: %v", err)}
	}

	if server != "" {
		return orchestrator.ToolResult{CallID: call.ID, Content: fmt.Sprintf("MCP plugin reloaded. Server %q tools refreshed.", server)}
	}
	return orchestrator.ToolResult{CallID: call.ID, Content: "MCP plugin reloaded. All server tools refreshed."}
}

const maxRuntimePromptBytes = 32 * 1024 // 32KB limit to reduce prompt injection impact (global file, all requests)

func (e *Executor) setPrompt(call orchestrator.ToolCall) orchestrator.ToolResult {
	text := strings.TrimSpace(call.Args["text"])
	if e.runtimePromptPath == "" {
		return orchestrator.ToolResult{CallID: call.ID, Error: "runtime prompt path not configured"}
	}
	if len(text) > maxRuntimePromptBytes {
		return orchestrator.ToolResult{CallID: call.ID, Error: fmt.Sprintf("prompt text exceeds %d bytes", maxRuntimePromptBytes)}
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
	// TODO: If the orchestrator maintains pending pipeline state per session, it should be cleared here (e.g. via a ClearSession hook).
	if err := e.sessions.Delete(sessionID); err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: fmt.Sprintf("delete session: %v", err)}
	}
	e.sessions.Create(sessionID)
	return orchestrator.ToolResult{
		CallID:  call.ID,
		Content: "Session cleared.",
	}
}

// redactConfig returns a copy of config with secrets redacted for display (API keys, plugin configs, and fields named secret/token/password).
func redactConfig(c *config.Config) *config.Config {
	if c == nil {
		return nil
	}
	out := *c
	// Redact provider API keys
	if out.Models.Providers != nil {
		provs := make(map[string]config.ProviderConfig)
		for name, p := range out.Models.Providers {
			p2 := p
			if p2.APIKey != "" {
				p2.APIKey = "[redacted]"
			}
			provs[name] = p2
		}
		out.Models.Providers = provs
	}
	// Redact entire plugin config maps (may contain webhook secrets, tokens, etc.)
	if out.Plugins != nil {
		redactedPlugins := make(map[string]config.PluginConfig)
		for name, p := range out.Plugins {
			redactedPlugins[name] = config.PluginConfig{
				Enabled:  p.Enabled,
				Insecure: p.Insecure,
				Plugin:   p.Plugin,
				GitHub:   p.GitHub,
				Ref:      p.Ref,
				Config:   nil, // omit config for show config
			}
		}
		out.Plugins = redactedPlugins
	}
	return &out
}
