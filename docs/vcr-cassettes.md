# VCR cassettes

Integration tests replay pre-recorded LLM interactions stored as JSON cassettes in `internal/orchestrator/testdata/vcr/`. The CI `test-vcr` job runs these on every PR and push to `master` — no API keys required for replay.

Replay and recording run the orchestrator on native tool calling, the path production takes: the player reports native tool support, and recording builds providers whose models declare it. The player checks each request against the recorded one, the tools it offers and whether its system prompt holds the native preamble, and fails a request off the recorded path:

```
vcr: cassette … interaction 1: request differs from the recorded one: tools [], recorded ["_meta__load_tools"]; native preamble false, recorded true
```

Each cassette embeds a `prompt_hash` computed from `internal/prompts/`. If you edit any prompt file, the hash changes and the CI job fails with:

```
vcr: cassette … is stale
  stored:  <old hash>
  current: <new hash>
Re-record with: make vcr-record-all
```

To fix it, re-record with real API keys and commit the updated cassettes:

```bash
ANTHROPIC_API_KEY=<key> OPENROUTER_API_KEY=<key> make vcr-record-all
git add internal/orchestrator/testdata/vcr/
git commit -m "chore: re-record VCR cassettes"
```

`ANTHROPIC_API_KEY` records the Anthropic/Haiku scenarios; `OPENROUTER_API_KEY` records the OpenRouter/Ministral scenarios. Each is optional — omitting one skips that provider's cassettes.

A recording that fails, a 401 included, empties the cassettes it was recording: restore them with `git checkout -- internal/orchestrator/testdata/vcr` and record again.
