# Security & Guardrails

Security is OpenTalon's leading principle. Four layers of defence sit between an incoming message and the main LLM — and between any plugin response and the LLM that consumes it.

## How isolation is enforced

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
| LLM autonomously installs or modifies skills/plugins | `user_only` actions are hidden from the LLM system prompt and blocked if invoked via an LLM-generated tool call |

**Guard of LLM models:** A plugin can host its **own LLM** (e.g. a small local model or a dedicated API). Used as a content preparer, such a plugin can implement a **guard of LLM models** — for example, classify or validate the request and block or redirect before the main orchestrator LLM is invoked, or enforce which models or providers are allowed. The core only sees the plugin's result (e.g. transformed message or "do not send to LLM"); the plugin's internal use of an LLM stays out of the main token path.

## Content preparers

**Content preparers** are plugin actions that run before the first LLM call. They receive the user's message and can transform it, enrich it, or block it entirely by returning `send_to_llm: false`.

```yaml
orchestrator:
  content_preparers:
    - plugin: opentalon-commands   # runs slash command handling before the LLM
      action: handle
    - plugin: terminology          # rewrites non-standard terms to company vocabulary
      action: normalize
```

A preparer returns plain text (the transformed message) or a JSON response:

```json
{ "send_to_llm": false, "message": "handled without LLM" }
```

```json
{ "send_to_llm": false, "invoke": [
    { "plugin": "jira", "action": "create_issue", "args": { "title": "..." } }
]}
```

When `send_to_llm` is `false`, the LLM is never called — the preparer's message or invoke steps become the response directly. Preparers with `invoke` steps must be explicitly trusted in config (see `insecure: false` in `config.example.yaml`).

## Guard plugins — prompt injection prevention

Regular content preparers run once, on the initial user message. **Guard plugins** go further: they run before **every** LLM call in the agent loop — including after tool results come back — so they can sanitize content that arrives from external systems before the LLM ever sees it.

This is the primary defence against **prompt injection**: a malicious tool response that says _"ignore previous instructions and do X"_ is intercepted and sanitized by the guard before the LLM processes it.

```yaml
orchestrator:
  content_preparers:
    - plugin: injection-guard
      action: sanitize
      guard: true        # ← runs before every LLM call, not just the first
```

Execution flow with a guard:

```
User message
    │
    ▼
[Content preparers]   ← regular preparers run once here
    │
    ▼
Agent loop iteration 1:
    ├─ [Guard plugins]  ← sanitize last message (user input)
    ├─ LLM call
    └─ Tool call → result appended
Agent loop iteration 2:
    ├─ [Guard plugins]  ← sanitize last message (tool result)  ← injection caught here
    ├─ LLM call
    └─ Final answer
```

The guard plugin receives the content of the most recent message (the last tool result or user message) as the `text` argument, and returns the sanitized version. It can also block entirely:

```json
{ "send_to_llm": false, "message": "Prompt injection detected — request blocked." }
```

Guard actions are **never listed in the LLM's tool list** — they are invisible to the model and run transparently as part of the infrastructure.

A guard plugin can use a small/cheap LLM internally to detect subtle injections, or apply deterministic pattern matching — either way the main LLM only sees the clean output.

## LLM safety rules

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
