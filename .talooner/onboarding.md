# Talooner onboarding spec

## Sensitive paths

Changes here wait for a review before merge.

- `.talooner/` — the review policy itself (rules, tests, teams, guides index) — owner assigned for review
- `internal/prompts/` — system prompts; edits change model behaviour everywhere and invalidate VCR cassettes — owner assigned for review
- `internal/auth/` — credential rotation and cooldown — owner assigned for review
- `internal/provider/` — chat-LLM clients: keys, headers, retries, context overflow — owner assigned for review
- `internal/state/` — sessions, memories and plugin state on disk — owner assigned for review
- `proto/` — wire contract for third-party plugins and channels — owner assigned for review
- `.github/workflows/` — CI and release path — owner assigned for review

## Extra rules

- PRs need a description
- Auto-approve small, tested, non-sensitive PRs (200 changed lines or fewer)

## AI review

on — provider: anthropic, model: claude-haiku-5-5

## Decision model

on — backend and model as configured by `CDM_URL` / `CDM_MODEL`

## Guides

- `internal/prompts/` — docs/guides/prompts.md
- `internal/state/store/` — docs/guides/state-store.md
- `internal/provider/` — docs/guides/providers.md
- `internal/decideprovider/` — docs/guides/decide-providers.md

## Notes

- Advice mode for the first weeks.
- Check runs come from the CI jobs named `Test` and `Lint`.
- One reviewer target (`review.opentalon`) for every gate; no separate "senior" concept.
- The review is requested by assigning `user.owner`: with `GITHUB_TOKEN`, GitHub rejects
  a team review request with 422, so `do require` is not used.
