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
	Metrics         MetricsConfig            `yaml:"metrics,omitempty"`
	Orchestrator    OrchestratorConfig       `yaml:"orchestrator"`
	Plugins         map[string]PluginConfig  `yaml:"plugins"`
	Channels        map[string]ChannelConfig `yaml:"channels"`
	Scheduler       SchedulerConfig          `yaml:"scheduler"`
	RequestPackages RequestPackagesConfig    `yaml:"request_packages"`
	Lua             *LuaConfig               `yaml:"lua,omitempty"`
	Profiles        ProfilesConfig           `yaml:"profiles,omitempty"`
	Bootstrap       BootstrapConfig          `yaml:"bootstrap,omitempty"`
	Redis           RedisConfig              `yaml:"redis,omitempty"`
	Cluster         ClusterConfig            `yaml:"cluster,omitempty"`
	PluginExec      PluginExecConfig         `yaml:"plugin_exec,omitempty"`
}

// MetricsConfig enables a Prometheus /metrics HTTP endpoint.
type MetricsConfig struct {
	Enabled bool   `yaml:"enabled"`
	Addr    string `yaml:"addr"` // e.g. ":2112"; defaults to ":2112" when enabled
}

// RedisConfig holds the connection details for the shared Redis instance used by
// cluster deduplication and the plugin exec dispatcher. Having one block avoids
// operators who want only one subsystem having to fill in a section named after
// the other.
//
// Standalone mode: set redis_url only.
// Sentinel mode:   set master_name + sentinels (redis_url is ignored).
type RedisConfig struct {
	RedisURL         string   `yaml:"redis_url"`         // standalone: redis://[:pass@]host:port/db
	MasterName       string   `yaml:"master_name"`       // sentinel: name of the master
	Sentinels        []string `yaml:"sentinels"`         // sentinel: list of host:port addresses
	Password         string   `yaml:"password"`          // Redis master password (sentinel mode; standalone uses URL)
	SentinelPassword string   `yaml:"sentinel_password"` // optional: Sentinel ACL password
}

// PluginExecConfig enables trusted plugins to execute ToolRegistry actions via a Redis stream.
// Requires redis.redis_url (or sentinel config) to be set.
// See docs/workflows.md for details.
type PluginExecConfig struct {
	Enabled       bool   `yaml:"enabled"`
	ActionTimeout string `yaml:"action_timeout,omitempty"` // e.g. "30s"; max time per RunAction call (default 60s)
}

// ClusterConfig enables Redis-backed message deduplication for multi-pod deployments.
// When enabled: every inbound message acquires a Redis lock before processing, so only
// one pod handles each unique message even when multiple pods receive it simultaneously.
// Requires redis.redis_url (or sentinel config) to be set.
type ClusterConfig struct {
	Enabled  bool   `yaml:"enabled"`
	DedupTTL string `yaml:"dedup_ttl"` // Go duration for dedup lock TTL; default "5m"
}

// BootstrapConfig configures a remote HTTP endpoint that is called once at startup
// to fetch additional channels, plugins, and group_plugins. Remote entries are merged
// into the static config (static wins on key conflicts).
type BootstrapConfig struct {
	URL         string `yaml:"url"`          // required to enable bootstrap
	Token       string `yaml:"token"`        // bearer token; supports ${ENV_VAR}
	TokenHeader string `yaml:"token_header"` // default "Authorization"
	TokenPrefix string `yaml:"token_prefix"` // default "Bearer "
	Timeout     string `yaml:"timeout"`      // Go duration; default "30s"
	Retries     int    `yaml:"retries"`      // retries on transient error; default 3
	Required    bool   `yaml:"required"`     // if true, fail startup when fetch fails
}

// ProfilesConfig enables profile-based multi-tenancy.
// When who_am_i.url is set, every inbound message must carry a profile token in
// InboundMessage.Metadata["profile_token"]. The token is verified against the WhoAmI
// server; on failure the request is blocked.
type ProfilesConfig struct {
	WhoAmI WhoAmIConfig           `yaml:"who_am_i"`
	Groups map[string]GroupConfig `yaml:"groups,omitempty"` // optional static baseline seeded to DB on startup
}

// WhoAmIConfig configures the external identity-verification server.
type WhoAmIConfig struct {
	URL               string            `yaml:"url"`                     // required to enable profile verification
	Method            string            `yaml:"method"`                  // GET or POST; default POST
	TokenHeader       string            `yaml:"token_header"`            // header sent to server; default "Authorization"
	TokenPrefix       string            `yaml:"token_prefix"`            // value prefix; default "Bearer "
	Timeout           string            `yaml:"timeout"`                 // Go duration; default "5s"
	CacheTTL          string            `yaml:"cache_ttl"`               // Go duration; default "60s"
	EntityIDField     string            `yaml:"entity_id_field"`         // JSON field in response; default "entity_id"
	GroupField        string            `yaml:"group_field"`             // JSON field in response; default "group"
	PluginsField      string            `yaml:"plugins_field"`           // optional JSON field for plugin list; default "plugins"
	ModelField        string            `yaml:"model_field"`             // optional JSON field for model override; default "model"
	ChannelTypeField  string            `yaml:"channel_type_field"`      // optional JSON field for channel type in response; default "channel_type"
	ChannelTypeHeader string            `yaml:"channel_type_header"`     // optional header name to send channel type to WhoAmI server (e.g. "X-Channel-Type")
	LimitField        string            `yaml:"limit_field"`             // optional JSON field for token spend limit; default "limit"
	LimitTimeField    string            `yaml:"limit_time_field"`        // optional JSON field for limit window duration (e.g. "1h"); default "limit_time"
	CredentialsField  string            `yaml:"credentials_field"`       // optional JSON field for per-MCP-server tokens map; default "credentials"
	ExtraHeaders      map[string]string `yaml:"extra_headers,omitempty"` // static headers added to every WhoAmI call; values support ${ENV_VAR}
}

