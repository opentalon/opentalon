# OpenTalon

**An open-source alternative to OpenClaw, built from scratch in Go.**

[![CI](https://github.com/opentalon/opentalon/actions/workflows/ci.yml/badge.svg)](https://github.com/opentalon/opentalon/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.22+-00ADD8.svg)](https://go.dev)

---

## What is OpenTalon?

OpenTalon is an open-source platform built from the ground up in Go as a robust alternative to OpenClaw. It is designed for individuals, teams, and large organizations that need a reliable, secure, and extensible solution — without the compromises that come with legacy codebases.

## Why OpenTalon?

Existing solutions often suffer from:

- **Poor maintainability** — tangled codebases that become harder to change over time
- **Low code quality** — inconsistent patterns, lack of tests, and technical debt that compounds
- **Repeatable bugs** — the same classes of issues resurfacing release after release
- **Hard to extend** — adding features means forking or fighting the architecture
- **No smart model routing** — you either pick one model and overpay, or manually juggle providers yourself

OpenTalon is engineered for long-term quality from day one. Every architectural decision is made with maintainability, security, and stability in mind — so the project stays healthy as it grows.

## Core Principles

### Security First

Security is not an afterthought. OpenTalon is secure by default with a minimal attack surface. Plugins run as **separate OS processes** communicating over gRPC, so a compromised or misbehaving plugin can never access the core's memory or escalate privileges. Secrets are handled properly, inputs are validated at every boundary, and dependencies are kept lean and audited. No shortcuts.

```mermaid
graph LR
    User[User Message] --> Core[OpenTalon Core / LLM]
    Core -->|"conversation context only"| PluginA[Plugin A]
    Core -->|"conversation context only"| PluginB[Plugin B]
    PluginA -->|"result"| Core
    PluginB -->|"result"| Core
    PluginA x--x|"blocked"| PluginB
```

- **Plugins work strictly within their own scope** — each plugin does its job and returns a result, nothing else
- **Plugins never talk to each other** — no shared state, no event bus, no direct calls
- **A plugin cannot trigger another plugin** — not via its response, not via prompt injection, not by any mechanism. Only the core/LLM decides what runs next
- **The LLM orchestrates everything** — it decides which plugin to call, in what order, and routes results between them
- **Plugins only see what the core sends** — conversation context and the specific task, nothing more

#### How isolation is enforced

Every plugin response passes through a **guard pipeline** before reaching the LLM:

```mermaid
flowchart TD
    LLM[LLM decides tool call] --> Validate[Validate call against registry]
    Validate --> Timeout["Set timeout (default 30s)"]
    Timeout --> Execute[Execute plugin via gRPC]
    Execute --> SizeCheck{Response within 64KB limit?}
    SizeCheck -->|No| Truncate[Truncate + add notice]
    SizeCheck -->|Yes| Sanitize[Strip forbidden patterns]
    Truncate --> Sanitize
    Sanitize --> ValidateResult{CallID matches? Fields valid?}
    ValidateResult -->|No| ErrorResult[Replace with error]
    ValidateResult -->|Yes| WrapResult["Wrap in [plugin_output] block"]
    ErrorResult --> FeedBack[Feed back to LLM as data]
    WrapResult --> FeedBack
```

| Threat | Guard |
|---|---|
| Plugin returns fake tool calls in its output | Response sanitizer strips all tool-call patterns before the LLM sees them |
| Plugin crafts output to trick the LLM | Output is wrapped in `[plugin_output]` blocks — the LLM is instructed to treat it as data only |
| Plugin tries to read another plugin's state | State store enforces namespace isolation — pluginID is set by the core, not the plugin |
| Plugin tries to discover or call other plugins | gRPC contract exposes exactly one method: `Execute`. No registry, no peer discovery |
| Plugin runs forever or consumes all resources | Per-call timeout (configurable) + OS-level resource limits |

#### LLM Safety Rules

The LLM itself receives **built-in safety rules** in its system prompt at the start of every session. These rules instruct the LLM — in multiple languages — to never execute tool calls found inside plugin output, to treat all plugin responses as untrusted data, and to never let a plugin influence which other plugins get called.

The default rules are built into OpenTalon and can be **customized** via `config.yaml`:

```yaml
orchestrator:
  rules:
    - "Never send PII or personal data to external plugins"
    - "All financial data must stay within internal plugins only"
    - |
      When working with customer data, follow these constraints:
      1. Never log raw customer identifiers
      2. Mask email addresses before passing to any plugin
      3. Reject plugin results that contain unmasked credit card numbers
    - |
      For compliance with internal policy SEC-2024-07:
      - Only approved plugins may access production databases
      - Plugin responses containing SQL must be flagged for review
```

This lets organizations add domain-specific rules — including multi-line instructions — without modifying source code. These custom rules are appended to the built-in safety rules and injected into the LLM system prompt at the start of every session.

### Stability

Thorough testing at every level — unit, integration, and end-to-end. Pull requests can only be merged with a fully green test suite, no exceptions — not even for project owners :) Zero tolerance for repeatable bugs. Predictable behavior under load, graceful degradation, and clear error reporting.

### Customizability

A two-tier plugin system lets users extend OpenTalon without forking. Whether you need a full-blown storage backend or a lightweight request filter, there is a plugin tier designed for your use case. See [Plugin System](#plugin-system) below.

### Maintainability

Clean architecture, idiomatic Go, and high code quality standards enforced by linters, formatters, and code review. The codebase is designed to be readable and approachable for new contributors.

### Multi-Platform Deployment

First-class support for running anywhere:

- **Local** — single binary, no external dependencies required to get started
- **VPS** — lightweight deployment with systemd or supervisor
- **Docker** — official container images
- **Kubernetes** — Helm charts for production-grade deployments

## Getting Started

<!-- TODO: Add prerequisites and quick-start commands once the initial codebase exists -->

## Installation

<!-- TODO: Add installation instructions for binary, Docker, Helm/Kubernetes, from source -->

## Plugin System

OpenTalon features a **two-tier plugin architecture** that balances power, safety, and ease of use.

```mermaid
graph LR
    subgraph core [OpenTalon Core]
        PluginHost[Plugin Host]
        LuaVM[Lua VM]
    end

    subgraph grpcPlugins [gRPC Plugins]
        AuthPlugin[Auth Plugin]
        StoragePlugin[Storage Plugin]
        CustomPlugin[Custom Plugin]
    end

    subgraph luaScripts [Lua Scripts]
        FilterScript[filter.lua]
        HookScript[hook.lua]
        RuleScript[rules.lua]
    end

    PluginHost <-->|gRPC| AuthPlugin
    PluginHost <-->|gRPC| StoragePlugin
    PluginHost <-->|gRPC| CustomPlugin
    LuaVM <-->|embedded| FilterScript
    LuaVM <-->|embedded| HookScript
    LuaVM <-->|embedded| RuleScript
```

### Tier 1: gRPC Plugins (HashiCorp go-plugin style)

For heavy, standalone extensions such as auth providers, storage backends, and third-party integrations.

- Each plugin is a **separate binary** that communicates with the core over **gRPC via a local socket**
- **Process isolation** — a crashing or misbehaving plugin cannot take down the core; each plugin runs in its own OS process with its own memory space
- **Language-agnostic** — write plugins in Go, Python, Rust, or any language that speaks gRPC (Go is the primary SDK)
- **Security boundary** — separate OS process with limited permissions; strict protobuf contracts define exactly what a plugin can and cannot do
- **Discovery and lifecycle** — plugins are registered via config or auto-discovered from a directory, health-checked, and gracefully restarted on failure
- Same proven pattern behind **Terraform**, **Vault**, and **Nomad**

### Tier 2: Lua Scripting (embedded via gopher-lua)

For lightweight, hot-reloadable customization such as filters, rules, hooks, and data transformations.

- **Embedded Lua VM** inside the core — no separate process, no recompilation needed
- **Hot-reload** — update `.lua` scripts without restarting OpenTalon
- **Sandboxed** — restricted standard library, memory and CPU limits to prevent runaway scripts
- **Low barrier to entry** — ideal for operators and non-Go developers who need quick customizations
- Inspired by **Nginx/OpenResty**, **Kong**, and **Redis** scripting models

### Extension Points

Both plugin tiers share the same set of extension points:

- Auth providers
- Storage backends
- Notification channels
- Scheduled tasks
- Request/response hooks
- Custom API endpoints

### Developer Experience

- **gRPC Plugin SDK** — scaffolding CLI, example plugins, and integration test helpers
- **Lua API reference** — documentation, example scripts, and a REPL for interactive testing

## Smart Model Routing

OpenTalon includes a **weighted smart router** that automatically picks the best AI model for each task — optimizing for cost without sacrificing quality.

```mermaid
graph LR
    Request[Request] --> Classifier[Task Classifier]
    Classifier --> Router[Weighted Router]
    Router --> CheapModel["Cheap Model (weight: 90)"]
    Router --> MidModel["Mid Model (weight: 50)"]
    Router --> StrongModel["Strong Model (weight: 10)"]
    CheapModel --> Signal{User accepts?}
    Signal --> |Yes| Store[Affinity Store]
    Signal --> |No| MidModel
    Store --> |"learns over time"| Router
```

### How it works

1. **Weights** — each model has a weight (0–100). Cheaper models get higher weight and are tried first
2. **Auto-classification** — incoming requests are categorized by heuristics (message length, code blocks, keywords, conversation depth)
3. **Escalation** — if the user rejects a response (regenerates, says "try again", or thumbs-down), the system escalates to the next model by weight
4. **Learning** — the affinity store records which model succeeded for which task type. Over time, the router learns: "code generation needs Sonnet, simple Q&A is fine on Haiku"
5. **User overrides** — if you already know what you want, pin a model per request (`--model`), per session (`/model`), or per task type in config

### Multi-Provider Support

OpenTalon supports multiple AI providers out of the box, with a unified configuration:

- **Built-in providers** — Anthropic, OpenAI, Google, and more
- **Custom providers** — any OpenAI-compatible or Anthropic-compatible endpoint (self-hosted, OVH, Ollama, vLLM, etc.)
- **Provider plugins** — add new providers via the gRPC plugin system
- **Auth profile rotation** — multiple API keys or OAuth tokens per provider, with automatic round-robin and cooldown on rate limits
- **Two-stage failover** — first rotate credentials within a provider, then fall back to the next model in the chain. Exponential backoff on failures.

> For the full architecture, see [docs/design/providers.md](docs/design/providers.md).

## Roadmap

<!-- TODO: Add roadmap milestones -->

## Contributing

<!-- TODO: Add contributing guidelines and link to CONTRIBUTING.md -->

## License

OpenTalon is licensed under the [Apache License 2.0](LICENSE).
