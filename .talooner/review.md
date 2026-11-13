# Review guidelines

Repo-wide rules Talooner applies to every pull request in `opentalon`.

## No comments in source code

Do not add comments to Go source. If a change needs explaining beyond the code,
put an explanation in `docs/` in the same PR.

A comment in a changed file is a finding unless it documents a genuinely
non-obvious *why* that the code cannot express, such as a workaround for an
external bug.

## System prompts live in `.txt` files

System prompts are `internal/prompts/*.txt`, embedded with `//go:embed` and
exposed through `internal/prompts/prompts.go`. Never introduce a system prompt
as a Go string literal, and never inline prompt text at a call site. To add one:
drop the `.txt` file in that folder, add the `//go:embed` var, and register a
`promptSetters` entry so config can override it.

## Cassettes must stay in sync with prompts and scenarios

`prompts.Hash()` is a sha256 over every embedded prompt file, and VCR cassettes
record it. Editing a `.txt` prompt or a scenario YAML without re-recording makes
VCR replay fail. A PR that touches prompts or scenario YAML must say which
cassettes it re-recorded, or why none were affected.

## Never commit secrets or local state

These must never appear in a diff: `.env`, `config.yaml`, `prod.config.yaml`,
`*.config.yaml.bak`, `agents.db` and its `-wal`/`-shm` files, and any file
carrying a real API key. New configuration belongs in `config.example.yaml` with
`${ENV_VAR}` placeholders.
