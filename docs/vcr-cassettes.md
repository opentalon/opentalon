# VCR cassettes

Integration tests replay pre-recorded LLM interactions stored as JSON cassettes in `internal/orchestrator/testdata/vcr/`. The CI `test-vcr` job runs these on every PR and push to `master` — no API keys required for replay.

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
