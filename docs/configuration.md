# Configuration Guide

OpenTalon uses a single YAML file for configuration. By default it lives at `~/.opentalon/config.yaml`.

## Quick Start

The simplest possible config — one provider, one API key:

```yaml
models:
  providers:
    anthropic:
      api_key: "${ANTHROPIC_API_KEY}"
      api: anthropic-messages
```

Set your key and run with the **console channel** in your config (see [Bundler-style plugins and channels](#bundler-style-plugins-and-channels) below). Example:

```bash
export ANTHROPIC_API_KEY="sk-ant-..."
opentalon -config config.yaml
```

With the console channel enabled, you get an interactive prompt: type a message, press Enter, and the LLM replies. Ctrl+C or Ctrl+D to exit. OpenTalon uses the provider and model from your config (e.g. `routing.primary`). Without the console channel, the process may run other channels only and not show a prompt.

## Adding Providers

Every provider needs three things:

| Field | What it does | Example |
|---|---|---|
| `api_key` | Your API key (always use `${ENV_VAR}`) | `"${OPENAI_API_KEY}"` |
| `api` | Wire format — tells OpenTalon how to talk to the API | `openai-completions` or `anthropic-messages` |
| `base_url` | API endpoint (optional — defaults to the official URL) | `"http://localhost:11434/v1"` |

There are only **two API formats**:

| Format | Use for | Default endpoint |
|---|---|---|
| `openai-completions` | OpenAI, Azure, Ollama, vLLM, Groq, Together, OVH, any OpenAI-compatible | `https://api.openai.com/v1` |
| `anthropic-messages` | Anthropic Claude models | `https://api.anthropic.com` |

### Example: Multiple providers

```yaml
models:
  providers:
    # Anthropic — uses its own API format
    anthropic:
      api_key: "${ANTHROPIC_API_KEY}"
      api: anthropic-messages

    # OpenAI — standard OpenAI format
    openai:
      api_key: "${OPENAI_API_KEY}"
      api: openai-completions

    # Self-hosted Ollama — same format as OpenAI, different endpoint, no key
    ollama:
      base_url: "http://localhost:11434/v1"
      api: openai-completions

    # OVH Cloud — OpenAI-compatible with a custom endpoint
    ovh:
      base_url: "${OVH_BASE_URL}"
      api_key: "${OVH_API_KEY}"
      api: openai-completions
```

## Custom Models

For well-known providers (Anthropic, OpenAI), the model catalog is built in. For custom or self-hosted providers, declare the models explicitly:

```yaml
models:
  providers:
    ollama:
      base_url: "http://localhost:11434/v1"
      api: openai-completions
      models:
        - id: llama3
          name: Llama 3 8B
          input: [text]
          context_window: 8192
          cost:
            input: 0        # free (local)
            output: 0

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
            input: 0.08      # $ per 1M tokens
            output: 0.44
```

Model fields:

| Field | Required | Description |
|---|---|---|
| `id` | yes | Model ID sent to the API (e.g. `llama3`, `gpt-4o`) |
| `name` | no | Human-readable name |
| `reasoning` | no | Whether the model supports chain-of-thought |
| `input` | no | Supported input types (`[text]`, `[text, image]`) |
| `context_window` | no | Max context size in tokens |
| `max_tokens` | no | Max output tokens |
| `cost.input` | no | Cost per 1M input tokens (USD). Used by smart router |
| `cost.output` | no | Cost per 1M output tokens (USD) |

## Smart Routing

The catalog assigns **weights** to models. Higher weight = cheaper = tried first:

```yaml
models:
  catalog:
    anthropic/claude-haiku-4:
      alias: haiku
      weight: 90          # cheapest, tried first
    anthropic/claude-sonnet-4:
      alias: sonnet
      weight: 50          # mid-tier
    anthropic/claude-opus-4-6:
      alias: opus
      weight: 10          # most expensive, last resort
    openai/gpt-5.2:
      alias: gpt52
      weight: 40
```

How it works:
1. Cheap models (high weight) are tried first
2. If the user rejects the response (regenerates, says "try again"), OpenTalon escalates to the next model
3. Over time, the router learns which model works best for which task type

### Pinning models to task types

If you already know what works best:

```yaml
routing:
  pin:
    code: anthropic/claude-sonnet-4     # always use Sonnet for code
    chat: anthropic/claude-haiku-4      # Haiku is fine for chat
```

### Failover chain

If a provider is down or rate-limited, OpenTalon falls back:

```yaml
routing:
  primary: anthropic/claude-haiku-4
  fallbacks:
    - anthropic/claude-sonnet-4
    - openai/gpt-5.2
    - anthropic/claude-opus-4-6
```

### Affinity learning

Enable this to let the router learn from user feedback:

```yaml
routing:
  affinity:
    enabled: true
    store: ~/.opentalon/affinity.json
    decay_days: 30        # forget old preferences after 30 days
```

## API Keys & Secrets

**Never hardcode secrets.** Always use environment variables:

```yaml
# Good
api_key: "${ANTHROPIC_API_KEY}"

# Bad — never do this
api_key: "sk-ant-abc123..."
```

Set them in your shell, `.env` file, or secrets manager:

```bash
export ANTHROPIC_API_KEY="sk-ant-..."
export OPENAI_API_KEY="sk-..."
```

For multiple keys per provider (round-robin on rate limits):

```bash
export ANTHROPIC_API_KEY_1="sk-ant-key1..."
export ANTHROPIC_API_KEY_2="sk-ant-key2..."
```

## Auth Cooldowns

When a provider returns rate limit errors, OpenTalon backs off automatically:

```yaml
auth:
  cooldowns:
    initial: 1m           # first cooldown: 1 minute
    max: 1h               # max cooldown: 1 hour
    multiplier: 5          # each consecutive failure: 5x longer
    billing_max_hours: 24  # billing/credit errors: up to 24h
```

## State Directory

All runtime data (sessions, affinity, scheduler jobs) is stored here:

```yaml
state:
  data_dir: ~/.opentalon    # default
```

Override with an environment variable:

```yaml
state:
  data_dir: "${OPENTALON_DATA_DIR}"
```

## Orchestrator

Control how the LLM is prompted and how user input is pre-processed.

### Rules

Optional custom rules appended to the system prompt (e.g. compliance, terminology):

```yaml
orchestrator:
  rules:
    - "Never send PII to external plugins"
    - "All financial data must stay internal"
```

### Content preparers

Plugin actions that run **before** the first LLM call. Their output becomes the user message sent to the LLM (or they can block the LLM and return a message to the user).

```yaml
orchestrator:
  rules: []
  content_preparers:
    - plugin: hello-world
      action: prepare
      arg_key: text    # optional; default is "text"
```

- **Normal case**: the preparer returns a string → that string is the user message for the LLM. Content preparers are **not** listed as callable tools; the LLM does not see or call them.
- **Guard case**: the preparer returns JSON `{"send_to_llm": false, "message": "..."}` → the orchestrator skips the LLM and sends that message to the user.

See [Hello World plugin](plugins/hello-world-plugin.md) for an example.

## Bundler-style plugins and channels

Instead of a local `path`, you can point a plugin or channel at a GitHub repo and a **ref** (branch, tag, or commit). OpenTalon will clone the repo, build it, and pin the resolved commit in a lock file so installs are reproducible.

| Field   | Description |
|--------|-------------|
| `github` | Repo in the form `owner/repo` (e.g. `opentalon/hello-world-plugin`). |
| `ref`    | Branch, tag, or commit SHA (e.g. `main`, `v1.0.0`, or `abc123def`). |

**Plugins** — use either `path` or `github` + `ref`:

```yaml
plugins:
  hello-world:
    enabled: true
    github: "opentalon/hello-world-plugin"
    ref: "master"
```

**Channels** — use either `plugin` (path or `grpc://...`) or `github` + `ref`:

```yaml
channels:
  slack:
    enabled: true
    github: "opentalon/slack-channel"
    ref: "v0.1.0"
```

- The first run resolves `ref` to a commit, clones into `<state_dir>/plugins/<name>/` or `<state_dir>/channels/<name>/`, runs `go build`, and writes **`plugins.lock`** or **`channels.lock`** under the state dir with the resolved commit and binary path.
- Later runs reuse the locked version until you change `ref` or delete the lock entry. Requires `git` and `go` on the host.

## Full Example

```yaml
models:
  providers:
    anthropic:
      api_key: "${ANTHROPIC_API_KEY}"
      api: anthropic-messages
    openai:
      api_key: "${OPENAI_API_KEY}"
      api: openai-completions
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
    openai/gpt-5.2:
      alias: gpt52
      weight: 40

routing:
  primary: anthropic/claude-haiku-4
  fallbacks:
    - anthropic/claude-sonnet-4
    - openai/gpt-5.2
  pin:
    code: anthropic/claude-sonnet-4
  affinity:
    enabled: true
    store: ~/.opentalon/affinity.json
    decay_days: 30

auth:
  cooldowns:
    initial: 1m
    max: 1h
    multiplier: 5
    billing_max_hours: 24

state:
  data_dir: ~/.opentalon
```

> For the full architecture details, see [docs/design/providers.md](design/providers.md).
