package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Models       ModelsConfig             `yaml:"models"`
	Routing      RoutingConfig            `yaml:"routing"`
	Auth         AuthConfig               `yaml:"auth"`
	State        StateConfig              `yaml:"state"`
	Orchestrator OrchestratorConfig       `yaml:"orchestrator"`
	Plugins      map[string]PluginConfig  `yaml:"plugins"`
	Channels     map[string]ChannelConfig `yaml:"channels"`
	Scheduler    SchedulerConfig          `yaml:"scheduler"`
}

type PluginConfig struct {
	Enabled bool                   `yaml:"enabled"`
	Path    string                 `yaml:"path"`
	Config  map[string]interface{} `yaml:"config,omitempty"`
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
	Plugin  string                 `yaml:"plugin"`
	Config  map[string]interface{} `yaml:"config"`
}

type OrchestratorConfig struct {
	Rules []string `yaml:"rules"`
}

type StateConfig struct {
	DataDir string `yaml:"data_dir"`
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
		cfg.State.DataDir = expandEnv(cfg.State.DataDir)
	}
	return &cfg, nil
}

func expandEnvInPlugins(cfg *Config) {
	for name, p := range cfg.Plugins {
		p.Path = expandEnv(p.Path)
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
		for k, v := range ch.Config {
			if s, ok := v.(string); ok {
				ch.Config[k] = expandEnv(s)
			}
		}
		cfg.Channels[name] = ch
	}
}
