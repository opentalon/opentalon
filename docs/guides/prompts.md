# Guide: system prompts (`internal/prompts/`)

Prompt text drives every model call in the host. Design background:
[`docs/design/pipeline.md`](../design/pipeline.md), VCR rules:
[`docs/vcr-cassettes.md`](../vcr-cassettes.md).

## Prompts are embedded `.txt` files

Add prompt text as `internal/prompts/<name>.txt` and expose it from
`prompts.go` with `//go:embed <name>.txt` on an unexported `...Raw` var plus a
`strings.TrimRight(raw, "\n")` export. Never build a prompt as a Go string
literal, and never paste prompt text at a call site.

> **Gotcha**: `fmt.Sprintf("You are a helpful assistant...")` inside
> `internal/orchestrator/`. The prompt is then invisible to `Hash()`, not
> overridable from config, and outside the review path.

## Every exported prompt has a `promptSetters` entry

`ApplyOverrides` only knows the names listed in `promptSetters`, and the name is
the `.txt` filename without the extension. A prompt with an export but no setter
is silently ignored when a deployment sets it in config.

> **Gotcha**: adding `//go:embed orchestrator_reminder.txt` + `OrchestratorReminder`
> and forgetting the `"orchestrator_reminder"` key in `promptSetters`.
> `ApplyOverrides` then reports the key as `unknown` instead of applying it.

## Match the setter's post-processing

`promptSetters` mirrors what the embedded default gets: raw for preambles,
`TrimRight(…, "\n")` for text the caller concatenates, `splitLines` for the
`rules_*.txt` files that become `[]string`. A setter with different
post-processing makes config text behave differently from the default.

> **Gotcha**: `"rules_default": func(s string) { DefaultRules = []string{s} }`
> — one long rule instead of one rule per line.

## A prompt edit is a behaviour change

`Hash()` is the sha256 of every embedded `.txt`, sorted by filename, including
the filename and its content. Cassettes store that digest, so any prompt edit
invalidates recorded replays. Re-record or update the affected cassettes in the
same PR; `docs/vcr-cassettes.md` has the commands.

> **Gotcha**: fixing a typo in `orchestrator_preamble.txt` and leaving the VCR
> jobs failing, or re-recording on a different branch so the cassette carries a
> digest that `master` no longer produces.

## Advice

- Prefer one `.txt` per logical block over one giant preamble: a block that can
  be blanked from config (`ApplyOverrides` applies empty values, which is how a
  deployment drops a built-in block) is easier to reason about than a paragraph
  buried in a larger prompt.
- Prompt tweaks deserve a before/after example in the PR description; the diff
  alone does not show the behaviour change.

## Specs

### `internal/prompts/prompts.go`

- A new prompt has both a `//go:embed` var and a `promptSetters` entry keyed by
  the filename without `.txt`.
- The exported value is post-processed the same way as the other prompts of its
  kind (raw / trailing-newline-trimmed / line-split).
- `Hash()` needs no change: it reads the embed directory.

### `internal/prompts/prompts_test.go`

- `TestPromptSettersCoverAllEmbedded` already enforces both directions of the
  mapping (every embedded `.txt` has a setter, every setter has a `.txt`), so a
  half-added prompt fails `make test`.
- A prompt whose text is transformed needs an `ApplyOverrides` case proving the
  override is transformed the same way as the embedded default.
