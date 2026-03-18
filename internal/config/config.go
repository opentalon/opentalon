package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Models          ModelsConfig             `yaml:"models"`
	Routing         RoutingConfig            `yaml:"routing"`
	Auth            AuthConfig               `yaml:"auth"`
	State           StateConfig              `yaml:"state"`
	Log             LogConfig                `yaml:"log"`
	Orchestrator    OrchestratorConfig       `yaml:"orchestrator"`
	Plugins         map[string]PluginConfig  `yaml:"plugins"`
	Channels        map[string]ChannelConfig `yaml:"channels"`
	Scheduler       SchedulerConfig          `yaml:"scheduler"`
	RequestPackages RequestPackagesConfig    `yaml:"request_packages"`
	Lua             *LuaConfig               `yaml:"lua,omitempty"`
}

// LuaConfig configures embedded Lua plugins (content preparers). Use scripts_dir for local .lua files,
// or plugins + default_github/ref to download by name from GitHub (one repo, one subdir per plugin).
type LuaConfig struct {
	ScriptsDir    string           `yaml:"scripts_dir"`    // local dir of .lua files (e.g. scripts/hello-world.lua)
	Plugins       []LuaPluginEntry `yaml:"plugins"`        // plugin names to download (use default repo or per-plugin github/ref)
	DefaultGitHub string           `yaml:"default_github"` // default repo for plugins (e.g. opentalon/lua-plugins)
	DefaultRef    string           `yaml:"default_ref"`    // default ref (e.g. master)
}

// LuaPluginEntry is one Lua plugin: either a name (string) or { name, github?, ref? }.
type LuaPluginEntry struct {
	Name   string `yaml:"name"`
	GitHub string `yaml:"github"`
	Ref    string `yaml:"ref"`
}

// UnmarshalYAML allows Lua plugin to be a string (name only) or a map (name, github, ref).
func (e *LuaPluginEntry) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		e.Name = n.Value
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("lua plugin must be a string (name) or object { name, github?, ref? }")
	}
	var raw struct {
		Name   string `yaml:"name"`
		GitHub string `yaml:"github"`
		Ref    string `yaml:"ref"`
	}
	if err := n.Decode(&raw); err != nil {
		return err
	}
	e.Name = raw.Name
	e.GitHub = raw.GitHub
	e.Ref = raw.Ref
	return nil
}

// RequestPackagesConfig configures skill-style request packages (no compiled plugin).
type RequestPackagesConfig struct {
	Path               string          `yaml:"path"`                 // directory containing .yaml files (each file = one plugin set)
	SkillsPath         string          `yaml:"skills_path"`          // directory of OpenClaw-style skills (each subdir: SKILL.md or request.yaml)
	Skills             []SkillEntry    `yaml:"skills"`               // skill names to download (use default repo or per-skill github/ref)
	DefaultSkillGitHub string          `yaml:"default_skill_github"` // default repo for skills (e.g. openclaw/skills)
	DefaultSkillRef    string          `yaml:"default_skill_ref"`    // default ref (e.g. main)
	Inline             []RequestSetInl `yaml:"inline"`               // inline plugin sets
}

// SkillEntry is one skill to download: either a name (string in YAML) or { name, github?, ref? }.
type SkillEntry struct {
	Name   string `yaml:"name"`
	GitHub string `yaml:"github"`
	Ref    string `yaml:"ref"`
}

// UnmarshalYAML allows skill to be a string (name only) or a map (name, github, ref).
func (s *SkillEntry) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		s.Name = n.Value
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("skill must be a string (name) or object { name, github?, ref? }")
	}
	var raw struct {
		Name   string `yaml:"name"`
		GitHub string `yaml:"github"`
		Ref    string `yaml:"ref"`
	}
	if err := n.Decode(&raw); err != nil {
		return err
	}
	s.Name = raw.Name
	s.GitHub = raw.GitHub
	s.Ref = raw.Ref
	return nil
}

// MCPServerConfigInl is the inline config shape for one MCP server.
type MCPServerConfigInl struct {
	Server  string            `yaml:"server"`
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers,omitempty"`
}

// RequestSetInl is an inline request package set (plugin name + packages).
// If MCP is set, Packages is ignored — the tools come from the MCP plugin binary.
type RequestSetInl struct {
	Plugin      string              `yaml:"plugin"`
	Description string              `yaml:"description"`
	Packages    []RequestPackageInl `yaml:"packages"`
	MCP         *MCPServerConfigInl `yaml:"mcp,omitempty"`
}

// RequestPackageInl is the config shape for one request package.
type RequestPackageInl struct {
	Action      string            `yaml:"action"`
	Description string            `yaml:"description"`
	Method      string            `yaml:"method"`
	URL         string            `yaml:"url"`
	Body        string            `yaml:"body"`
	Headers     map[string]string `yaml:"headers"`
	RequiredEnv []string          `yaml:"required_env"`
	Parameters  []RequestParamInl `yaml:"parameters"`
}

