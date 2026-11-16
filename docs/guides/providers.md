# Guide: chat-LLM providers (`internal/provider/`)

Two wire formats behind one `Provider` interface: `openai-completions`
(OpenAI, OVH, Ollama, vLLM, …) and `anthropic-messages`. Design background:
[`docs/design/providers.md`](../design/providers.md), config keys:
[`docs/configuration.md`](../configuration.md).

## New wire formats go through `FromConfig`

`FromConfig` switches on `cfg.API` and must reject anything unknown with an
error naming the supported values (`APIOpenAI`, `APIAnthropic`). An empty `api`
means OpenAI-compatible. Adding a provider means a new constant, a constructor
returning `Provider`, and a case here — never a second construction path in
`main.go`.

> **Gotcha**: building a provider directly with `NewOpenAIProvider(...)` in
> `bootstrap/` for a special endpoint. It skips `WithOpenAIHeaders`, the retry
> policy and the event sink, so the provider silently loses tracing and redaction.

## Configured headers are credentials

Header values from `models.providers.<id>.headers` are as sensitive as
`api_key`. `HeaderMap.Format` redacts them when the config is printed,
`copyExtraHeaders` drops empty values (an unset credential sends no header at
all), and `guardClient` wraps the transport so the values are scrubbed from
response bodies and transport errors, and cross-origin redirects are refused.

> **Gotcha**: wrapping the client as
> `withRetry(&http.Client{…}, retry, sink)` and calling `guardClient` afterwards.
> Retry then sees unredacted bodies, and a log line in the retry path leaks the
> key. Correct order: `withRetry(guardClient(client, headers), retry, sink)`.

Redaction only matches values of 8+ characters, and only literally — an encoded
or streaming-split echo is not caught. Treat the guard as defence in depth, not
as a licence to log raw bodies.

## A context overflow is our bug, not an endpoint failure

`ContextOverflow(err)` classifies "prompt too long" and returns the token count
the provider measured. Do not retry it on another endpoint (same window, same
failure, and the user sees the next endpoint's unrelated error). Do not trip the
endpoint health gate on it either — the endpoint is fine.

> **Gotcha**: feeding a `prompt is too long` error into the generic
> failover/health path. A working endpoint leaves rotation and the user is shown
> "app has no replica running" instead of "context too long".

Keep the phrase list narrow. A false positive turns a real endpoint failure into
"oversized prompt" and hides an outage.

## A profile model goes through the selecting provider

`NewSelectingProvider` stands in front of the default provider (the health gate,
or the single routing provider). `ID`, `Models`, `SupportsFeature` and
`PrimaryModels` are the default provider's. User-facing behaviour:
[Model from the profile](../configuration.md#model-from-the-profile).

- `CompletionRequest.ModelRef` is the profile's model as the chat loop received
  it; `Model` holds its model part, for a provider that serves the request
  itself. Only the chat loop sets `ModelRef`.
- A request for a selected model carries that model's id and its own reasoning
  flag (`features: [reasoning]`), not the default model's.
- A non-200 answer is a `StatusError` (status and body; its text is
  `<api> api error (status N): <body>`). `providerFailure` decides from it, or
  from a `net.Error`, whether the provider is marked unhealthy; the classes are
  the table in [When the selected provider fails](../configuration.md#when-the-selected-provider-fails).
- A `SelectableProvider` without a `Probe` is never marked. With one, the probes
  start at its first mark, run with `routing.health` interval, timeout and
  `recover_after`, and stop when the context passed to `NewSelectingProvider`
  ends. `endpointHealth` is the gate's; its name starts its log messages.
- `NewProviderProbe(cfg, path)` gets the model list of the provider's API
  (`/v1/models` with `x-api-key` for `anthropic-messages`, `/models` with
  `Bearer` otherwise) at its base URL or the API's default, with the provider's
  headers. A set `path` replaces the model list path (`routing.health.path`, the
  primary only).
- A provider that cannot stream returns `ErrStreamingNotImplemented` (wrapped).
  The selecting provider passes it through unmarked, so the caller's `Complete`
  serves the selected model.
- `CompletionResponse.Fallback` and `StreamChunk.Fallback` say the default model
  answered a request for another model. The orchestrator then corrects the
  default's calibration (key `""`), with the estimate rescaled from the requested
  model's correction to the default's, and leaves the requested model's alone.
- `ProviderConfig.Timeout` is the HTTP client's `Timeout` for calls that do not
  stream, retries included; 0 keeps 120 s. The OpenAI-compatible stream uses a
  client of its own without it.

## Retry policy: zero fields fall back to defaults

`RetryPolicy.withDefaults()` fills every empty field from `DefaultRetryPolicy()`
so a partial config is safe. Retry lives in the transport (`withRetry`), which
means a request is retried before any caller sees it; anything above it must not
retry again.

## Advice

- New classification logic (error shapes, status codes) belongs in a helper next
  to `ContextOverflow` with a table test of raw provider strings, not as an
  `if` inside a request loop.
- When a provider changes its wording, add the new string to the pattern list and
  keep the old one until the pinned version stops producing it.

## Specs

### `internal/provider/factory.go`

- Every new `cfg.API` value has a case and a constructor, and the default case
  lists all supported values.

### `internal/provider/headers.go`

- A client with extra headers is built as `withRetry(guardClient(c, headers), …)`.
- Values shorter than 8 characters are never treated as secrets.

### `internal/provider/context_overflow.go`

- Each new provider message has a regex capturing the measured token count, or
  appears in the phrase list when it carries no count.
- A test asserts a similar-sounding but unrelated error is **not** classified as
  an overflow.
