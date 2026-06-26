-- Per-session channel label, mirroring the channel_id already carried per LLM
-- call in profile_usage (migration 002). Today every channel record lives in
-- profile_usage only, so "show me HTTP-channel sessions" requires a JOIN that
-- the api-plugin REST surface does not perform. A column on sessions makes
-- per-channel filtering and grouping cheap and indexable for sessions/events
-- queries — same shape entity_id and group_id already have (migration 004).
--
-- Backfill from profile_usage: the channel_id is per-session by construction
-- (msg.ChannelID is stamped once per inbound message and copied into every
-- usage row for that session), so a scalar subquery picking any matching row
-- yields the authoritative value. LIMIT 1 is portable across SQLite and
-- PostgreSQL. Sessions with no profile_usage rows (orchestrator failed before
-- the first LLM call, no profile verification configured) keep the empty
-- default — readers must treat '' as "channel unknown" rather than as a
-- distinct group, matching how entity_id='' and group_id='' are handled today.
--
-- TEXT not VARCHAR for SQLite/PostgreSQL portability — matches migration 011.
-- Column is NOT NULL with a DEFAULT '' so existing readers that SELECT * from
-- sessions never see NULL for this field. This is stricter than the entity_id /
-- group_id columns from migration 004, which are nullable (TEXT DEFAULT '',
-- without NOT NULL); the '' default carries the same "unknown" convention here,
-- and NOT NULL additionally rules out NULL.
ALTER TABLE sessions ADD COLUMN channel_id TEXT NOT NULL DEFAULT '';

UPDATE sessions
   SET channel_id = COALESCE(
       (SELECT channel_id FROM profile_usage
         WHERE profile_usage.session_id = sessions.id
         LIMIT 1),
       '')
 WHERE channel_id = '';

CREATE INDEX IF NOT EXISTS idx_sessions_channel_id ON sessions(channel_id);
