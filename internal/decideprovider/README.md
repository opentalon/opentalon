# decideprovider

opentalon's client layer for external **typed-decision models** — the host side
of tln's `decide "..." using model "m"` block
([tln-language #205](https://github.com/opentalon/tln-language/pull/205),
[opentalon #361](https://github.com/opentalon/opentalon/issues/361)).

## Why it lives in core

A tln program never talks to a model directly. When a `decide` block runs, the
`tln-plugin` runtime forwards it as a **host callback**:

```
RunAction(ctx, "<model>", "decide", {state, choices})
```

Core resolves `<model>` to a `Provider` here, runs the decision, and — crucially
— records the call's token usage in `profile_usage` exactly like a
`provider.Provider` LLM call. That's the whole reason this is in opentalon rather
than a standalone tln plugin: decision spend must be **metered** and gated by the
same per-profile limits (`UsageStore.TotalTokensSince`) as every other model
call. A plugin off to the side would bypass that.

## The contract

Every backend answers the same shape the `decide` executor expects:

```
Decide(state, choices) -> { chosen, confidence, probabilities }
```

with the invariants enforced in `normalize.go`:

- `probabilities` has one entry per **declared** choice and sums to 1
  (stray labels dropped, unseen choices zero-filled),
- `chosen` is the argmax, tie-broken by declared order (determinism),
- `confidence == probabilities[chosen]`, so the executor's `confidence >=` gate
  is a threshold on the chosen option's mass.

Calibration is honored **here**, so a backend that returns overconfident argmax
or an un-normalized vector is still made honest before the gate sees it.

## Backends

| `backend`        | source | notes |
|------------------|--------|-------|
| `laya`           | [`convaiinnovations/laya-typed-decisions`](https://huggingface.co/convaiinnovations/laya-typed-decisions) | open-weights ModernBERT classifier server; `probabilities` renormalized or `scores` softmaxed; input-only usage |
| `jev`            | [typesafe.ai](https://typesafe.ai/) | hosted System-1 API, bearer auth; response renormalized locally; passes through reported usage |
| `local-logits`   | [Jev in 25 lines](https://www.nobodywho.ai/posts/jev-in-25-lines/) | any OpenAI-compatible completions server; reads first-token logprobs over the choice tokens and **softmaxes them locally** |

All three return the same shape, so they're swappable via config with **no change
to the `.tln` source**.

## Usage (tln-first)

Bind a decider by name in `config.yaml`:

```yaml
models:
  deciders:
    decider:
      backend: local-logits
      base_url: "http://localhost:8080/v1"
      model: "qwen3-0.6b"
```

Then reference it from a `.tln` program — nothing here is written in Go:

```tln
decide "email_kind" {
  for records where folder == "Inbox"
  choices ["Legitimate", "Spam", "Phishing"]
  ask concat("Subject: ", attr "subject", "\n\n", attr "body")
  using model "decider"
  confidence >= 0.9
}
```

Switching to Jev or laya is a config edit (`backend:` + endpoint), not a code or
`.tln` change.
