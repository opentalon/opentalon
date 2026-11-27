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

### Extra request headers

Some endpoints need more than an API key. A typical case is an
OpenAI-compatible server behind an authenticating gateway or proxy that
expects its own token in a header of its choosing. Add those headers under
`headers`:

```yaml
models:
  providers:
    gateway:
      base_url: "https://llm.example.com/v1"
      api: openai-completions
      headers:
        cf-access-token: "${LLM_ACCESS_TOKEN}"
```

How they behave:

- They are sent with every request to that provider, streaming or not, and
  with its health check: the routing health check (`routing.health`) when
  the provider is the primary, the probe after a failure when a profile
  selected it (see [Model from the profile](#model-from-the-profile)).
- Values support `${ENV_VAR}` like the rest of the config. As everywhere
  else, an unset variable keeps the literal `${ENV_VAR}` text and OpenTalon
  warns about it at start-up. A variable that is set to an empty string
  gives an empty value, and **a header with an empty value is not sent at
  all**, so no empty token goes out.
- They are applied after the provider's own headers. A configured
  `Authorization` replaces the default `Bearer <api_key>` (OpenAI-compatible),
  and a configured `x-api-key` replaces the default key (Anthropic). An empty
  value never removes a default header. Without an `api_key` and without a
  configured `Authorization`, no `Authorization` header is sent, as before.
- Header names are not case-sensitive; Go writes them in its usual form
  (`cf-access-token` goes out as `Cf-Access-Token`).
- Without `headers`, requests are exactly what they were before.

OpenTalon refuses to start when a header is invalid:

- an empty name, a name that is not a valid HTTP header name, or two names
  that differ only in case;
- a value that is not a plain string, or that contains a line break or other
  control character;
- a name OpenTalon or its HTTP client sets itself: `Content-Type`,
  `Content-Length`, `Host` and `Transfer-Encoding` for every provider, and
  `anthropic-version` for `api: anthropic-messages`;
- a connection-level name: `Connection`, `Keep-Alive`, `Proxy-Connection`,
  `Upgrade`, `TE` and `Trailer`.
- `Accept-Encoding`: setting it would switch off the HTTP client's automatic
  decompression of replies.

Overriding the first group would break the request or be silently ignored;
the connection-level names describe a single network hop and are refused by
HTTP/2. None of these error messages repeat the header value.

Header values are treated like API keys:

- They do not appear in logs, in the `/debug` capture, in session events or
  in error messages, and `/show config` prints them as `[redacted]`.
- If the endpoint echoes a value back (for example
  `invalid token: <value>`), every occurrence in the response is replaced
  with `[redacted]` before OpenTalon logs, records or returns it. This covers
  error replies, replies that are retried, and streamed replies. For a value
  of the form `Bearer <token>`, `Basic <token>` or `Token <token>`, the token
  on its own is replaced too. Values shorter than 8 characters are not
  replaced, because they are too short to be credentials and replacing them
  would garble ordinary text.
- Leading and trailing spaces and tabs are removed from values before they
  are sent; a value that is empty after that is not sent.
- Errors from the HTTP client name the request address without its query
  string, and with any configured value replaced, so a redirect address
  that carries a token does not end up in logs.
- OpenTalon will not follow a redirect to a different origin (scheme, host
  or port) for a provider with `headers`, because the HTTP client would
  forward the headers to that other server. The request fails with an error
  instead. Redirects within the same origin are followed as before, and
  providers without `headers` keep the usual redirect behaviour.

Known limits:

- Treat header values exactly like `api_key`: as secrets. Replacing them in
  replies is a safety net for an endpoint that echoes the exact value, not
  a guarantee. A value that comes back encoded (for example `&` written as
  `\u0026` in JSON), or split across two parts of a streamed reply, is not
  recognised.
- A YAML syntax error found before OpenTalon reads the `headers` map (for
  example an unquoted value that starts with `*`) can quote the text in the
  error, just as for `api_key`. Put values in quotes, as in the example
  above.

### Time limit

```yaml
models:
  providers:
    anthropic:
      api_key: "${ANTHROPIC_API_KEY}"
      api: anthropic-messages
      timeout: 45s
```

- `timeout` is the longest one call to the provider that does not stream
  may take, its retries and their waits included. Without it such a call
  has 120 seconds. A streamed call (`openai-completions` only) is not bounded
  by it.
- It takes a Go duration above 0, such as `45s` or `1m30s`. A `timeout`
  without a value, `null`, `~`, `""`, `0`, a negative duration or a number
  without a unit stops the start with an error naming the key.
- A call that does not stream and reaches it fails like a provider that
  cannot be reached: the primary of a failover chain hands the request to
  its fallbacks, a model from the profile to the default chain.
- Retries count against it. By default a call answered 429, 500, 502, 503
  or 504, or failing on the way, is tried up to 4 times with at most 30
  seconds of waiting; `retry: {max_attempts: 1}` makes one attempt.

### Currency

```yaml
models:
  providers:
    anthropic:
      api_key: "${ANTHROPIC_API_KEY}"
      api: anthropic-messages
      currency: USD
```

- `currency` is the ISO 4217 code of the currency the provider's model
  `cost` prices are in: the currency of its invoice. It takes three
  upper-case letters, such as `USD` or `EUR`. Any other value, and the key
  without a value, `null`, `~` or `""`, stops the start with an error naming
  the key.
- Without it the provider's costs carry no currency, as before.
- Each call's `llm_response` event carries it as `cost_currency`. It also
  decides how a call of a model the profile's tariffs do not name is priced;
  see [Customer price](profiles.md#customer-price).
- Nothing converts between currencies. The usage row's `input_cost` and
  `output_cost` add up the costs of a run's calls whatever their currency.

### Prompt caching (Anthropic)

For an `anthropic-messages` provider, `prompt_cache: true` marks the chat
loop's requests for Anthropic's prompt cache. The system prompt, tools and
conversation that repeat from one model call to the next are then read from
the cache at the cache price instead of the full input price.

```yaml
models:
  providers:
    anthropic:
      api_key: "${ANTHROPIC_API_KEY}"
      api: anthropic-messages
      prompt_cache: true          # default false
      cache_diagnostics: false    # default false
      models:
        - id: claude-haiku-4-5
          cost:
            input: 1.00
            output: 5.00
            cache_write: 1.25     # per 1M tokens written to the cache
            cache_read: 0.10      # per 1M tokens read from the cache
```

How it behaves:

- The system prompt goes out as up to three text blocks: the part every
  conversation with the same plugins shares, the part of one conversation,
  and the part that may change from round to round. The first block carries
  a cache marker, and the request carries Anthropic's automatic marker, which
  caches the conversation so far. Entries last Anthropic's default 5 minutes.
- Loading tools adds them to the request's tools and takes them out of the
  tool catalog in the system prompt. Anthropic renders the tools ahead of the
  system prompt and the messages, so the call after a load writes the whole
  prompt to the cache again. On a model with [`inline_tools`](#inline-tools)
  a load leaves the tools and the system prompt as they were.
- Only the chat loop's model calls are marked. Session titles, summaries, the
  planner, sub-agents and confirmation messages are sent as before.
- With `prompt_cache: false`, the default, the system prompt is sent as one
  string, as before.
- Anthropic caches a prompt only above a minimum length per model. A shorter
  one is sent without error and not cached.
- `prompt_cache` takes `true` or `false` only; any other value (`yes`,
  `"true"`, `1`) fails loading the config with an error naming the provider.
  OpenTalon also refuses to start when `prompt_cache: true` is set on a
  provider whose `api` is not `anthropic-messages`, or when a model's `cost`
  sets a price but lacks `cache_write` or `cache_read`.

#### Cache diagnostics

`cache_diagnostics: true` asks Anthropic to compare each chat-loop request
with the previous one of the same session and to report where the cacheable
start of the prompt first differs. Every chat-loop request then carries the
`anthropic-beta` header `cache-diagnosis-2026-04-07` and the id of the
previous response in the session (none on the session's first request). The
reason Anthropic reports, for example `system_changed`, `tools_changed` or
`messages_changed`, is recorded as `cache_miss_reason` in the `llm_response`
session event.

- It needs `prompt_cache: true`; without it OpenTalon refuses to start with
  an error naming the provider. Like `prompt_cache`, it takes `true` or
  `false` only.
- A configured `anthropic-beta` header is kept: OpenTalon joins its own
  betas to it (this one, `thinking-binding-controls-2026-08-01` for adaptive
  thinking and `inline-tools-2026-09-15` for inline tools), separated by
  commas.
- The previous response ids are kept in memory, for at most 10,000 sessions
  (the oldest is dropped first). After a restart, after a session's id was
  dropped, or when the previous request ran on another instance, the next
  request sends no id and the comparison starts over. `previous_message_not_found`
  is recorded only when an id was sent that Anthropic no longer knows; the
  request is answered as usual.

#### Inline tools

`inline_tools` in the `features` of a model on an `anthropic-messages`
provider keeps a conversation's tools and system prompt as first sent when
the chat loop loads tools. The call after a load then reads the earlier
prompt from the cache and writes only what follows it.

```yaml
models:
  providers:
    anthropic:
      api_key: "${ANTHROPIC_API_KEY}"
      api: anthropic-messages
      prompt_cache: true
      models:
        - id: claude-sonnet-5-5
          features: [tools, inline_tools]
          thinking: adaptive
```

Every chat-loop request to such a model, from a conversation's first round
on:

- carries the `anthropic-beta` value `inline-tools-2026-09-15`;
- sends in `tools` the always-included tools (`_meta__load_tools` among
  them), then every other tool the session may use, loaded or not, in name
  order and each with `defer_loading: true`. Anthropic keeps such a tool out
  of the model's context until a `tool_addition` names it, so a load leaves
  `tools` as it was. The request carries the definitions of every tool the
  session may use and is larger for it;
- sends a tool catalog that lists every tool of that second part, loaded or
  not, under its own header (prompt `orchestrator_tool_catalog_inline`), so a
  load leaves the system prompt as it was;
- announces each tool that became callable with a request (through
  `_meta__load_tools`, as a companion, or by a call made before it was
  loaded) with a `tool_addition` block that names it, and each tool that
  stopped being callable with a `tool_removal` block. The blocks follow the
  user message or tool result that ended that request, in one system message
  directly before the next assistant message or at the end of the request.
  Every later request sends each announcement again at its place, unchanged.

How the announcements follow the session:

- A tool stops being callable with the first request that no longer offers
  it: it was pushed out of the 40 loaded tools, or demoted. Tools loaded in
  the running turn stay callable until the turn ends, so their
  `tool_removal` comes with the user message that opens the next turn. The
  earlier `tool_addition` stays where it is; a tool loaded again later is
  announced again at that later place.
- A tool loaded again while it is announced is not announced a second time.
- A tool the session may no longer use, or that no plugin offers any more,
  is neither declared nor announced, and its earlier announcements are left
  out. Anthropic then writes the conversation from the first of them to the
  cache once.
- A callable tool that no announcement in the request names, because the
  message that carried it was trimmed or summarised away, was sent for one
  request only (a retry hint), or was stored without one, is announced after
  the request's last user message or tool result. Nothing is inserted
  earlier.
- When OpenTalon cannot read which tools the session has loaded, the request
  offers only the always-included tools and those loaded in the running
  turn, and it withdraws the other announced tools at its end. Nothing of
  that is stored. The next request that reads the loaded tools carries no
  such withdrawal, so the conversation from that place is written to the
  cache once.
- A request ends on the same user message or tool result as the request
  before it when the session store saved no message of the calls in between,
  or saved a call's result without its assistant message so that the result
  is left out (a confirmation prompt and the approval that answers it are
  left out of every request). That message keeps the announcement it was
  sent with. A tool that became callable or stopped being callable since is
  announced after it for that request only, and the request after it
  announces the change after its own last user message or tool result. The
  conversation from that place is written to the cache once.
- A user message stored after the message an announcement follows, such as
  a note added while a call waits for confirmation, comes before the
  announcement from the next request on. The conversation from that place is
  written to the cache once.
- A load by itself does not stop earlier thinking blocks from going back;
  see [Adaptive thinking (Anthropic)](#adaptive-thinking-anthropic).

Which requests take this form:

- A model without the feature and every `openai-completions` provider, also
  with `inline_tools` in a model's `features`, get the callable tools in one
  flat `tools` array and the catalog of the tools the session may use but
  has not loaded, without announcements and without the beta value. So does
  every call outside the chat loop (session titles, summaries, the planner,
  sub-agents, confirmation messages).
- The model that serves a round decides from its own entry. When the default
  chain answers a round for a selected model with the feature (see [When the
  selected provider fails](#when-the-selected-provider-fails)), a default
  model without it gets the flat form, with every loaded tool in its
  `tools`.
- A 400 for this form, for example from an endpoint that does not accept the
  beta value, is a refused request: for a model a profile selects, the
  default chain answers the round with a warning and the provider stays in
  use, so the next round tries it again. Removing `inline_tools` from the
  model is the way back.
- The feature acts only when the chat loop sends tools natively, which a
  model with `features: [tools]` at the provider of `routing.primary` turns
  on. Without that, its requests differ only by the beta value.
- It does not need `prompt_cache: true`. Without it the system prompt is
  sent as one string with the same catalog, and nothing is cached.
- Turning the feature on or off changes the tools and the system prompt of a
  running conversation: its next call writes the whole prompt to the cache
  once and sends none of the earlier thinking blocks. The changes of the
  callable tools are recorded with the messages for every model, so a
  conversation that gets the feature has its tools announced where they were
  loaded.
- An absent `features`, `null` and `[]` turn nothing on, and a name OpenTalon
  does not know (`inline-tools`) is ignored.

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
| `reasoning_effort` | no | Effort the model is asked for: `low`, `medium` or `high`; with `thinking: adaptive` also `xhigh` and `max`. Empty = the provider's default |
| `thinking` | no | `adaptive` on an `anthropic-messages` provider, for Claude 4.6 and later. See "Adaptive thinking (Anthropic)" |
| `features` | no | Capabilities of the model: `tools` (native tool calls), `reasoning` (the request asks for reasoning), `retry_tool_calls` (a round answered with text where the planner expected a tool call is retried), `inline_tools` (see [Inline tools](#inline-tools)). Other names, `null` and `[]` turn nothing on |
| `input` | no | Supported input types (`[text]`, `[text, image]`) |
| `context_window` | no | Max context size in tokens |
| `max_tokens` | no | Max output tokens |
| `cost.input` | no | Cost per 1M input tokens, in the provider's [`currency`](#currency). Used by smart router |
| `cost.output` | no | Cost per 1M output tokens, in the provider's `currency` |
| `cost.cache_write` | no | Cost per 1M tokens written to the prompt cache, in the provider's `currency`. Required with `prompt_cache: true` when the model has a cost |
| `cost.cache_read` | no | Cost per 1M tokens read from the prompt cache, in the provider's `currency`. Required like `cache_write` |

### Adaptive thinking (Anthropic)

Claude 4.6 and later models think adaptively and take an effort instead of a
thinking budget; Claude Opus and Sonnet 4.7 and later and the 5.x models
reject the budget. Mark such a model with `thinking: adaptive`:

```yaml
models:
  providers:
    anthropic:
      api_key: "${ANTHROPIC_API_KEY}"
      api: anthropic-messages
      models:
        - id: claude-sonnet-5-5
          thinking: adaptive
          reasoning_effort: high    # low, medium, high, xhigh or max
```

- Every request to the model carries `thinking: {type: adaptive}` and, when
  an effort is set, `output_config: {effort: <reasoning_effort>}`. Without
  an effort, Anthropic uses the model's default effort. `max_tokens` is sent
  as configured.
- A model without `thinking` is sent a thinking budget (`budget_tokens`)
  when it has `features: [reasoning]`, as before. Claude Haiku 4.5 and older
  models need that form and reject `thinking: adaptive`.
- A channel can set the effort of one turn with the message metadata
  `reasoning_effort` (`low`, `medium` or `high`); it replaces the model's
  effort for that turn. When the effort changes between two requests of a
  conversation, Anthropic reads a different prompt start, so with
  `prompt_cache: true` the conversation is written to the cache again.
- The thinking blocks a response returns together with tool calls are stored
  with the conversation and sent back unchanged, ahead of those tool calls, on
  every later request of the conversation that turns thinking on (`thinking:
  adaptive`, or a thinking budget), as Anthropic requires in a tool loop. A
  model that thinks without `thinking: adaptive`, as the 5.x models do, is
  sent none of them.
- On Claude Fable 5.1, Opus 5.5, Sonnet 5.5 and Haiku 5.5 a thinking block
  stays valid only while the system prompt, the tools and every earlier
  message are unchanged. OpenTalon changes the system prompt when a
  conversation first holds more than two messages and with its first tool
  result, and, on a model without [`inline_tools`](#inline-tools), the tools
  and the system prompt when it loads tools. Anthropic drops a block that is
  no longer valid, and a dropped block makes it write the conversation after
  it to the cache again. To a model with `thinking: adaptive` OpenTalon
  therefore sends a block only while the system prompt, the tools and the
  earlier messages, the announcements of `inline_tools` included, are what
  they were when the block was produced (cache markers and the effort do not
  count); a block produced after such a change goes back as long as nothing
  changes again. On a model with `inline_tools` a load changes none of them,
  so the blocks from before the load keep going back. With a thinking budget
  every block goes back, as those models need the unfinished turn's blocks
  and do not check them.
- Every request to a model with `thinking: adaptive` also carries
  `thinking.block_binding.prefix_mismatch_behavior: drop_block` and the
  `anthropic-beta` value `thinking-binding-controls-2026-08-01` (joined with
  `cache-diagnosis-2026-04-07` when cache diagnostics are on and with
  `inline-tools-2026-09-15` on a model with `inline_tools`), so a block
  that is no longer valid is left out instead of failing the request. The
  `llm_response` session event records how many Anthropic left out as
  `thinking_dropped`.
- `thinking` takes `adaptive` only. Any other value, `thinking: adaptive` on
  a provider whose `api` is not `anthropic-messages`, or a `reasoning_effort`
  other than the five levels on such a model fails loading the config with
  an error naming the model.

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

### Model from the profile

A whoami profile can name the model for its chat (`model` in the whoami
answer, see [Profiles](profiles.md)) as `<provider>/<model>`, both as
`models.providers` names them, for example `anthropic/claude-sonnet-5-5`. The
provider does not have to be part of `routing`:

| Profile `model` | Answered by |
|---|---|
| absent | the default chain (`routing.primary` and its fallbacks) |
| empty, or not a string | the default chain, with a warning |
| `<provider>/<model>`, both defined | that model at that provider; when the call fails or the provider is unhealthy, the default chain (see [When the selected provider fails](#when-the-selected-provider-fails)) |
| `<provider>/<model>`, provider or model not defined | the default model, with a warning |
| `<model>` without a provider, listed by a model of the default chain | the default chain, sent that id as before |
| `<model>` without a provider that no model of the default chain lists, or a reference with an empty provider or model part (`/model`, `provider/`) | the default model, with a warning |

- The chat loop follows the profile, for `chat` and `system` runs alike, and
  so do the turn's other calls to the chat models, with the same fallback:
  the planner and the text of a plan's confirmation, the confirmation
  question, the confirmation classifier, the repair call, and the session
  title and summary written after the turn. A turn a used-up budget moved
  to the default chain sends all of them there.
- A set `repair.model` keeps the repair call on that model. Sub-agents run
  on the model a host-side caller names, else on their parent's, else on
  the default chain, so a task the chat model starts never fans out onto
  the profile's model. They call their tools natively only, which a model
  with `features: [tools]` at the provider of `routing.primary` turns on;
  without it a `_subprocess` call gets an error result and sends no request.
- The call uses the selected model's own entry: its `thinking` and
  `reasoning_effort`, reasoning only when it has `features: [reasoning]`, and
  the request form of [`inline_tools`](#inline-tools) only when it has that
  feature, whatever the default model has. When the default chain answers in
  its place, the default model's entry decides the same way, so a default
  model without `inline_tools` gets every loaded tool in a flat `tools` array
  and the catalog of the tools not loaded. A turn's `reasoning_effort` still
  replaces the model's effort.
- A provider that cannot stream (`anthropic-messages`) answers a round that
  would stream with its non-streaming call.
- The usage row names the model that answered the run's first round, and each
  round's cost comes from the price list of the provider that served it,
  cache prices included. The row counts the chat loop's rounds only. Every
  other call of the turn has its own `llm_request` event, which names the
  model it was sent to (after a fallback a second one names the default
  model), and its `llm_response`.
- Every provider in `models.providers` is started. One whose `api_key`
  references an unset variable still starts; its calls fail and the default
  chain answers. One that cannot be built is left out with a warning
  instead of stopping the start; a profile model naming it gets the default
  model.
- The token count of an answer corrects the size estimate of the model that
  answered: the default model's when the default chain answered in place of
  the selected one.
- Conversations are trimmed to the default model's `context_window` and
  `max_tokens`, whichever model answers.
- A selected model whose `context_window` is smaller than the default
  model's can refuse long conversations: give it at least the default
  model's `context_window` and `max_tokens`.

#### When the selected provider fails

| The selected provider | Then |
|---|---|
| answers with a status other than 400, 413 and 422 (a refused key or account, a rate or spend limit, an unknown model, an overload or server error), cannot be reached, or reaches its `timeout` | the default chain answers with the default model, and one warning names the provider, the model and the error, with `unhealthy=true`: the provider is marked unhealthy |
| answers 400 with Anthropic's spend limit message ("You have reached your specified … API usage limits") or its empty credit message ("Your credit balance is too low to access the Anthropic API") | as above: the default chain answers and the provider is marked unhealthy |
| answers 400, 413 or 422 otherwise (it refuses this request), or with a reply that cannot be read | the default chain answers, with the warning and `unhealthy=false`; the provider stays in use |
| refuses the prompt as longer than the model's window | the chat loop gets the refusal, as before, and trims and sends again |
| does not answer before the turn is cancelled | the turn ends; the default chain is not called and nothing is marked |

While a provider is marked unhealthy:

- Requests for its models go straight to the default chain with the default
  model, warned about only at debug level.
- OpenTalon probes it, from its first mark on: `GET <base_url>/v1/models`
  with `x-api-key` for `anthropic-messages`, `GET <base_url>/models` with
  `Bearer` for `openai-completions` (`x-api-key` at `api.anthropic.com`),
  each with the provider's `headers`. `routing.health` sets `interval`
  (10s), `timeout` (3s) and `recover_after` (3): after `recover_after`
  healthy probes in a row the provider gets requests again.
  `routing.health.path` applies to the primary only.
- A probe checks that the endpoint answers and takes the key, not that the
  account may spend. When the probes answer but every call is refused, each
  return costs one refused call, and the default chain answers every
  request.
- A provider that cannot stream (`anthropic-messages`) is not marked for it;
  its non-streaming call serves the round.

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
      fail_open: false # optional; default false (fail-closed)
```

- **Normal case**: the preparer returns a string → that string is the user message for the LLM. Content preparers are **not** listed as callable tools; the LLM does not see or call them.
- **Guard case**: the preparer returns JSON `{"send_to_llm": false, "message": "..."}` → the orchestrator skips the LLM and sends that message to the user.
- **Failure behavior**: defaults to **fail-closed** (`fail_open: false`) — if the preparer/guard errors or is missing, the request is blocked. Set `fail_open: true` only for non-critical cases where skipping is acceptable.

See the [Hello World plugin](https://github.com/opentalon/hellow-world-plugin) for an example.

### Plugin call limits

```yaml
orchestrator:
  callback_timeout: "30s"     # Go duration; default "30s"
  max_response_bytes: 65536   # bytes; default 65536 (64 KB)
```

- `callback_timeout` caps one non-streaming plugin action: a tool call from the LLM, or a plugin's callback into the host (`RunAction`), including a decider's `decide`. A decider's HTTP request has no timeout of its own, so `callback_timeout` is its only cut, retries included. Unset, zero or invalid keeps 30s.
- `_subprocess.run` and `_subprocess.parallel` get the larger of `callback_timeout` and `subprocess.default_timeout × ceil(tasks / max_parallel) + 10s` (`tasks` = 1 for `run`), so a callback is not cut before its sub-agents can finish.
- `_subprocess.run` and `_subprocess.parallel` ask the user nothing; each call a sub-agent makes asks for approval as a main-loop call does. In the chat loop such a call pauses the sub-agent, and the turn ends with a question that names its task. An approval runs the call and the sub-agent goes on, also on another pod; the wait does not count against `subprocess.default_timeout`. The children of a parallel call that need no approval run to their end, and those that wait are asked about one per turn. A rejection, or a reply that is neither yes nor no, stops every waiting child, and the `_subprocess` call's result lists the calls that ran and the answers of the children that finished. A sub-agent started anywhere else (a plugin callback, the scheduler, the exec dispatcher) gets such a call refused.
- An action on a plugin's external gateway (`grpc_port`) has no host deadline of its own; it runs until the caller's gRPC deadline. Callbacks it makes get the deadlines above.
- `max_response_bytes` caps `Content`, `Error` and `StructuredContent` of each plugin result handled by the orchestrator. Text over the cap is truncated with a notice. Structured content over the cap is dropped and the result becomes an error naming its size and the limit, so a caller never gets truncated JSON. Results returned to an external gateway caller are not capped. Unset, zero or negative keeps 65536.

## Bundler-style plugins and channels

Instead of a local `plugin` path, you can point a plugin or channel at a GitHub repo and a **ref** (branch, tag, or commit). OpenTalon will clone the repo, build it, and pin the resolved commit in a lock file so installs are reproducible.

| Field   | Description |
|--------|-------------|
| `github` | Repo in the form `owner/repo` (e.g. `opentalon/hellow-world-plugin`). |
| `ref`    | Branch, tag, or commit SHA (e.g. `main`, `v1.0.0`, or `abc123def`). |

**Plugins** — use either `plugin` or `github` + `ref`:

```yaml
plugins:
  hello-world:
    enabled: true
    github: "opentalon/hellow-world-plugin"
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

### Multiple instances of the same channel

The map key under `channels:` is the **per-instance identifier**: it scopes
sessions, dedup, and actor IDs. The channel adapter declares a separate
**kind** (its `spec.id`, e.g. `slack`) which drives type-based routing
(content preparers, the WhoAmI `channel_type` header). The two are
deliberately distinct so a single opentalon process can host more than one
instance of the same kind — e.g. an admin Slack bot alongside a customer
one — with their messages isolated from each other.

```yaml
channels:
  slack-admin:
    enabled: true
    github: "opentalon/slack-channel"
    ref: "master"
    config:
      app_token: ${SLACK_APP_TOKEN_ADMIN}
      bot_token: ${SLACK_BOT_TOKEN_ADMIN}
  slack-customer:
    enabled: true
    github: "opentalon/slack-channel"
    ref: "master"
    config:
      app_token: ${SLACK_APP_TOKEN_CUSTOMER}
      bot_token: ${SLACK_BOT_TOKEN_CUSTOMER}
```

Inbound messages from `slack-admin` carry `ChannelID="slack-admin"` and
`Kind="slack"`; from `slack-customer` they carry `ChannelID="slack-customer"`
and `Kind="slack"`. Pair with `profiles.who_am_i.metadata_headers` (see
`docs/profiles.md`) to let the WhoAmI server grant different groups or
limits per bot — the channel writes its own bot user ID into
`msg.Metadata["channel_id"]` and opentalon forwards it as a configured HTTP
header.

Credentials live inside the per-instance `config:` block; `${ENV_VAR}`
expansion runs against the host environment so secrets stay out of YAML.

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