// RequestParamInl describes one parameter.
type RequestParamInl struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Required    bool   `yaml:"required"`
}

// LogConfig holds logging options. When LOG_LEVEL=debug, LLM request payloads are logged.
type LogConfig struct {
	File string `yaml:"file"` // optional path to log file (env-expanded)
	Dir  string `yaml:"dir"`  // optional path to per-session log directory; default {data_dir}/logs when LOG_LEVEL=debug
}

type PluginConfig struct {
	Enabled  bool                   `yaml:"enabled"`
	Insecure *bool                  `yaml:"insecure"` // if true or omitted (default), preparer cannot run invoke; if false (trusted), can invoke
	Plugin   string                 `yaml:"plugin"`   // path to binary or grpc://... (optional if github is set)
	GitHub   string                 `yaml:"github"`   // e.g. "owner/repo" (bundler-style)
	Ref      string                 `yaml:"ref"`      // branch, tag, or commit; resolved and pinned in plugins.lock
	Config   map[string]interface{} `yaml:"config,omitempty"`
}

type SchedulerConfig struct {
	Jobs           []JobConfig `yaml:"jobs"`
	Approvers      []string    `yaml:"approvers,omitempty"`
	MaxJobsPerUser int         `yaml:"max_jobs_per_user,omitempty"`
}

type JobConfig struct {
	Name          string            `yaml:"name"`
	Interval      string            `yaml:"interval"`
	Action        string            `yaml:"action"`
	Args          map[string]string `yaml:"args,omitempty"`
	NotifyChannel string            `yaml:"notify_channel,omitempty"`
	Enabled       *bool             `yaml:"enabled,omitempty"`
}

type ChannelConfig struct {
	Enabled bool                   `yaml:"enabled"`
	Plugin  string                 `yaml:"plugin"` // path to binary or grpc://... (optional if github is set)
	GitHub  string                 `yaml:"github"` // e.g. "opentalon/slack-channel" (bundler-style)
	Ref     string                 `yaml:"ref"`    // branch, tag, or commit; pinned in channels.lock
	Config  map[string]interface{} `yaml:"config"`
}

// ContentPreparerEntry configures a plugin action to run before the first LLM call; its output becomes the user message (or can block the LLM via send_to_llm: false).
// If Guard is true, the plugin also runs before every subsequent LLM call (e.g. to sanitize tool results and prevent prompt injection).
type ContentPreparerEntry struct {
	Plugin   string `yaml:"plugin"`
	Action   string `yaml:"action"`
	ArgKey   string `yaml:"arg_key"`             // optional, default "text" — key for passing current content as arg
	Guard    bool   `yaml:"guard"`               // if true, also runs before every LLM call to sanitize messages
	FailOpen bool   `yaml:"fail_open,omitempty"` // default false (fail-closed): block request if guard/preparer fails
}

type OrchestratorConfig struct {
	Rules                 []string                   `yaml:"rules"`
	ContentPreparers      []ContentPreparerEntry     `yaml:"content_preparers,omitempty"`
	PermissionPlugin      string                     `yaml:"permission_plugin,omitempty"`       // if set, core calls this plugin with action "check" (actor, plugin) before running a tool
	MaxConcurrentSessions int                        `yaml:"max_concurrent_sessions,omitempty"` // max sessions running in parallel (default 1 = sequential)
	Pipeline              PipelineOrchestratorConfig `yaml:"pipeline,omitempty"`
}

// PipelineOrchestratorConfig enables structured multi-step pipeline execution.
type PipelineOrchestratorConfig struct {
	Enabled        bool   `yaml:"enabled"`          // default false
	MaxStepRetries int    `yaml:"max_step_retries"` // default 3
	StepTimeout    string `yaml:"step_timeout"`     // Go duration, default "60s"
}

type StateConfig struct {
	DataDir string        `yaml:"data_dir"`
	Session SessionConfig `yaml:"session,omitempty"`
}

// SessionConfig limits session size and optional idle pruning.
type SessionConfig struct {
	MaxMessages             int    `yaml:"max_messages"`               // cap messages per session (0 = no cap)
	MaxIdleDays             int    `yaml:"max_idle_days"`              // delete sessions not updated in N days (0 = don't prune)
	SummarizeAfter          int    `yaml:"summarize_after_messages"`   // run summarization after N messages (0 = off)
	MaxMessagesAfterSummary int    `yaml:"max_messages_after_summary"` // keep this many messages after summarization
	SummarizePrompt         string `yaml:"summarize_prompt"`           // system prompt for initial summarization (any language); empty = default English
	SummarizeUpdatePrompt   string `yaml:"summarize_update_prompt"`    // system prompt for updating existing summary (any language); empty = default English
}

