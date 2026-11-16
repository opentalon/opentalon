-- The verified owner of a chat session: the entity, the account and the
-- channel of the whoami-verified profile of the latest chat turn a user ran
-- in it themselves.
--
--   sessions.verified_entity_id  — the profile's entity.
--   sessions.verified_group_id   — the profile's account (its group).
--   sessions.verified_channel_id — the channel the turn arrived on.
--
-- The core writes the three together, before a user's own chat turn runs in
-- the session, and nothing else writes them. They are the proof that a user
-- holds the session: entity_id is not, because backfillSessionOwnership (Go,
-- after migration 004) derived it from the key's first segment, and a session
-- keyed without a profile ("<channel>:<conversation>:<thread>") has the form of
-- a user's conversation. A session without a mark has verified_entity_id NULL.
--
-- No backfill, on purpose: no existing row gets the mark. A session from
-- before this migration gets it with its next chat turn.
--
-- Portability: TEXT only, nullable, no default. Runs on SQLite and PostgreSQL.
--
-- Deploy order: only the core reads and writes these columns. An older core
-- ignores them, so a rollback needs no down migration.
ALTER TABLE sessions ADD COLUMN verified_entity_id TEXT;
ALTER TABLE sessions ADD COLUMN verified_group_id TEXT;
ALTER TABLE sessions ADD COLUMN verified_channel_id TEXT;
