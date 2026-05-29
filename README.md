# OpenTalon

**Open-source enterprise AI orchestration — built in Go to solve real enterprise problems, not toy demos.**

[![CI](https://github.com/opentalon/opentalon/actions/workflows/ci.yml/badge.svg)](https://github.com/opentalon/opentalon/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.24+-00ADD8.svg)](https://go.dev)

> 📚 **Looking for setup, deployment, scheduler, extensibility, or other detailed guides?** See the [**`docs/`**](docs/) directory.

---

## The big ideas behind OpenTalon

OpenTalon exists to solve **enterprise** issues — the gap between a chatbot demo and an AI system a real organization can actually depend on. Three ideas drive the design:

- 📘 **[Enterprise AI Orchestration](https://opakalex.github.io/posts/enterprise-ai-orchestration/)** — why one big LLM call is not an enterprise architecture, and how multi-provider routing, deterministic preprocessing, plugin isolation, and policy enforcement combine into a system that holds up in production.
- 📘 **[Expert-in-the-Loop (EITL)](https://opakalex.github.io/posts/expert-in-the-loop/)** — moving past "human-in-the-loop" rubber-stamping toward workflows where domain experts encode rules, gates, and review steps the system enforces deterministically — implemented in OpenTalon via [Talon workflows & EITL rules](#talon-workflows--eitl-rules).
- 🛠️ **LLMs write code, the runtime keeps it safe** — adopting the same insight behind Cloudflare's [Code Mode for MCP](https://blog.cloudflare.com/code-mode/) (LLMs are dramatically better at writing programs than at orchestrating long chains of tool calls). OpenTalon lets the LLM emit **Talon scenarios** in a deliberately restricted DSL instead of raw tool calls. The grammar physically cannot express unsafe operations, so the sandbox is structural — not a policy you hope the model follows. See [Why LLMs write Talon scenarios, not raw tool calls](#why-llms-write-talon-scenarios-not-raw-tool-calls).

Everything below is in service of these three ideas.

## What is OpenTalon?

OpenTalon is an open-source platform built from the ground up in Go for **enterprises that need AI in production**: predictable behaviour, auditable boundaries, deterministic business rules, multi-provider economics, and expert-defined guardrails. It is designed for teams and large organizations that need a reliable, secure, and extensible solution — without the compromises that come with bolted-on prompt scripts or legacy codebases.

## Why OpenTalon? (the enterprise problems it solves)

Most "AI integrations" inside large organizations hit the same walls:

- **No separation of business logic from AI** — company-specific transformations (terminology, routing, validation) are tangled into prompts instead of handled by deterministic, testable code
- **Every rule burns LLM tokens** — business vocabulary, compliance checks, and formatting rules all get stuffed into the prompt, inflating cost and latency on every single request
- **No smart model routing** — you either pick one model and overpay, or manually juggle providers yourself
- **No expert-in-the-loop story** — domain experts have no way to encode rules, gates, or review steps that the system actually enforces (see the [EITL essay](https://opakalex.github.io/posts/expert-in-the-loop/))
- **Weak isolation between integrations** — one buggy or compromised tool can poison the LLM's context and pivot into others
- **Poor maintainability, repeatable bugs, hard to extend** — tangled codebases, inconsistent patterns, and architectures that fight you when you try to add features

OpenTalon is engineered for long-term quality from day one. Every architectural decision is made with **enterprise** maintainability, security, and stability in mind.

## Core Principles

Two principles drive the architecture. Everything else — extensibility, the scheduler, smart routing, persistence — is implementation in service of these.

### 1. Security & isolation first

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

> **Deep dive:** see [docs/security-guardrails.md](docs/security-guardrails.md) for the full guard pipeline, isolation enforcement, content preparers, prompt-injection defence, and LLM safety rules.

### 2. Deterministic business logic — without burning LLM tokens

Enterprise rules and context belong in code, not prompts. OpenTalon's preprocessing pipeline lets organizations enforce vocabulary, apply business rules, run compliance checks, and pull in relevant context (RAG, databases, internal APIs) **before** the message ever reaches the main LLM — through pluggable preparers and hooks that run as part of the request lifecycle.

- **Vocabulary enforcement** — rewrite non-standard terms into company-approved language. No tokens burned on terminology drift.
- **Business rule classification** — route, prioritize, or reject requests using deterministic rules. The LLM never sees what it doesn't need to see.
- **Compliance checks** — detect PII, credentials, or policy violations and block them at the door.
- **Context enrichment & RAG** — inject company metadata (project codes, team names, priority levels) and retrieve relevant documents from internal knowledge bases. The LLM gets the right context without burning tokens to figure it out, and RAG sources are managed as plugins like any other integration.
- **Business transformation** — convert LLM output into structured actions (Jira tickets, calendar events, CRM updates) deterministically.

Implementation is language-agnostic (see [Extensibility](docs/extensibility.md)); the principle is that this work happens in deterministic code, not in the prompt. For ambiguous cases, preparers can call a small/cheap LLM internally for lightweight classification — the main (expensive) LLM only sees clean, pre-processed input with the right context already attached.

## MCP (Model Context Protocol) Support

OpenTalon supports the **Model Context Protocol (MCP)**, allowing it to connect to any MCP-compatible tool server alongside its native gRPC plugins.

The **[opentalon/mcp-plugin](https://github.com/opentalon/mcp-plugin)** acts as an MCP bridge: it runs as a standard OpenTalon gRPC tool plugin and transparently proxies tool calls to one or more MCP servers over HTTP+SSE. From the core's perspective it is just another plugin; from the MCP server's perspective it is a standard MCP client.

```
OpenTalon Core  ──gRPC──▶  mcp-plugin  ──HTTP+SSE──▶  MCP Server A
                                        ──HTTP+SSE──▶  MCP Server B
```

Configure it like any other plugin:

```yaml
plugins:
  mcp:
    enabled: true
    github: "opentalon/mcp-plugin"
    ref: "master"
    config:
      servers:
        - name: my-mcp-server
          url: "http://localhost:8080/sse"
```

Each tool exposed by the MCP servers is automatically registered in OpenTalon's tool registry and becomes callable by the LLM. All security guards (response sanitization, size limits, timeouts, namespace isolation) apply to MCP tool results exactly as they do to native gRPC plugins.

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

> **Getting started?** See the [Configuration Guide](docs/configuration.md). For the full architecture, see [docs/design/providers.md](docs/design/providers.md).

## Talon workflows & EITL rules

> Conceptual background: [**Expert-in-the-Loop**](https://opakalex.github.io/posts/expert-in-the-loop/) — why enterprise AI needs domain experts encoding deterministic rules and gates, not just humans rubber-stamping LLM output.

"Human-in-the-loop" usually means a human clicks **approve** on whatever the LLM produces. That works for demos. It breaks at enterprise scale, because:

- Humans don't review every call — they rubber-stamp the easy cases and miss the dangerous ones
- The rules a domain expert actually cares about (approval thresholds, region restrictions, customer tier policies, compliance flags) live in their head, not in code
- The LLM gets blamed when really the system never gave the expert a way to *encode* those rules

**Expert-in-the-Loop (EITL)** flips the relationship. Domain experts write the rules once, in a deterministic language; the system enforces them every time without asking a human to babysit.

### Why LLMs write Talon scenarios, not raw tool calls

The other half of the idea: **LLMs are at their best when they write code.** They are markedly weaker when forced to orchestrate long chains of individual tool calls — drift accumulates, prompt-injection surface explodes, and you can't audit what the model "decided" to do. Cloudflare's [**Code Mode for MCP**](https://blog.cloudflare.com/code-mode/) makes exactly this bet: instead of having the model issue tool calls one at a time, expose the MCP surface as a typed API and let the model write a small program against it, executed inside a sandboxed V8 isolate.

OpenTalon applies the same bet with **Talon**:

- Instead of emitting a sequence of tool calls, the LLM writes a **Talon scenario** — a small program in a domain-specific language that the orchestrator compiles and executes.
- Talon is **deliberately restricted**. No general I/O, no arbitrary process execution, no shell, no `eval`. The only actions available inside Talon are the ones the host orchestrator explicitly exposed — so the same `user_only`, sanitization, namespace, and isolation guarantees that protect individual tool calls also protect Talon programs by construction.
- This shifts the safety boundary from *"can we trust the LLM to behave?"* to *"can the language even express the unsafe thing?"* — and the answer is no, because the grammar doesn't include it.
- Experts and the LLM speak the **same restricted language**, against the same surface. Experts define policy in Talon; the LLM proposes scenarios in Talon; the runtime is the only thing that decides what runs.

The LLM gets to use what it's actually good at (writing code). The enterprise gets a sandbox that physically cannot execute the dangerous thing.

### How OpenTalon implements EITL

The [**talon-plugin**](https://github.com/opentalon/talon-plugin) installs like any other OpenTalon plugin — no special orchestrator config knob. It uses the [`talon-language` Go SDK](https://github.com/opentalon/talon-language/tree/master/pkg/talon) to compile and execute **Talon source**: a small, expert-readable language for describing multi-step workflows, approval gates, conditional branches, and rule-driven escalation.

Inside a Talon workflow, every MCP/tool call is routed **back through the host orchestrator** via OpenTalon's **bidirectional gRPC** host-callback contract (`ExecuteBidi`) — so each step picks up the same policy, observability, credential injection, and security guards as a normal tool call. Workflows are not a backdoor around OpenTalon's controls; they are a deterministic *driver* sitting on top of them.

```mermaid
flowchart LR
    Expert["Domain expert"] -->|"writes rules"| TalonSrc["Talon source (VCS)"]
    LLM["LLM proposes action"] --> Workflow["Talon workflow"]
    TalonSrc --> Workflow
    Workflow -->|"approve"| Exec["Execute via orchestrator"]
    Workflow -->|"block / escalate"| Human["Human review"]
    Exec --> Guards["Same security guards as any tool call"]
```

### What this looks like in practice

- **Approval gates** — "any refund above €5,000 routes to a regional manager; under €5,000 auto-approves with audit trail"
- **Compliance rules** — "if customer is in the EU and the action involves PII, require GDPR review step"
- **Multi-step orchestration** — "open AppSignal incident, file Jira, draft MR, post status to #ops — but stop and alert if any step touches production data without an active change window"
- **Tier-based routing** — "enterprise tier customers go to senior agents; SMB tier handled by the agent loop with policy guardrails"

The expert writes those rules in Talon. The LLM proposes actions. The Talon workflow decides what runs, what blocks, and what needs human review — deterministically, the same way every time, auditable line by line.

### Why this is enterprise-grade

- **Deterministic** — same input → same outcome. No prompt drift, no model upgrade surprises.
- **Auditable** — every rule is a piece of source code, in version control, reviewable in PRs.
- **Owned by domain experts** — the people who know the policy can read and write it, without going through engineering for every change.
- **Compatible with the rest of OpenTalon** — Talon steps still go through guards, isolation, sanitization, and `user_only` enforcement.

See the [talon-plugin repo](https://github.com/opentalon/talon-plugin) and [talon-language](https://github.com/opentalon/talon-language) for the language reference and examples.

## Documentation

Most operational details live in [`docs/`](docs/). Highlights:

| Topic | Doc |
|---|---|
| Security & Guardrails — isolation, content preparers, prompt-injection defence, LLM safety rules | [docs/security-guardrails.md](docs/security-guardrails.md) |
| Configuration reference | [docs/configuration.md](docs/configuration.md) |
| Extensibility — plugins, channels, hooks | [docs/extensibility.md](docs/extensibility.md) |
| Slash commands | [docs/slash-commands.md](docs/slash-commands.md) |
| Scheduler — periodic background jobs | [docs/scheduler.md](docs/scheduler.md) |
| State & context persistence | [docs/state.md](docs/state.md) |
| Concurrency — session parallelism | [docs/concurrency.md](docs/concurrency.md) |
| Lua scripts | [docs/lua-scripts.md](docs/lua-scripts.md) |
| Workflows | [docs/workflows.md](docs/workflows.md) |
| Profiles & multi-tenancy | [docs/profiles.md](docs/profiles.md) |
| Kubernetes deployment & health probes | [docs/deployment-guide-k8s.md](docs/deployment-guide-k8s.md) |
| Cluster mode | [docs/cluster.md](docs/cluster.md) |
| Prometheus metrics | [docs/prometheus-metrics.md](docs/prometheus-metrics.md) |
| WebSocket integration | [docs/websocket-integration.md](docs/websocket-integration.md), [docs/websocket-messages.md](docs/websocket-messages.md) |
| VCR cassettes (for contributors) | [docs/vcr-cassettes.md](docs/vcr-cassettes.md) |
| Architecture & design notes | [docs/design/](docs/design/) |

## Contributing

We welcome contributions of all kinds — bug reports, feature requests, documentation improvements, and code.

1. **Report issues** — found a bug or have an idea? [Open an issue](https://github.com/opentalon/opentalon/issues/new) with a clear description and steps to reproduce
2. **Submit pull requests** — fork the repo, create a feature branch, and open a PR against `master`. Keep PRs focused on a single change
3. **Write tests** — every PR must include tests. The CI pipeline runs `go test -race ./...` and `golangci-lint` — both must pass before merging
4. **Follow conventions** — idiomatic Go, `gofmt`-formatted, meaningful commit messages. Read the existing code to match the style
5. **Discuss first** — for large changes or new features, open an issue to discuss the approach before writing code

### System prompt convention

All LLM system prompt text lives in `internal/prompts/*.txt` and is loaded via `//go:embed`. Never write a system prompt as a plain Go string literal — add a `.txt` file and export it through `internal/prompts/prompts.go`. This keeps prompts diffable and hashable for VCR cassette staleness checks.

### Structural integration tests

`internal/orchestrator/integration_test.go` (build tag `integration`) hits the real Anthropic API at temperature=0 and asserts structural properties: correct tool selection, no hallucinated plugins, no max-iteration breach, no parser errors. Run before a release:

```bash
ANTHROPIC_API_KEY=<key> make test-structural
# OpenRouter tests also run if OPENROUTER_API_KEY is set
```

These are gated in CI by `.github/workflows/release-gate.yml` on every release and push to `release/*` branches.

### Eval framework

`internal/eval/` contains a YAML-driven scenario runner with baseline tracking. Scenarios live in `internal/eval/scenarios/*.yaml`. Add new scenarios there without touching Go code.

```bash
ANTHROPIC_API_KEY=<key> make eval
```

Pass rate is checked against a baseline stored in `.eval-baselines/<git-tag>.json`. On the first run for a given tag the baseline is saved; subsequent runs fail if pass rate regresses. The eval runs on minor/major releases only (patch releases skip it).

> Re-recording VCR cassettes after prompt or scenario changes is documented in [docs/vcr-cassettes.md](docs/vcr-cassettes.md).

All contributions are subject to the [Apache 2.0 License](LICENSE).

## License

OpenTalon is licensed under the [Apache License 2.0](LICENSE).
