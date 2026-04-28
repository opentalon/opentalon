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
