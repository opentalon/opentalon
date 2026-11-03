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