// GroupConfig is a static baseline of plugin IDs for a group.
// Seeded to group_plugins on startup with source="config"; never overwrites "whoami" or "admin" rows.
type GroupConfig struct {
	Plugins []string `yaml:"plugins"`
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
	Plugin        string              `yaml:"plugin"`
	Description   string              `yaml:"description"`
	Packages      []RequestPackageInl `yaml:"packages"`
	MCP           *MCPServerConfigInl `yaml:"mcp,omitempty"`
	AllowedGroups []string            `yaml:"groups,omitempty"` // restrict to these profile groups
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

// LogConfig holds logging options. Level can be overridden by the LOG_LEVEL env var.
type LogConfig struct {
	Level string `yaml:"level"` // debug, info, warn, error; default info; overridden by LOG_LEVEL env var
}

type PluginConfig struct {
	Enabled     bool                   `yaml:"enabled"`
	Insecure    *bool                  `yaml:"insecure"` // if true or omitted (default), preparer cannot run invoke; if false (trusted), can invoke
	Plugin      string                 `yaml:"plugin"`   // path to binary or grpc://... (optional if github is set)
	GitHub      string                 `yaml:"github"`   // e.g. "owner/repo" (bundler-style)
	Ref         string                 `yaml:"ref"`      // branch, tag, or commit; resolved and pinned in plugins.lock
	Config      map[string]interface{} `yaml:"config,omitempty"`
	DialTimeout string                 `yaml:"dial_timeout,omitempty"` // e.g. "30s"; overrides the default 5s gRPC init timeout
	ExposeHTTP  bool                   `yaml:"expose_http,omitempty"`  // opt-in: reverse-proxy /{plugin-name}/* through the webhook server
}

type SchedulerConfig struct {
	Jobs           []JobConfig `yaml:"jobs"`
	Approvers      []string    `yaml:"approvers,omitempty"`
	MaxJobsPerUser int         `yaml:"max_jobs_per_user,omitempty"`
}

type JobConfig struct {
	Name          string            `yaml:"name"`
	Interval      string            `yaml:"interval,omitempty"`
	Cron          string            `yaml:"cron,omitempty"`
	At            string            `yaml:"at,omitempty"`
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
	DB      DBConfig      `yaml:"db,omitempty"`
	Session SessionConfig `yaml:"session,omitempty"`
}

// DBConfig selects the database backend. Driver defaults to "sqlite".
// For postgres, set driver: "postgres" and provide a DSN.
type DBConfig struct {
	Driver string `yaml:"driver"` // "sqlite" (default) or "postgres"
	DSN    string `yaml:"dsn"`    // postgres only: e.g. "postgres://user:pass@host:5432/dbname?sslmode=require"
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
	expandEnvInBootstrap(&cfg)
	expandEnvInRedis(&cfg)
	cfg.Cluster.DedupTTL = expandEnv(cfg.Cluster.DedupTTL)
	cfg.Metrics.Addr = expandEnv(cfg.Metrics.Addr)
	if cfg.Metrics.Enabled && cfg.Metrics.Addr == "" {
		cfg.Metrics.Addr = ":2112"
	}
	if cfg.State.DataDir == "" {
		home, _ := os.UserHomeDir()
		cfg.State.DataDir = filepath.Join(home, ".opentalon")
	} else {
		cfg.State.DataDir = expandTilde(expandEnv(cfg.State.DataDir))
	}
	if cfg.Log.Level != "" {
		cfg.Log.Level = expandEnv(cfg.Log.Level)
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

// ResolveStateDataDir returns an absolute path for state data storage.
// If state.data_dir is relative, it is resolved against the directory
// containing configFile. configFile should be an absolute path; if it is not,
// its directory is absolutized when possible. Absolute data_dir values are
// returned cleaned.
func ResolveStateDataDir(cfg *Config, configFile string) string {
	d := cfg.State.DataDir
	if d == "" {
		return ""
	}
	if filepath.IsAbs(d) {
		return filepath.Clean(d)
	}
	if configFile == "" {
		abs, err := filepath.Abs(d)
		if err != nil {
			return filepath.Clean(d)
		}
		return abs
	}
	cfgDir := filepath.Dir(configFile)
	if !filepath.IsAbs(cfgDir) {
		var err error
		cfgDir, err = filepath.Abs(cfgDir)
		if err != nil {
			return filepath.Clean(filepath.Join(filepath.Dir(configFile), d))
		}
	}
	return filepath.Clean(filepath.Join(cfgDir, d))
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

func expandEnvInBootstrap(cfg *Config) {
	cfg.Bootstrap.URL = expandEnv(cfg.Bootstrap.URL)
	cfg.Bootstrap.Token = expandEnv(cfg.Bootstrap.Token)
	cfg.Bootstrap.TokenHeader = expandEnv(cfg.Bootstrap.TokenHeader)
	cfg.Bootstrap.TokenPrefix = expandEnv(cfg.Bootstrap.TokenPrefix)
	cfg.Bootstrap.Timeout = expandEnv(cfg.Bootstrap.Timeout)
}

func expandEnvInRedis(cfg *Config) {
	cfg.Redis.RedisURL = expandEnv(cfg.Redis.RedisURL)
	cfg.Redis.MasterName = expandEnv(cfg.Redis.MasterName)
	cfg.Redis.Password = expandEnv(cfg.Redis.Password)
	cfg.Redis.SentinelPassword = expandEnv(cfg.Redis.SentinelPassword)
	for i, s := range cfg.Redis.Sentinels {
		cfg.Redis.Sentinels[i] = expandEnv(s)
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
