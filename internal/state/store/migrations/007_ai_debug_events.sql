-- Per-session deep debug capture. Populated only when a session's metadata
-- carries debug=true (toggled via the set_debug_mode action). Each row holds
-- one raw HTTP exchange between OpenTalon and the LLM endpoint (request /
-- response / error), with body kept as JSON text for after-the-fact replay
-- and diffing against parallel implementations.
--
-- Rows are pruned after debug.retention_days (default 30) by a background
-- worker started in cmd/opentalon. Volume is intentionally bounded by
-- session-level opt-in — without an active /debug session this table stays
-- empty.
--
-- Schema is portable across SQLite and PostgreSQL: TEXT for ids/timestamps/
-- bodies, INTEGER for status. Postgres-native shapes (jsonb / GIN / generated
-- columns) are deliberately avoided so the same migration runs on both
-- dialects and so debug-data inspection is just `SELECT body FROM ...` —
-- no jsonb-path queries assumed.
CREATE TABLE IF NOT EXISTS ai_debug_events (
  id          TEXT NOT NULL PRIMARY KEY,
  session_id  TEXT NOT NULL,
  trace_id    TEXT NOT NULL,
  ts          TEXT NOT NULL,
  direction   TEXT NOT NULL,
  status      INTEGER,
  url         TEXT,
  body        TEXT NOT NULL,
  created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_ai_debug_events_session_ts ON ai_debug_events(session_id, ts);
CREATE INDEX IF NOT EXISTS idx_ai_debug_events_ts ON ai_debug_events(ts);
