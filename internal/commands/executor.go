package commands

import (
	"context"
	"fmt"
	"log/slog"
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

	ActionInstallSkill     = "install_skill"
	ActionShowConfig       = "show_config"
	ActionListCommands     = "list_commands"
	ActionSetPrompt        = "set_prompt"
	ActionClearSession     = "clear_session"
	ActionReloadMCP        = "reload_mcp"
	ActionSetDebugMode     = "set_debug_mode"
	ActionProfileAssign    = "profile_assign"
	ActionProfileRevoke    = "profile_revoke"
	ActionProfileListGroup = "profile_list_group"
)

// PluginReloader can reload a named plugin subprocess.
type PluginReloader interface {
	Reload(ctx context.Context, name string) error
}

// OnClearAction is a plugin action to call when the session is cleared.
type OnClearAction struct {
	Plugin string
	Action string
}

// GroupPluginManager manages group→plugin assignments (admin commands).
type GroupPluginManager interface {
	PluginsForGroup(ctx context.Context, groupID string) ([]string, error)
	UpsertGroupPlugins(ctx context.Context, groupID string, pluginIDs []string, source string) error
	RevokePlugin(ctx context.Context, groupID, pluginID string) error
}

// DebugEventCounter reports the per-session row count in ai_debug_events.
// Used by ActionSetDebugMode "status" replies so users can see whether
// capture is actually filling the table. Optional — when nil the status
// reply just reports on/off without a row count.
type DebugEventCounter interface {
	CountForSession(ctx context.Context, sessionID string) (int64, error)
}

// Executor runs built-in opentalon actions (install_skill, show_config, list_commands, set_prompt, clear_session, reload_mcp).
// It implements orchestrator.PluginExecutor.
type Executor struct {
	registry           *orchestrator.ToolRegistry
	sessions           orchestrator.SessionStoreInterface
	dataDir            string
	cfg                *config.Config
	runtimePromptPath  string
	pluginReloader     PluginReloader     // optional; enables reload_mcp
	mcpCacheDir        string             // optional; mcp-cache dir for cache invalidation on reload
	groupPluginManager GroupPluginManager // optional; enables profile_assign/revoke/list_group
	debugEventCounter  DebugEventCounter  // optional; populates "status" reply with row counts
	onClearActions     []OnClearAction
	runAction          func(ctx context.Context, plugin, action string, args map[string]string) (string, error)
}

