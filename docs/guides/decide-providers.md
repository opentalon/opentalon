# Guide: decision providers (`internal/decideprovider/`)

Host side of a tln `decide "…" using model "m"` block. A `decide` step arrives as
a host callback `RunAction(ctx, "<model>", "decide", {state, choices})`; core
resolves the name here, runs it through the configured backend, and meters the
usage like any other provider call.

Backends: `laya`, `jev`, `subq`, `local-logits`.

## Every backend returns the same normalized shape

A `Decision` is `{chosen, confidence, probabilities}` with three invariants:

- `probabilities` has exactly one entry per declared choice, summing to 1;
- `chosen` is the argmax, ties broken by declared choice order;
- `confidence == probabilities[chosen]`, so the executor's `confidence >=` gate
  is a threshold on the chosen option's mass.

A backend that violates any of these makes tln `decide` blocks behave
differently for the same program.

> **Gotcha**: returning the model's raw scores verbatim (for example
> `{"yes": 0.7, "no": 0.2}`, summing to 0.9, or carrying a label the program never
> declared). Gate checks now compare against a number that is not a probability.

## Normalize in the shared helpers, not per backend

`normalizeProbabilities` drops labels outside the declared choices, zero-fills
declared choices the backend omitted, clamps negatives and NaN to 0, and spreads
mass uniformly when every score is 0 — so a dead model yields a valid
distribution instead of NaN. `softmaxLogits` does the same for logit backends.
New backends must route their output through one of these, never build a
`Decision` by hand.

> **Gotcha**: `&Decision{Chosen: top, Confidence: topScore}` constructed inline.
> `Probabilities` is nil, and every downstream rule that reads the distribution
> panics or silently sees confidence 1.

## Decider names are plugin tool names

A decider is registered as a plugin named after itself, so the name must match
`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,55}$` — no leading underscore (`_subprocess` and
friends are built-ins) and at most 56 characters. `RegistryFromConfigs` rejects
duplicates and processes names in sorted order so the error is deterministic.

> **Gotcha**: a decider named `_risk`. Config loads, but the plugin name
> collides with the reserved built-in namespace.

## Cost must be a real number of 0 or more

`FromConfig` rejects a negative, NaN or infinite `cost.input`/`cost.output`.
A negative price makes the cost counters decrease, which panics Prometheus on
the first `decide` call — a crash at request time instead of a startup error.

## Every decision is metered

`Usage` mirrors `provider.Usage` so a `Decision` feeds `store.UsageRecord`
unchanged. Pure classifiers (`laya`) report `output_tokens == 0` but still report
input tokens, so input spend stays attributable. A new backend that does not
populate `Usage` makes decisions free in the profile limit accounting.

## Advice

- Prefer `normalizeProbabilities` over a bespoke calibration step; calibration
  differences belong in config (cost, model) rather than in the backend.
- A new backend usually needs: constructor, `BackendX` constant, `FromConfig`
  case, and a table test proving the normalization invariants hold for hostile
  output (empty, negative, extras, NaN).

## Specs

### `internal/decideprovider/factory.go`

- A new backend appears in the `FromConfig` switch and in the error message that
  lists supported backends.
- `ValidateName`, `BaseURL` presence and the cost check all still run before the
  switch.

### `internal/decideprovider/normalize.go`

- Any new scoring path produces a distribution over exactly the declared
  choices that sums to 1, with `confidence == probabilities[chosen]`.
- A tie resolves to the earliest declared choice.

### `internal/decideprovider/*_test.go`

- Hostile outputs: empty map, all-zero scores, negative scores, NaN, an
  undeclared label, fewer labels than choices.
- A transport failure returns an error from `Decide` (it does not invent a
  `chosen`), so the executor's `on_error` policy decides what happens.
