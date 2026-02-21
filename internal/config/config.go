package config

import (
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Models  ModelsConfig  `yaml:"models"`
	Routing RoutingConfig `yaml:"routing"`
	Auth    AuthConfig    `yaml:"auth"`
}

type ModelsConfig struct {
	Providers map[string]ProviderConfig   `yaml:"providers"`
	Catalog   map[string]CatalogEntry     `yaml:"catalog"`
}

type ProviderConfig struct {
	BaseURL string            `yaml:"base_url"`
	APIKey  string            `yaml:"api_key"`
	API     string            `yaml:"api"`
	Models  []ModelDefinition `yaml:"models"`
}

type ModelDefinition struct {
	ID            string   `yaml:"id"`
	Name          string   `yaml:"name"`
	Reasoning     bool     `yaml:"reasoning"`
	InputTypes    []string `yaml:"input"`
	ContextWindow int      `yaml:"context_window"`
	MaxTokens     int      `yaml:"max_tokens"`
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
	Enabled  bool   `yaml:"enabled"`
	Store    string `yaml:"store"`
	DecayDays int   `yaml:"decay_days"`
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
	return &cfg, nil
}