// Capability returns the plugin capability for the opentalon built-in plugin.
func Capability() orchestrator.PluginCapability {
	return orchestrator.PluginCapability{
		Name:        PluginName,
		Description: "Built-in OpenTalon commands: install skill, show config, list commands, set prompt, clear session, reload MCP, profile management.",
		Actions: []orchestrator.Action{
			{Name: ActionInstallSkill, Description: "Install a skill from a GitHub URL (e.g. /install skill org/repo).", Parameters: []orchestrator.Parameter{{Name: "url", Description: "GitHub URL or org/repo", Required: true}, {Name: "ref", Description: "Branch or tag (default main)", Required: false}}, AuditLog: true, UserOnly: true},
			{Name: ActionShowConfig, Description: "Show current config (secrets redacted).", Parameters: nil},
			{Name: ActionListCommands, Description: "List available slash commands.", Parameters: nil},
			{Name: ActionSetPrompt, Description: "Set the editable runtime prompt.", Parameters: []orchestrator.Parameter{{Name: "text", Description: "Prompt text", Required: true}}},
			{Name: ActionClearSession, Description: "Clear the current session.", Parameters: nil, InjectContextArgs: []string{"session_id"}},
			{Name: ActionReloadMCP, Description: "Reload MCP server connections and refresh available tools. Optionally target one server by name.", Parameters: []orchestrator.Parameter{{Name: "server", Description: "MCP server name to reload (leave empty to reload all)", Required: false}}},
			{Name: ActionSetDebugMode, Description: "Toggle per-session deep debug logging (the user-facing /debug command). When on, raw LLM HTTP request and response bodies for this session are emitted on stderr at INFO level and persisted to the ai_debug_events table for 30 days. Use to capture the full prompt/response pair when diagnosing why the model produced a wrong answer. Other sessions are unaffected. Persistence requires the state store to be configured.", Parameters: []orchestrator.Parameter{{Name: "mode", Description: "on, off, toggle (default), or status", Required: false}}, InjectContextArgs: []string{"session_id"}},
			{Name: ActionProfileAssign, Description: "Assign a plugin to a profile group (admin). Source is set to 'admin' and cannot be overwritten by WhoAmI.", Parameters: []orchestrator.Parameter{{Name: "group", Description: "Group name", Required: true}, {Name: "plugin", Description: "Plugin ID", Required: true}}, AuditLog: true, UserOnly: true},
			{Name: ActionProfileRevoke, Description: "Revoke a plugin from a profile group (admin).", Parameters: []orchestrator.Parameter{{Name: "group", Description: "Group name", Required: true}, {Name: "plugin", Description: "Plugin ID", Required: true}}, AuditLog: true, UserOnly: true},
			{Name: ActionProfileListGroup, Description: "List plugins assigned to a profile group.", Parameters: []orchestrator.Parameter{{Name: "group", Description: "Group name", Required: true}}, UserOnly: true},
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

// WithOnClear configures plugin actions to run when a session is cleared.
// runAction is the function used to execute plugin actions (typically wired from the orchestrator).
func (e *Executor) WithOnClear(actions []OnClearAction, runAction func(ctx context.Context, plugin, action string, args map[string]string) (string, error)) *Executor {
	e.onClearActions = actions
	e.runAction = runAction
	return e
}

// WithProfileStore enables profile admin commands (profile_assign, profile_revoke, profile_list_group).
func (e *Executor) WithProfileStore(m GroupPluginManager) *Executor {
	e.groupPluginManager = m
	return e
}

// WithDebugEventCounter wires the row-count source for set_debug_mode status
// replies. Optional — without it the status reply still works but skips the
// row-count line.
func (e *Executor) WithDebugEventCounter(c DebugEventCounter) *Executor {
	e.debugEventCounter = c
	return e
}

// Execute implements orchestrator.PluginExecutor.
func (e *Executor) Execute(ctx context.Context, call orchestrator.ToolCall) orchestrator.ToolResult {
	switch call.Action {
	case ActionInstallSkill:
		return e.installSkill(ctx, call)
	case ActionShowConfig:
		return e.showConfig(call)
	case ActionListCommands:
		return e.listCommands(call)
	case ActionSetPrompt:
		return e.setPrompt(call)
	case ActionClearSession:
		return e.clearSession(ctx, call)
	case ActionReloadMCP:
		return e.reloadMCP(ctx, call)
	case ActionSetDebugMode:
		return e.setDebugMode(ctx, call)
	case ActionProfileAssign:
		return e.profileAssign(ctx, call)
	case ActionProfileRevoke:
		return e.profileRevoke(ctx, call)
	case ActionProfileListGroup:
		return e.profileListGroup(ctx, call)
	default:
		return orchestrator.ToolResult{
			CallID: call.ID,
			Error:  fmt.Sprintf("unknown action %q", call.Action),
		}
	}
}

func (e *Executor) profileAssign(ctx context.Context, call orchestrator.ToolCall) orchestrator.ToolResult {
	if e.groupPluginManager == nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: "profile store not configured"}
	}
	group := strings.TrimSpace(call.Args["group"])
	plug := strings.TrimSpace(call.Args["plugin"])
	if group == "" || plug == "" {
		return orchestrator.ToolResult{CallID: call.ID, Error: "group and plugin are required"}
	}
	if err := e.groupPluginManager.UpsertGroupPlugins(ctx, group, []string{plug}, "admin"); err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: fmt.Sprintf("assign failed: %v", err)}
	}
	return orchestrator.ToolResult{CallID: call.ID, Content: fmt.Sprintf("Plugin %q assigned to group %q.", plug, group)}
}

func (e *Executor) profileRevoke(ctx context.Context, call orchestrator.ToolCall) orchestrator.ToolResult {
	if e.groupPluginManager == nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: "profile store not configured"}
	}
	group := strings.TrimSpace(call.Args["group"])
	plug := strings.TrimSpace(call.Args["plugin"])
	if group == "" || plug == "" {
		return orchestrator.ToolResult{CallID: call.ID, Error: "group and plugin are required"}
	}
	if err := e.groupPluginManager.RevokePlugin(ctx, group, plug); err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: fmt.Sprintf("revoke failed: %v", err)}
	}
	return orchestrator.ToolResult{CallID: call.ID, Content: fmt.Sprintf("Plugin %q revoked from group %q.", plug, group)}
}

