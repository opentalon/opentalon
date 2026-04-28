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

**After editing any `.txt` file in `internal/prompts/`, VCR cassettes become stale.** The CI gate will fail with a prompt_hash mismatch. Re-record:

```
ANTHROPIC_API_KEY=<key> make vcr-record-all
```

Then commit the updated cassettes alongside the prompt change.

## VCR cassettes

Integration tests that exercise the full orchestrator loop use pre-recorded cassettes instead of hitting the real API. Cassettes live in `internal/orchestrator/testdata/vcr/*.json`.

Each cassette stores:
- `prompt_hash` — `prompts.Hash()` at record time; Player fails immediately if this doesn't match the current hash
- `interactions` — sequential LLM responses; Player returns them in order regardless of request content

**Never edit cassette JSON by hand** except for hand-crafted fixtures. Real cassettes must be recorded with `make vcr-record-all`.

To add a new VCR test scenario:
1. Write the test in `internal/orchestrator/vcr_test.go` using `mustPlayer(t, "testdata/vcr/<name>.json")`.
2. Create an initial hand-crafted cassette in `testdata/vcr/<name>.json` with the current `prompts.Hash()` (run `go test -run TestPrintCurrentHash ./internal/prompts/` to get it).
3. Run `make vcr-record-all` to replace it with a real recorded cassette when an API key is available.
