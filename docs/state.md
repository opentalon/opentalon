# State and context persistence

When `state.data_dir` is set, conversation and rules persist across restarts using SQLite (`state.db`). The app uses the **pure-Go** driver [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite), so no system SQLite library is required — it's a normal Go dependency and is compiled into the binary.

**General** rules (shared) and **per-actor** rules (per user) are stored as memories so multiple users get shared context plus their own. The LLM receives:

- **General context** — config rules, general stored rules, tools
- **User/actor context** — that actor's stored rules
- **Session** — conversation history

Sessions are one per channel/conversation or thread; optional **summarization** after N messages compresses history into a summary and keeps only the last few messages, so token usage stays bounded.

Pipeline state is currently in-memory only — persistence is planned for Phase 4.

```
User message ──▶ Pre-hooks (Lua / Go / small LLM) ──▶ Planner ──▶ Main LLM ──▶ Post-hooks (Lua / Go) ──▶ Response
                  │  zero tokens for rules                │            │              │
                  │  cheap tokens for small LLM           │            │              │  enforce vocabulary
                  │  full power via gRPC plugins           │            │              │  compliance, transform
                                                          │            │
                                                   multi-step?         │
                                                    ├─ yes ──▶ Pipeline Executor ──▶ Results
                                                    └─ no  ──▶ Agent Loop ─────────┘
```

See [docs/design/channels.md](design/channels.md) (State and memory) and `config.example.yaml` (`state.session`) for details.