func (e *Executor) profileListGroup(ctx context.Context, call orchestrator.ToolCall) orchestrator.ToolResult {
	if e.groupPluginManager == nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: "profile store not configured"}
	}
	group := strings.TrimSpace(call.Args["group"])
	if group == "" {
		return orchestrator.ToolResult{CallID: call.ID, Error: "group is required"}
	}
	plugins, err := e.groupPluginManager.PluginsForGroup(ctx, group)
	if err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: fmt.Sprintf("list failed: %v", err)}
	}
	if len(plugins) == 0 {
		return orchestrator.ToolResult{CallID: call.ID, Content: fmt.Sprintf("Group %q has no plugins assigned.", group)}
	}
	return orchestrator.ToolResult{CallID: call.ID, Content: fmt.Sprintf("Group %q plugins: %s", group, strings.Join(plugins, ", "))}
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
/reload mcp [server] — Reload MCP server connections and refresh available tools. Optionally name a specific server (e.g. /reload mcp magtuner).
/debug [on|off|status] — Toggle per-session deep debug logging. With no arg the flag toggles. Captured raw LLM HTTP bodies stay in ai_debug_events for 30 days.`
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

// setDebugMode toggles per-session deep debug capture. The flag lives in
// session.Metadata["debug"]; orchestrator.Run() reads it on every turn and
// promotes the slog level + tees raw HTTP bodies into the ai_debug_events
// table accordingly. Persistence requires the state store to be configured —
// without it the toggle still works but only the slog promotion takes
// effect.
func (e *Executor) setDebugMode(ctx context.Context, call orchestrator.ToolCall) orchestrator.ToolResult {
	sessionID := call.Args["session_id"]
	if sessionID == "" {
		return orchestrator.ToolResult{CallID: call.ID, Error: "session_id not set (internal error)"}
	}

	mode := strings.ToLower(strings.TrimSpace(call.Args["mode"]))
	if mode == "" {
		mode = "toggle"
	}

	sess, err := e.sessions.Get(sessionID)
	if err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: fmt.Sprintf("session lookup: %v", err)}
	}
	current := sess != nil && sess.Metadata["debug"] == "true"

	var enable bool
	switch mode {
	case "on", "enable", "true":
		enable = true
	case "off", "disable", "false":
		enable = false
	case "status":
		return e.debugStatusReply(ctx, call.ID, sessionID, current)
	case "toggle":
		enable = !current
	default:
		return orchestrator.ToolResult{CallID: call.ID, Error: fmt.Sprintf("unknown mode %q (expected on, off, toggle, or status)", mode)}
	}

	if enable == current {
		return e.debugStatusReply(ctx, call.ID, sessionID, current)
	}

	value := ""
	if enable {
		value = "true"
	}
	if err := e.sessions.SetMetadata(sessionID, "debug", value); err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: fmt.Sprintf("persist debug flag: %v", err)}
	}

	// Reply is honest about persistence: the counter being non-nil is the
	// proxy for "state store + async writer are wired", so sessions that
	// only get console promotion say so explicitly instead of promising a
	// table that's not being written.
	persistenceWired := e.debugEventCounter != nil
	if enable {
		body := "Debug mode ON. The next LLM exchange will be logged at INFO level on stderr"
		if persistenceWired {
			body += " and persisted to the ai_debug_events table."
		} else {
			body += " (no state store configured, so events are not persisted)."
		}
		return orchestrator.ToolResult{CallID: call.ID, Content: body + " Send /debug off to stop."}
	}
	body := "Debug mode OFF."
	if persistenceWired {
		body += " Already-captured events stay in ai_debug_events until they age out."
	}
	return orchestrator.ToolResult{CallID: call.ID, Content: body}
}

func (e *Executor) debugStatusReply(ctx context.Context, callID, sessionID string, on bool) orchestrator.ToolResult {
	state := "OFF"
	if on {
		state = "ON"
	}
	msg := "Debug mode " + state + "."
	if e.debugEventCounter != nil {
		if n, err := e.debugEventCounter.CountForSession(ctx, sessionID); err == nil {
			msg = fmt.Sprintf("Debug mode %s. %d events captured for this session so far.", state, n)
		}
	} else {
		msg += " (Persistence disabled — no state store configured.)"
	}
	return orchestrator.ToolResult{CallID: callID, Content: msg}
}

func (e *Executor) clearSession(ctx context.Context, call orchestrator.ToolCall) orchestrator.ToolResult {
	sessionID := call.Args["session_id"]
	if sessionID == "" {
		return orchestrator.ToolResult{CallID: call.ID, Error: "session_id not set (internal error)"}
	}
	// Drop messages + summary in one transaction. The session row itself
	// stays (entity_id, group_id, active_model, metadata preserved), and
	// session_events is untouched — see SessionStoreInterface.ClearMessages.
	// TODO: If the orchestrator maintains pending pipeline state per
	// session, it should be cleared here too (e.g. via a ClearSession hook).
	if err := e.sessions.ClearMessages(sessionID); err != nil {
		return orchestrator.ToolResult{CallID: call.ID, Error: fmt.Sprintf("clear session: %v", err)}
	}

	// Run configured on-clear plugin actions (e.g. weaviate refresh).
	if e.runAction != nil {
		for _, a := range e.onClearActions {
			if _, err := e.runAction(ctx, a.Plugin, a.Action, nil); err != nil {
				slog.Warn("on-clear action failed", "plugin", a.Plugin, "action", a.Action, "error", err)
			}
		}
	}

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
