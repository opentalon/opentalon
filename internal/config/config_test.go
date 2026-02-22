package config

import (
	"os"
	"testing"
)

const testYAML = `
models:
  providers:
    anthropic:
      api_key: "${ANTHROPIC_API_KEY}"
      api: anthropic-messages
    openai:
      api_key: "${OPENAI_API_KEY}"
      api: openai-completions
    ovh:
      base_url: "${OVH_BASE_URL}"
      api_key: "${OVH_API_KEY}"
      api: openai-completions
      models:
        - id: gpt-oss-120b
          name: GPT OSS 120B
          reasoning: true
          input: [text]
          context_window: 131072
          max_tokens: 131072
          cost:
            input: 0.08
            output: 0.44
    ollama:
      base_url: "http://localhost:11434/v1"
      api: openai-completions
      models:
        - id: llama3
          name: Llama 3 8B
          input: [text]
          context_window: 8192
          cost:
            input: 0
            output: 0
  catalog:
    anthropic/claude-haiku-4:
      alias: haiku
      weight: 90
    anthropic/claude-sonnet-4:
      alias: sonnet
      weight: 50
    anthropic/claude-opus-4-6:
      alias: opus
      weight: 10

routing:
  primary: anthropic/claude-haiku-4
  fallbacks:
    - anthropic/claude-sonnet-4
    - openai/gpt-5.2
    - anthropic/claude-opus-4-6
  pin:
    code: anthropic/claude-sonnet-4
  affinity:
    enabled: true
    store: ~/.opentalon/affinity.json
    decay_days: 30

auth:
  cooldowns:
    initial: "1m"
    max: "1h"
    multiplier: 5
    billing_max_hours: 24

state:
  data_dir: /var/lib/opentalon
`

func TestParseConfig(t *testing.T) {
	cfg, err := Parse([]byte(testYAML))
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.Models.Providers) != 4 {
		t.Errorf("expected 4 providers, got %d", len(cfg.Models.Providers))
	}

	ovh := cfg.Models.Providers["ovh"]
	if ovh.API != "openai-completions" {
		t.Errorf("ovh api = %q, want openai-completions", ovh.API)
	}
	if len(ovh.Models) != 1 {
		t.Fatalf("ovh models = %d, want 1", len(ovh.Models))
	}
	if ovh.Models[0].ID != "gpt-oss-120b" {
		t.Errorf("ovh model id = %q, want gpt-oss-120b", ovh.Models[0].ID)
	}
	if ovh.Models[0].Cost.Input != 0.08 {
		t.Errorf("ovh model cost.input = %f, want 0.08", ovh.Models[0].Cost.Input)
	}
	if ovh.Models[0].ContextWindow != 131072 {
		t.Errorf("ovh model context_window = %d, want 131072", ovh.Models[0].ContextWindow)
	}
}

func TestParseCatalog(t *testing.T) {
	cfg, err := Parse([]byte(testYAML))
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.Models.Catalog) != 3 {
		t.Errorf("expected 3 catalog entries, got %d", len(cfg.Models.Catalog))
	}

	haiku := cfg.Models.Catalog["anthropic/claude-haiku-4"]
	if haiku.Alias != "haiku" {
		t.Errorf("haiku alias = %q, want haiku", haiku.Alias)
	}
	if haiku.Weight != 90 {
		t.Errorf("haiku weight = %d, want 90", haiku.Weight)
	}

	opus := cfg.Models.Catalog["anthropic/claude-opus-4-6"]
	if opus.Weight != 10 {
		t.Errorf("opus weight = %d, want 10", opus.Weight)
	}
}

func TestParseRouting(t *testing.T) {
	cfg, err := Parse([]byte(testYAML))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Routing.Primary != "anthropic/claude-haiku-4" {
		t.Errorf("primary = %q, want anthropic/claude-haiku-4", cfg.Routing.Primary)
	}
	if len(cfg.Routing.Fallbacks) != 3 {
		t.Errorf("fallbacks = %d, want 3", len(cfg.Routing.Fallbacks))
	}
	if cfg.Routing.Pin["code"] != "anthropic/claude-sonnet-4" {
		t.Errorf("pin[code] = %q, want anthropic/claude-sonnet-4", cfg.Routing.Pin["code"])
	}
	if !cfg.Routing.Affinity.Enabled {
		t.Error("affinity should be enabled")
	}
	if cfg.Routing.Affinity.DecayDays != 30 {
		t.Errorf("decay_days = %d, want 30", cfg.Routing.Affinity.DecayDays)
	}
}

