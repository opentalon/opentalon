-- Structured per-session event log. Always-on (unlike ai_debug_events which is
-- /debug-opt-in) and intentionally written at the point of FIRST observation,
-- before any parsing / normalization. The orchestrator emits one row per
-- meaningful step (turn_start, user_message, llm_request, llm_response,
-- tool_call_extracted, tool_call_result, retries, summarization, failure
-- modes, score_computed, ...). The exhaustive list of event_type values
-- and their payload shape lives in internal/state/store/events.
--
-- Rows are append-only — the only mutator is the retention worker. payload
-- carries a schema-versioned JSON blob ("v":N) so producers and consumers
-- can evolve independently; per-event-type Go structs are the source of
-- truth (see internal/state/store/events/event_types.go).
--
-- parent_id wires events into a DAG: tool_call_result.parent_id points at
-- the tool_call_extracted row, llm_error / tool_call_parse_failed point at
-- llm_request, retries point at the failed predecessor. NULL = root event
-- of a turn.
--
-- duration_ms is recorded where it is meaningful (llm_response,
-- tool_call_result, summarization_completed, ...) and NULL otherwise.
--
-- prompt_snapshots is a content-addressed store for system_prompt /
-- server_instructions / tool_description bodies. turn_start references
-- snapshots by sha256, so 10k sessions sharing one system prompt cost one
-- snapshot row, not 10k inlined duplicates. The api-plugin resolves
-- references on read (?expand=prompt_snapshots).
--
-- Schema portability: TEXT for ids/timestamps/payloads, INTEGER for seq /
-- duration_ms. No jsonb / generated columns — the same migration runs on
-- SQLite and PostgreSQL, matching the pattern set in 007_ai_debug_events.sql
-- and 008_messages_tool_calls.sql.
CREATE TABLE IF NOT EXISTS session_events (
  id          TEXT NOT NULL PRIMARY KEY,
  session_id  TEXT NOT NULL,
  seq         INTEGER NOT NULL,
  ts          TEXT NOT NULL,
  event_type  TEXT NOT NULL,
  parent_id   TEXT,
  duration_ms INTEGER,
  payload     TEXT NOT NULL,
  created_at  TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_session_events_session_seq ON session_events(session_id, seq);
CREATE INDEX IF NOT EXISTS idx_session_events_type_ts ON session_events(event_type, ts);
CREATE INDEX IF NOT EXISTS idx_session_events_parent ON session_events(parent_id);

CREATE TABLE IF NOT EXISTS prompt_snapshots (
  sha256      TEXT NOT NULL PRIMARY KEY,
  kind        TEXT NOT NULL,
  content     TEXT NOT NULL,
  created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_prompt_snapshots_kind ON prompt_snapshots(kind);
