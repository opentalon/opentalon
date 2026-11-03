# Talooner onboarding spec

## Sensitive paths

Changes here wait for a review before merge.

- `internal/prompts/` — system prompts; edits change model behaviour everywhere and invalidate VCR cassettes — reviewed by @opentalon/opentalon
- `internal/auth/` — credential rotation and cooldown — reviewed by @opentalon/opentalon
- `internal/provider/` — chat-LLM clients: keys, headers, retries, context overflow — reviewed by @opentalon/opentalon
- `internal/state/` — sessions, memories and plugin state on disk — reviewed by @opentalon/opentalon
- `proto/` — wire contract for third-party plugins and channels — reviewed by @opentalon/opentalon
- `.github/workflows/` — CI and release path — reviewed by @opentalon/opentalon

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