func TestEnvSubstitution(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test-123")
	t.Setenv("OVH_BASE_URL", "https://ovh.example.com/v1")
	t.Setenv("OVH_API_KEY", "ovh-key-456")

	cfg, err := Parse([]byte(testYAML))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Models.Providers["anthropic"].APIKey != "sk-ant-test-123" {
		t.Errorf("anthropic api_key = %q, want sk-ant-test-123", cfg.Models.Providers["anthropic"].APIKey)
	}
	if cfg.Models.Providers["ovh"].BaseURL != "https://ovh.example.com/v1" {
		t.Errorf("ovh base_url = %q, want https://ovh.example.com/v1", cfg.Models.Providers["ovh"].BaseURL)
	}
	if cfg.Models.Providers["ovh"].APIKey != "ovh-key-456" {
		t.Errorf("ovh api_key = %q, want ovh-key-456", cfg.Models.Providers["ovh"].APIKey)
	}
}

func TestEnvSubstitutionPreservesUnsetVars(t *testing.T) {
	//nolint:errcheck // test cleanup of env var
	os.Unsetenv("OPENAI_API_KEY")
	cfg, err := Parse([]byte(testYAML))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Models.Providers["openai"].APIKey != "${OPENAI_API_KEY}" {
		t.Errorf("unset env var should be preserved, got %q", cfg.Models.Providers["openai"].APIKey)
	}
}

func TestEnvSubstitutionLiteralURLs(t *testing.T) {
	cfg, err := Parse([]byte(testYAML))
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Models.Providers["ollama"].BaseURL != "http://localhost:11434/v1" {
		t.Errorf("literal URL should not be modified, got %q", cfg.Models.Providers["ollama"].BaseURL)
	}
}

