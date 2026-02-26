# Channel Plugin Framework

## Overview

The channel framework provides a **platform-agnostic** abstraction for external messaging systems to plug into OpenTalon. The core defines interfaces and contracts; actual implementations (Slack, Teams, Telegram, WhatsApp, Discord, Jira, Matrix, etc.) live in **separate repositories**.

```
┌──────────────────────────────────────────────────────────┐
│                    OpenTalon Core                         │
│                                                          │
│  ┌─────────────┐   ┌──────────────┐   ┌──────────────┐  │
│  │   Channel    │──▶│     LLM      │──▶│    Tool      │  │
│  │   Registry   │◀──│ Orchestrator │◀──│   Registry   │  │
│  └──────┬───────┘   └──────────────┘   └──────────────┘  │
│         │                                                │
│  ┌──────┴───────┐                                        │
│  │   Launcher   │  (auto-detects connection mode)        │
│  └──────┬───────┘                                        │
└─────────┼────────────────────────────────────────────────┘
          │
    ┌─────┴─────┐
    │  Channel   │  (binary | grpc | docker | webhook | ws)
    │  Plugin    │
    └───────────┘
```

## Channel vs Tool plugins

OpenTalon has two distinct plugin categories:

| Aspect | Channel plugin | Tool plugin |
|---|---|---|
| **Purpose** | I/O adapter: brings messages in, delivers responses out | Capability: the LLM calls it to perform actions |
| **Direction** | Bidirectional (user ↔ core) | Request-response (core → plugin) |
| **Lifecycle** | Long-lived connection (stream, WS, or polling) | Per-call invocation |
| **Examples** | Slack, Teams, Telegram, WhatsApp, Discord, Jira, Matrix | GitLab, JIRA issue creation, code search, CI/CD |
| **gRPC contract** | `ChannelService` | `PluginService` |

Both categories are external binaries/services that connect via gRPC (or HTTP/WS for channels). Both are subject to the same security guards.

## Protobuf contract

The gRPC service defined in `proto/channel.proto`:

```protobuf
service ChannelService {
  rpc ReceiveMessages(stream InboundMessage) returns (stream OutboundMessage);
  rpc Capabilities(google.protobuf.Empty) returns (ChannelCapabilities);
}
```

### InboundMessage

A message coming in from a user on an external platform:

| Field | Type | Description |
|---|---|---|
| `channel_id` | string | Which channel instance sent this |
| `conversation_id` | string | Platform-specific conversation/room ID |
| `thread_id` | string | Thread within conversation (empty if unthreaded) |
| `sender_id` | string | Platform user ID |
| `sender_name` | string | Human-readable sender name |
| `content` | string | Message text |
| `files` | FileAttachment[] | Attached files |
| `metadata` | map | Platform-specific key-value pairs |
| `timestamp` | Timestamp | When the message was sent |

### OutboundMessage

A response from the core going back to the user:

| Field | Type | Description |
|---|---|---|
| `conversation_id` | string | Target conversation |
| `thread_id` | string | Target thread |
| `content` | string | Response text |
| `files` | FileAttachment[] | Files to attach |
| `metadata` | map | Platform-specific directives |

### ChannelCapabilities

Advertised by the plugin at startup so the core knows what the platform supports:

| Field | Type | Description |
|---|---|---|
| `id` | string | Plugin identifier |
| `name` | string | Human-readable name |
| `threads` | bool | Supports threaded conversations |
| `files` | bool | Supports file attachments |
| `reactions` | bool | Supports emoji reactions |
| `edits` | bool | Supports message editing |
| `max_message_length` | int64 | Platform's character limit (0 = unlimited) |

## Multi-mode launcher

The core auto-detects how to connect to a channel plugin based on the URI scheme in the `plugin` config field:

| Scheme | Mode | How it works |
|---|---|---|
| `./path` or `/abs/path` | **Binary** | Core spawns the plugin as a subprocess. Communication over gRPC via unix socket (HashiCorp go-plugin pattern). |
| `grpc://host:port` | **Remote gRPC** | Plugin runs independently as a service. Core connects as a gRPC client. |
| `docker://image:tag` | **Docker** | Core pulls the image, runs a container, and connects via mapped gRPC port. |
| `http://` or `https://` | **Webhook** | Stateless HTTP. Plugin pushes inbound messages via POST to core's `/api/channels/inbound`. Core POST-s outbound to the plugin URL. HMAC signatures authenticate both directions. Ideal for serverless (Lambda, Cloud Functions). |
| `ws://` or `wss://` | **WebSocket** | Persistent bidirectional connection. Plugin connects to core's `/api/channels/ws`. Messages flow as JSON frames. Auto-reconnects on drop. |

### When to use which mode

- **Binary** — simplest setup. Good for local dev and single-server deployments. Plugin lifecycle managed by the core.
- **Remote gRPC** — for Kubernetes / microservice architectures where the plugin runs as its own pod/service.
- **Docker** — self-hosted setups wanting isolation without managing binaries manually.
- **Webhook** — serverless deployments. Each request is stateless; the platform (AWS Lambda, GCP Cloud Functions, Vercel) handles scaling.
- **WebSocket** — real-time, lightweight plugins that need a persistent connection without the overhead of full gRPC.

### Webhook flow

```
Plugin                         Core
  │                              │
  │  POST /api/channels/inbound  │
  │  (InboundMessage JSON)       │
  │ ──────────────────────────▶  │
  │                              │── Orchestrator processes
  │  POST plugin_url/outbound    │
  │  (OutboundMessage JSON)      │
  │ ◀──────────────────────────  │
```

Both directions carry an `X-OpenTalon-Signature` HMAC header computed from a shared secret configured per channel.

### WebSocket flow

```
Plugin                             Core
  │                                  │
  │  WS CONNECT /api/channels/ws    │
  │ ─────────────────────────────▶  │
  │  {"type":"identify","id":"..."}  │
  │ ─────────────────────────────▶  │
  │                                  │
  │  JSON InboundMessage             │
  │ ─────────────────────────────▶  │
  │                                  │── Orchestrator processes
  │  JSON OutboundMessage            │
  │ ◀─────────────────────────────  │
```

After connecting, the plugin sends an `identify` frame with its channel ID. Then both sides exchange `InboundMessage` / `OutboundMessage` JSON frames.

## Pre-LLM processing (content preparers)

Before the first LLM call, the user message can be **transformed or blocked** so the LLM only sees the right content—or the request never reaches the LLM. This avoids burning tokens on guard logic and keeps company rules out of the prompt.

**Two layers:**

1. **Channel-level preparer** (optional) — A channel plugin can register a `ContentPreparer` for its channel ID (see `pkg/channel`: `RegisterContentPreparer`). When a message arrives, the handler runs this preparer first. It receives the raw content and can call plugin actions or Lua; it returns the string that is then passed to the orchestrator. Used for channel-specific normalization or guards.

2. **Orchestrator content preparers** (config-driven) — In `orchestrator.content_preparers` you list plugin actions or Lua scripts that run **in order** before the LLM. Each entry is either:
   - A **tool plugin** action (e.g. `plugin: hello-world`, `action: prepare`): the core invokes that action with the current content (e.g. as `args.text`). The plugin returns either new content (string) or a JSON object `{"send_to_llm": false, "message": "..."}` to skip the LLM and show a message to the user.
   - A **Lua script** (e.g. `plugin: lua:hello-world`): the core runs the script’s `prepare(text)` function; it returns a string (new content) or a table `{ send_to_llm = false, message = "..." }` to block.

Order is significant: the first entry in the list receives the user message first; each preparer's output is passed to the next. The output of one preparer becomes the input of the next. If any preparer returns “do not send to LLM”, the pipeline stops and the user sees the given message—no LLM call. Otherwise the final string is what the LLM sees as the user message.

**Invoke (skip LLM, run plugin steps):** A preparer can also tell the orchestrator to **skip the LLM** and run one or more **plugin actions** directly. Return `send_to_llm: false` and an `invoke` value:

