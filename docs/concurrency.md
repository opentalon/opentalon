# Concurrency

By default, OpenTalon processes one session at a time (`max_concurrent_sessions: 1`). This matches the original sequential behaviour and is the safe default for most deployments. Enable concurrent session processing by raising the limit:

```yaml
orchestrator:
  max_concurrent_sessions: 5   # up to 5 sessions run in parallel (default: 1)
```

**How it works:**

- **Different sessions run in parallel** — up to the configured limit. Session A no longer waits for Session B to finish.
- **Same session is always serialized** — a second message to the same session waits for the first to complete, regardless of the limit. This preserves conversation ordering.
- **Backpressure is built-in** — when the limit is reached, new requests wait until a slot is free (or their context is cancelled).

```
max_concurrent_sessions: 3

Session A ──────────────────► reply A   ┐
Session B ──────────────────► reply B   ├─ run in parallel
Session C ──────────────────► reply C   ┘
Session D ···················► (waits for A, B, or C to finish)

Same session:
Session A msg1 ──────────────► reply 1
Session A msg2 ···············► (waits for msg1 to finish) ──► reply 2
```

Set `max_concurrent_sessions` to match your LLM provider's rate limits and expected load. A value of `1` (default) gives the original sequential behaviour with no behaviour change needed for existing deployments.
