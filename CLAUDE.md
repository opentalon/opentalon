# Codebase conventions for AI assistants

## System prompts

All LLM system prompt strings live in `internal/prompts/*.txt` and are loaded via `//go:embed` in `internal/prompts/prompts.go`.

**Never define a system prompt as a plain Go string literal.** No backtick strings, no `const`, no inline `WriteString("You are...")`. Every prompt must be a `.txt` file in `internal/prompts/` and exported through `prompts.go`.

This keeps prompts diffable, hashable (used by VCR cassette staleness checks), and editable without touching Go source.

### Adding a new prompt

1. Create `internal/prompts/<name>.txt` with the prompt text.
2. Add a `//go:embed <name>.txt` var in `internal/prompts/prompts.go`.
   - For block prompts that include their own trailing newlines: export as `string` directly.
   - For single-line strings where callers control surrounding whitespace: trim with `strings.TrimRight(raw, "\n")`.
   - For rule lists (one rule per line): use `splitLines(raw)` to export as `[]string`.
3. Reference the exported var at the call site.

**After editing any `.txt` file in `internal/prompts/` or any `.yaml` file in `internal/scenarios/testdata/`, VCR cassettes become stale.** The `test-vcr` CI job will fail with:

```
vcr: cassette … is stale (prompts changed)
  stored:  <old hash>
  current: <new hash>
Re-record with: make vcr-record-all
```

or

```
vcr: cassette … is stale (scenarios changed)
  stored:  <old hash>
  current: <new hash>
Re-record with: make vcr-record-all
```

Re-record with both provider keys, then commit the updated cassettes:

```
ANTHROPIC_API_KEY=<key> OPENROUTER_API_KEY=<key> make vcr-record-all
git add internal/orchestrator/testdata/vcr/
git commit -m "chore: re-record VCR cassettes after prompt/scenario change"
```

## VCR cassettes

Integration tests that exercise the full orchestrator loop use pre-recorded cassettes instead of hitting the real API. Cassettes live in `internal/orchestrator/testdata/vcr/*.json`.

Each cassette stores:
- `prompt_hash` — `prompts.Hash()` at record time; Player fails immediately if this doesn't match
- `scenarios_hash` — `scenarios.Hash()` at record time; Player fails immediately if this doesn't match
- `interactions` — sequential LLM responses; Player returns them in order regardless of request content

**Never edit cassette JSON by hand** except for hand-crafted fixtures. Real cassettes must be recorded with `make vcr-record-all`.

To add a new shared scenario (covered by VCR, structural, and eval):
1. Add a `Scenario` entry to a YAML file in `internal/scenarios/testdata/`.
2. `TestVCRScenarios` skips it automatically until a cassette exists.
3. Record with real API keys:
   ```
   ANTHROPIC_API_KEY=<key> OPENROUTER_API_KEY=<key> make vcr-record-all
   ```
4. Commit the YAML change and the generated cassette files together.

To add a VCR-only scenario (needs special setup like custom rules or pipeline):
1. Write the test function directly in `internal/orchestrator/vcr_test.go`.
2. Use `mustPlayer(t, "testdata/vcr/<name>.json")` — skips if cassette missing.
3. Record with `VCR_RECORD=1 go test -v -run TestVCR<Name> ./internal/orchestrator/...`

To get the current `prompts.Hash()` value (needed when hand-crafting cassettes):
```
go test -v -run TestPrintCurrentHash ./internal/prompts/
```