func TestParseInvalidYAML(t *testing.T) {
	_, err := Parse([]byte("{{invalid yaml"))
	if err == nil {
		t.Error("expected error for invalid YAML")
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("TEST_VAR", "hello")

	tests := []struct {
		input string
		want  string
	}{
		{"${TEST_VAR}", "hello"},
		{"prefix-${TEST_VAR}-suffix", "prefix-hello-suffix"},
		{"${NONEXISTENT}", "${NONEXISTENT}"},
		{"no vars here", "no vars here"},
		{"", ""},
	}
	for _, tt := range tests {
		got := expandEnv(tt.input)
		if got != tt.want {
			t.Errorf("expandEnv(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestStateDataDirExplicit(t *testing.T) {
	cfg, err := Parse([]byte(testYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.State.DataDir != "/var/lib/opentalon" {
		t.Errorf("data_dir = %q, want /var/lib/opentalon", cfg.State.DataDir)
	}
}

func TestStateDataDirDefault(t *testing.T) {
	yaml := `
models:
  providers: {}
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	want := home + "/.opentalon"
	if cfg.State.DataDir != want {
		t.Errorf("data_dir = %q, want %q", cfg.State.DataDir, want)
	}
}

func TestStateDataDirEnvSubstitution(t *testing.T) {
	t.Setenv("OPENTALON_DATA_DIR", "/custom/data")
	yaml := `
models:
  providers: {}
state:
  data_dir: "${OPENTALON_DATA_DIR}"
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.State.DataDir != "/custom/data" {
		t.Errorf("data_dir = %q, want /custom/data", cfg.State.DataDir)
	}
}

func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.yaml"
	if err := os.WriteFile(path, []byte(testYAML), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Models.Providers) != 4 {
		t.Errorf("expected 4 providers, got %d", len(cfg.Models.Providers))
	}
}

func TestLoadFileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/config.yaml")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestParseAuthCooldowns(t *testing.T) {
	cfg, err := Parse([]byte(testYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Auth.Cooldowns.Initial != "1m" {
		t.Errorf("cooldown initial = %q, want 1m", cfg.Auth.Cooldowns.Initial)
	}
	if cfg.Auth.Cooldowns.Max != "1h" {
		t.Errorf("cooldown max = %q, want 1h", cfg.Auth.Cooldowns.Max)
	}
	if cfg.Auth.Cooldowns.Multiplier != 5 {
		t.Errorf("cooldown multiplier = %d, want 5", cfg.Auth.Cooldowns.Multiplier)
	}
	if cfg.Auth.Cooldowns.BillingMaxHours != 24 {
		t.Errorf("cooldown billing_max_hours = %d, want 24", cfg.Auth.Cooldowns.BillingMaxHours)
	}
}

func TestParseModelDefinitionFields(t *testing.T) {
	cfg, err := Parse([]byte(testYAML))
	if err != nil {
		t.Fatal(err)
	}
	ovh := cfg.Models.Providers["ovh"].Models[0]
	if ovh.Name != "GPT OSS 120B" {
		t.Errorf("name = %q, want GPT OSS 120B", ovh.Name)
	}
	if !ovh.Reasoning {
		t.Error("reasoning should be true")
	}
	if len(ovh.InputTypes) != 1 || ovh.InputTypes[0] != "text" {
		t.Errorf("input types = %v, want [text]", ovh.InputTypes)
	}
	if ovh.MaxTokens != 131072 {
		t.Errorf("max_tokens = %d, want 131072", ovh.MaxTokens)
	}
	if ovh.Cost.Output != 0.44 {
		t.Errorf("cost.output = %f, want 0.44", ovh.Cost.Output)
	}
}

func TestParseEmptyConfig(t *testing.T) {
	cfg, err := Parse([]byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Models.Providers) != 0 {
		t.Errorf("expected empty providers")
	}
	home, _ := os.UserHomeDir()
	want := home + "/.opentalon"
	if cfg.State.DataDir != want {
		t.Errorf("default data_dir = %q, want %q", cfg.State.DataDir, want)
	}
}

func TestExpandEnvMultipleVars(t *testing.T) {
	t.Setenv("VAR_A", "aaa")
	t.Setenv("VAR_B", "bbb")
	got := expandEnv("${VAR_A}-${VAR_B}")
	if got != "aaa-bbb" {
		t.Errorf("expandEnv = %q, want aaa-bbb", got)
	}
}

func TestParseOrchestratorRules(t *testing.T) {
	yaml := `
models:
  providers: {}
orchestrator:
  rules:
    - "Never send PII to external plugins"
    - "All financial data must stay internal"
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Orchestrator.Rules) != 2 {
		t.Errorf("expected 2 rules, got %d", len(cfg.Orchestrator.Rules))
	}
	if cfg.Orchestrator.Rules[0] != "Never send PII to external plugins" {
		t.Errorf("rule[0] = %q", cfg.Orchestrator.Rules[0])
	}
}

func TestParseOrchestratorNoRules(t *testing.T) {
	yaml := `
models:
  providers: {}
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Orchestrator.Rules) != 0 {
		t.Errorf("expected 0 rules when omitted, got %d", len(cfg.Orchestrator.Rules))
	}
}

func TestParseChannels(t *testing.T) {
	t.Setenv("SLACK_APP_TOKEN", "xapp-test")
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-test")

	yaml := `
models:
  providers: {}
channels:
  my-slack:
    enabled: true
    plugin: "./plugins/opentalon-slack"
    config:
      app_token: "${SLACK_APP_TOKEN}"
      bot_token: "${SLACK_BOT_TOKEN}"
  my-telegram:
    enabled: true
    plugin: "grpc://telegram.internal:9001"
    config:
      bot_token: "static-token"
  disabled-channel:
    enabled: false
    plugin: "docker://ghcr.io/opentalon/plugin-x:latest"
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Channels) != 3 {
		t.Fatalf("expected 3 channels, got %d", len(cfg.Channels))
	}

	slack := cfg.Channels["my-slack"]
	if !slack.Enabled {
		t.Error("my-slack should be enabled")
	}
	if slack.Plugin != "./plugins/opentalon-slack" {
		t.Errorf("slack plugin = %q", slack.Plugin)
	}
	if slack.Config["app_token"] != "xapp-test" {
		t.Errorf("slack app_token = %q, want xapp-test", slack.Config["app_token"])
	}
	if slack.Config["bot_token"] != "xoxb-test" {
		t.Errorf("slack bot_token = %q, want xoxb-test", slack.Config["bot_token"])
	}

	tg := cfg.Channels["my-telegram"]
	if tg.Plugin != "grpc://telegram.internal:9001" {
		t.Errorf("telegram plugin = %q", tg.Plugin)
	}

	disabled := cfg.Channels["disabled-channel"]
	if disabled.Enabled {
		t.Error("disabled-channel should be disabled")
	}
}

func TestParseChannelsEnvInPlugin(t *testing.T) {
	t.Setenv("MY_PLUGIN_HOST", "myhost.internal")
	yaml := `
models:
  providers: {}
channels:
  dynamic:
    enabled: true
    plugin: "grpc://${MY_PLUGIN_HOST}:9001"
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Channels["dynamic"].Plugin != "grpc://myhost.internal:9001" {
		t.Errorf("plugin = %q, want grpc://myhost.internal:9001", cfg.Channels["dynamic"].Plugin)
	}
}

func TestParseNoChannels(t *testing.T) {
	yaml := `
models:
  providers: {}
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Channels) != 0 {
		t.Errorf("expected 0 channels when omitted, got %d", len(cfg.Channels))
	}
}

func TestParseChannelAllModes(t *testing.T) {
	yaml := `
models:
  providers: {}
channels:
  binary:
    enabled: true
    plugin: "./plugins/my-plugin"
  grpc:
    enabled: true
    plugin: "grpc://host:9001"
  docker:
    enabled: true
    plugin: "docker://img:tag"
  webhook:
    enabled: true
    plugin: "https://example.com/hook"
  websocket:
    enabled: true
    plugin: "wss://ws.example.com/ch"
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Channels) != 5 {
		t.Fatalf("expected 5 channels, got %d", len(cfg.Channels))
	}
}

func TestParseSchedulerJobs(t *testing.T) {
	yaml := `
models:
  providers: {}
scheduler:
  jobs:
    - name: "violation-check"
      interval: "1h"
      action: "ipossum.check_violations"
      notify_channel: "whatsapp"
    - name: "daily-report"
      interval: "24h"
      action: "reports.generate"
      args:
        format: "summary"
      enabled: false
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Scheduler.Jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(cfg.Scheduler.Jobs))
	}

	job := cfg.Scheduler.Jobs[0]
	if job.Name != "violation-check" {
		t.Errorf("job name = %q", job.Name)
	}
	if job.Interval != "1h" {
		t.Errorf("job interval = %q", job.Interval)
	}
	if job.Action != "ipossum.check_violations" {
		t.Errorf("job action = %q", job.Action)
	}
	if job.NotifyChannel != "whatsapp" {
		t.Errorf("job notify_channel = %q", job.NotifyChannel)
	}
	if job.Enabled != nil {
		t.Error("enabled should be nil (defaults to true)")
	}

	job2 := cfg.Scheduler.Jobs[1]
	if job2.Enabled == nil || *job2.Enabled != false {
		t.Error("daily-report should be explicitly disabled")
	}
	if job2.Args["format"] != "summary" {
		t.Errorf("job2 args format = %q", job2.Args["format"])
	}
}

func TestParseSchedulerApprovers(t *testing.T) {
	yaml := `
models:
  providers: {}
scheduler:
  approvers: ["admin@company.com", "ops-lead@company.com"]
  max_jobs_per_user: 5
  jobs:
    - name: "check"
      interval: "1h"
      action: "a.b"
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Scheduler.Approvers) != 2 {
		t.Fatalf("expected 2 approvers, got %d", len(cfg.Scheduler.Approvers))
	}
	if cfg.Scheduler.Approvers[0] != "admin@company.com" {
		t.Errorf("approver[0] = %q", cfg.Scheduler.Approvers[0])
	}
	if cfg.Scheduler.Approvers[1] != "ops-lead@company.com" {
		t.Errorf("approver[1] = %q", cfg.Scheduler.Approvers[1])
	}
	if cfg.Scheduler.MaxJobsPerUser != 5 {
		t.Errorf("max_jobs_per_user = %d", cfg.Scheduler.MaxJobsPerUser)
	}
}

func TestParseSchedulerNoApprovers(t *testing.T) {
	yaml := `
models:
  providers: {}
scheduler:
  jobs:
    - name: "check"
      interval: "1h"
      action: "a.b"
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Scheduler.Approvers) != 0 {
		t.Errorf("expected no approvers, got %d", len(cfg.Scheduler.Approvers))
	}
	if cfg.Scheduler.MaxJobsPerUser != 0 {
		t.Errorf("max_jobs_per_user should default to 0, got %d", cfg.Scheduler.MaxJobsPerUser)
	}
}

func TestParseSchedulerNoJobs(t *testing.T) {
	yaml := `
models:
  providers: {}
`
	cfg, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Scheduler.Jobs) != 0 {
		t.Errorf("expected 0 jobs when omitted, got %d", len(cfg.Scheduler.Jobs))
	}
}
