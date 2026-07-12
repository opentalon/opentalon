# Concurrency

## The inbound pipeline

Every inbound message passes through the same stages, in this order. This is
the authoritative description of the pipeline; `docs/cluster.md` and the
channel registry's code comments point here.

1. **Cross-pod message dedup** (channel registry; cluster mode only). When
   several pods receive the same event from a channel, each races for a Redis
   lock (`SET NX`) keyed to the message; only the winner proceeds. Fail-open:
   if Redis is unreachable the message is processed anyway.
2. **Debounce** (channel registry). Messages arriving in quick succession in
   the same conversation are batched for `orchestrator.debounce_window` and
   dispatched as one turn.
3. **Global concurrency cap** (orchestrator semaphore). At most
   `max_concurrent_sessions` turns run at once per pod (see below).
4. **In-pod per-session mutex** (orchestrator). Turns for the same session
   are serialized within the pod, preserving conversation ordering.
5. **Cross-pod session-turn lease** (orchestrator; cluster mode only). A
   Redis lease with a heartbeat extends "one turn at a time per session"
   across pods, so two messages for the same session landing on different
   pods cannot run concurrently. Fail-open on Redis errors: the pod proceeds
   with only in-pod serialization. See `internal/sessionlock`.

Stages 4 and 5 are always taken together, in that order (one helper in the
orchestrator owns the ordering). Background work that rewrites session state —
the summarizer, which deletes and reinserts message rows — takes the same two
locks as a turn.

## Session parallelism

By default, OpenTalon processes one session at a time (`max_concurrent_sessions: 1`). This matches the original sequential behaviour and is the safe default for most deployments. Enable concurrent session processing by raising the limit:

```yaml
orchestrator:
  max_concurrent_sessions: 5   # up to 5 sessions run in parallel (default: 1)
```

**How it works:**

- **Different sessions run in parallel** — up to the configured limit. Session A no longer waits for Session B to finish.
- **Same session is always serialized** — a second message to the same session waits for the first to complete, regardless of the limit (and in cluster mode, regardless of which pod it lands on). This preserves conversation ordering.
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

## State-placement invariant

Where per-session state may live, so that multi-pod deployments stay correct:

- **Per-session mutable state lives in the state store or is only mutated
  while holding the session locks.** Anything a later turn — possibly on
  another pod — must observe (messages, summaries, titles, pending tool
  calls) is persisted, and rewritten only under the in-pod mutex plus the
  cross-pod turn lease.
- **In-memory maps are per-pod optimizations.** They must tolerate cross-pod
  invalidation: another pod may change the authoritative state at any time,
  so a map entry may only ever be a cache over persisted state — never the
  sole copy of something another pod needs, and never a trigger for an
  action another pod may already have performed.
- **Ephemeral counters may stay in-memory when their loss is benign.**
  Per-pod heuristics such as consecutive-tool-error counters may reset on
  restart or diverge between pods without correctness impact.

Known exceptions are per-pod by design and warn at startup in cluster mode:
pending *pipeline* plans (in-memory) and scheduler/reminder jobs (pod-local
disk).