- **Plugin (JSON):** `{"send_to_llm": false, "invoke": {"plugin": "gitlab", "action": "deploy", "args": {"branch": "one", "env": "staging"}}}` for a single step, or `"invoke": [ step1, step2, ... ]` for multiple steps.
- **Lua:** Return a table with `send_to_llm = false` and `invoke = { plugin = "...", action = "...", args = { ... } }` (single step) or `invoke = { step1, step2, ... }` (array of step tables). Each step table has `plugin`, `action`, and optional `args` (table of string key/value).

The orchestrator runs each step in order. The **output (content) of the previous step** is injected into the next step's args under the reserved key **`previous_result`**. The final step's content is shown to the user. Invalid or missing plugin/action for a step is skipped (logged); on step error, the user sees that error. This supports flows like "rebase branch with master and deploy to staging" where a preparer (or Lua) owns context and drives a short pipeline without the LLM.

This is where company rules, vocabulary enforcement, and compliance checks can run **without burning LLM tokens**. See the [Lua scripts](../lua-scripts.md) guide and the [hello-world plugin](https://github.com/opentalon/hellow-world-plugin) for examples.

**Insecure preparers:** Preparers are **insecure by default** (they cannot run invoke). This is only about invoking other plugins: an insecure preparer can change the message and block with a message, but **cannot** run invoke steps. To allow a plugin to run invoke when used as a preparer, set `insecure: false` (trusted) in that plugin's config.

**How this fits with orchestration:** The only place the core does a **pre-check call** to a plugin (or Lua) before the LLM is this content-preparer pipeline. All other plugin invocations are **orchestration-driven**: the LLM decides which tools to call during the turn, and the core runs those actions in the agent loop. There is no separate “after” hook—the response to the user is the LLM’s final output (and any tool results). So “before” in config is the only special pipeline that runs plugins/Lua ahead of the LLM; everything else is normal tool use decided by the LLM.

## Actor and permission plugin

Every request that comes from a channel carries an **actor** identifier (who is acting). The actor is channel-specific: e.g. Slack user ID, Teams user ID, Discord ID, WhatsApp phone number. The core derives it from the inbound message as `channel_id:sender_id` and attaches it to the request context. When a request does not come from a channel (e.g. scheduler or RunAction), no actor is set.

**Permission plugin:** The core can be configured with a **permission plugin** (e.g. `orchestrator.permission_plugin: permission`). Before running any tool, the core calls this plugin with a fixed action name (`check`) and args `actor`, `plugin` (the plugin the actor wants to use). The permission plugin **gets the request** and decides using its own config or code (e.g. who can use which plugin); it returns allow or deny. The core enforces the result: if deny, the tool is not run and the user sees "permission denied". If no permission plugin is configured, all tool runs are allowed. If no actor is set (e.g. scheduler), the permission check is skipped and the run is allowed. The permission plugin itself is never gated by permission.

**Contract:** The permission plugin exposes an action (the core uses the fixed name `check`). Input args: `actor` (string), `plugin` (string). Output: a string interpreted as allow or deny; `"true"` or JSON `{"allowed": true}` = allow, otherwise deny. On plugin error or timeout, the core denies the run.

## Session mapping

Each unique conversation thread maps to one orchestrator session:

```
Session key = <channel_id>:<conversation_id>:<thread_id>
```

- If `thread_id` is empty, the key is `<channel_id>:<conversation_id>`.
- The registry auto-creates a new session when it encounters a key it hasn't seen before.
- Sessions persist across messages, so the LLM maintains full conversation context within a thread.

## Configuration

```yaml
channels:
  my-slack:
    enabled: true
    plugin: "./plugins/opentalon-slack"                       # binary
    config:
      app_token: "${SLACK_APP_TOKEN}"
      bot_token: "${SLACK_BOT_TOKEN}"
  my-telegram:
    enabled: true
    plugin: "grpc://telegram-bot.internal:9001"               # remote gRPC
    config:
      bot_token: "${TELEGRAM_BOT_TOKEN}"
  my-teams:
    enabled: true
    plugin: "docker://ghcr.io/opentalon/plugin-teams:latest"  # docker
    config:
      tenant_id: "${TEAMS_TENANT_ID}"
  my-whatsapp:
    enabled: true
    plugin: "https://us-central1-proj.cloudfunctions.net/wa"  # webhook
    config:
      verify_token: "${WA_VERIFY_TOKEN}"
  my-custom:
    enabled: true
    plugin: "wss://custom-bridge.example.com/channel"         # websocket
    config:
      api_key: "${CUSTOM_API_KEY}"
```

The `config` block is **opaque to the core**. For binary/docker modes it is forwarded to the plugin at launch. For remote/webhook/websocket modes the remote service manages its own config.

Environment variables are expanded at parse time via `${VAR}` syntax.

## Plugin lifecycle

1. **Startup** — core reads `channels` config, calls `DetectMode()` for each entry, launches or connects accordingly.
2. **Capabilities** — core calls `Capabilities()` to learn what the plugin supports.
3. **Message loop** — core listens on the inbound channel. Each message is dispatched to the orchestrator via `MessageHandler`. Responses are routed back.
4. **Health-check** — periodic liveness probes. Unhealthy plugins are restarted (binary/docker) or reconnected (gRPC/WS).
5. **Shutdown** — `Registry.StopAll()` sends graceful stop signals and waits for all goroutines to drain.

## File handling

When a channel plugin sends a file attachment:

1. The `FileAttachment` is included in the `InboundMessage`.
2. The orchestrator can pass file content to tool plugins as needed.
3. Outbound files (e.g., generated reports) are attached to `OutboundMessage` and the channel plugin delivers them to the platform.

Platforms with size limits advertise `max_message_length` in capabilities; the core can split or truncate accordingly.

## Building a channel plugin

A channel plugin is a standalone program that:

1. Implements the `ChannelService` gRPC interface (for binary/gRPC/docker modes) **or** speaks JSON over HTTP/WebSocket (for webhook/WS modes).
2. Handles platform-specific authentication (OAuth, bot tokens, etc.) internally.
3. Translates platform messages into `InboundMessage` and `OutboundMessage`.
4. Advertises its capabilities via the `Capabilities` RPC.

The same protobuf contract works for any platform. The core never knows or cares what platform a channel plugin adapts — it only speaks the generic contract.

### Example: adapting any platform

```
Your platform SDK               Your plugin binary              OpenTalon Core
       │                              │                              │
       │  platform-specific event     │                              │
       │ ──────────────────────────▶  │                              │
       │                              │  InboundMessage (gRPC/HTTP)  │
       │                              │ ──────────────────────────▶  │
       │                              │                              │── LLM
       │                              │  OutboundMessage             │
       │                              │ ◀──────────────────────────  │
       │  platform API call           │                              │
       │ ◀──────────────────────────  │                              │
```

This pattern applies identically to Slack, Teams, Telegram, WhatsApp, Discord, Jira, Matrix, email, SMS, or any custom system.

## State and memory

When `state.data_dir` is set, OpenTalon uses a SQLite database (`state.db`) for **memories** and **sessions**. Both persist across restarts.

- **Memories** are scoped as **general** (shared; `actor_id` NULL) or **per-actor** (one row per actor). The LLM receives general context (config rules, general stored rules, tools, workflow memories) plus the current actor’s stored rules. Workflow summaries are stored per-actor by default. General memory is the place for **admin-created shared rules**; the write path for general memory (config bootstrap, permission plugin, or admin API) can be restricted in a future release.
- **Sessions** are one per conversation (e.g. channel + conversation or thread). Each session holds message history (and an optional **summary** after summarization). Session limits: `session.max_messages` (cap messages per session), `session.max_idle_days` (prune idle sessions on startup). Optional **summarization**: after `session.summarize_after_messages` messages, the LLM compresses history into a summary and only the last `session.max_messages_after_summary` messages are kept, so token usage stays bounded. The summarization prompts are configurable (`session.summarize_prompt` and `session.summarize_update_prompt`) so they can be in any language; if empty, default English is used.

Plugins can have their own SQLite DB under `data_dir/plugin_data/<name>.db` and optional migrations in `<plugin_path>/migrations/*.sql`; the core runs those migrations at load and never gives plugins access to the main `state.db`.