type ModelsConfig struct {
	Providers map[string]ProviderConfig `yaml:"providers"`
	Catalog   map[string]CatalogEntry   `yaml:"catalog"`
}

type ProviderConfig struct {
	BaseURL string            `yaml:"base_url"`
	APIKey  string            `yaml:"api_key"`
	API     string            `yaml:"api"`
	Models  []ModelDefinition `yaml:"models"`
}

type ModelDefinition struct {
	ID            string     `yaml:"id"`
	Name          string     `yaml:"name"`
	Reasoning     bool       `yaml:"reasoning"`
	InputTypes    []string   `yaml:"input"`
	ContextWindow int        `yaml:"context_window"`
	MaxTokens     int        `yaml:"max_tokens"`
	Cost          CostConfig `yaml:"cost"`
}

type CostConfig struct {
	Input  float64 `yaml:"input"`
	Output float64 `yaml:"output"`
}

type CatalogEntry struct {
	Alias  string `yaml:"alias"`
	Weight int    `yaml:"weight"`
}

type RoutingConfig struct {
	Primary   string            `yaml:"primary"`
	Fallbacks []string          `yaml:"fallbacks"`
	Pin       map[string]string `yaml:"pin"`
	Affinity  AffinityConfig    `yaml:"affinity"`
}

type AffinityConfig struct {
	Enabled   bool   `yaml:"enabled"`
	Store     string `yaml:"store"`
	DecayDays int    `yaml:"decay_days"`
}

type AuthConfig struct {
	Cooldowns CooldownConfig `yaml:"cooldowns"`
}

type CooldownConfig struct {
	Initial         string `yaml:"initial"`
	Max             string `yaml:"max"`
	Multiplier      int    `yaml:"multiplier"`
	BillingMaxHours int    `yaml:"billing_max_hours"`
}

var envPattern = regexp.MustCompile(`\$\{([^}]+)}`)

func expandEnv(s string) string {
	return envPattern.ReplaceAllStringFunc(s, func(match string) string {
		varName := envPattern.FindStringSubmatch(match)[1]
		if val, ok := os.LookupEnv(varName); ok {
			return val
		}
		return match
	})
}

func expandTilde(s string) string {
	if strings.HasPrefix(s, "~") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(s, "~"), "/"))
	}
	return s
}

func expandEnvInProviders(cfg *Config) {
	for name, p := range cfg.Models.Providers {
		p.BaseURL = expandEnv(p.BaseURL)
		p.APIKey = expandEnv(p.APIKey)
		cfg.Models.Providers[name] = p
	}
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}
	return Parse(data)
}

func Parse(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	expandEnvInProviders(&cfg)
	expandEnvInPlugins(&cfg)
	expandEnvInChannels(&cfg)
	if cfg.State.DataDir == "" {
		home, _ := os.UserHomeDir()
		cfg.State.DataDir = filepath.Join(home, ".opentalon")
	} else {
		cfg.State.DataDir = expandTilde(expandEnv(cfg.State.DataDir))
	}
	if cfg.Log.File != "" {
		cfg.Log.File = expandTilde(expandEnv(cfg.Log.File))
	}
	if cfg.Log.Dir != "" {
		cfg.Log.Dir = expandTilde(expandEnv(cfg.Log.Dir))
	}
	if cfg.Lua != nil {
		if cfg.Lua.ScriptsDir != "" {
			cfg.Lua.ScriptsDir = expandTilde(expandEnv(cfg.Lua.ScriptsDir))
		}
		if cfg.Lua.DefaultGitHub != "" {
			cfg.Lua.DefaultGitHub = expandEnv(cfg.Lua.DefaultGitHub)
		}
		if cfg.Lua.DefaultRef != "" {
			cfg.Lua.DefaultRef = expandEnv(cfg.Lua.DefaultRef)
		}
	}
	return &cfg, nil
}

func expandEnvInPlugins(cfg *Config) {
	for name, p := range cfg.Plugins {
		p.Plugin = expandEnv(p.Plugin)
		for k, v := range p.Config {
			if s, ok := v.(string); ok {
				p.Config[k] = expandEnv(s)
			}
		}
		cfg.Plugins[name] = p
	}
}

func expandEnvInChannels(cfg *Config) {
	for name, ch := range cfg.Channels {
		ch.Plugin = expandEnv(ch.Plugin)
		ch.GitHub = expandEnv(ch.GitHub)
		ch.Ref = expandEnv(ch.Ref)
		for k, v := range ch.Config {
			if s, ok := v.(string); ok {
				ch.Config[k] = expandEnv(s)
			}
		}
		cfg.Channels[name] = ch
	}
}
