-- A chat session a server opens for a user: the host backend calls in on the
-- user's behalf, the session is stamped 'chat' (interaction_kind, migration 014)
-- because the user is meant to continue it, and its first turn is a hidden
-- context message the assistant answers.
--
--   sessions.opened_by         — the origin the caller names (an opaque label,
--                                e.g. the feature that opened the session).
--                                Separate from system_source (migration 016),
--                                which labels sessions a feature runs as system
--                                sessions; an opened session keeps that NULL.
--   sessions.open_key          — the caller's idempotency key (one per domain
--                                event). The unique index below makes the open
--                                call a create-or-find: the same entity, origin
--                                and key always answer the same session.
--   sessions.opener_claimed_at — when one request claimed the right to run the
--                                first turn (a compare-and-set on this column).
--                                The turn runner stores the hidden message, so
--                                the claim is the only "first turn started"
--                                mark before that message exists.
--   sessions.opener_stored_at  — when the first message after the claim was
--                                stored, written in the same transaction. A
--                                stale claim without it may be claimed again
--                                (the claimant died before its run); the
--                                transcript is no proof, since clearing,
--                                summarising and trimming delete messages.
--   session_note_reservations  — the claim to post a note into an existing
--                                session, one row per session and note key (the
--                                caller's key for that note). The same split as
--                                the opener: the reservation is the
--                                compare-and-set, the turn runner stores the
--                                note, and that store sets stored_at in the same
--                                transaction; a stale reservation without it may
--                                be claimed again.
--   messages.note_key          — the key on the stored note. Unique per session,
--                                so one key stores one note.
--
-- Ordinary sessions and messages leave the new columns NULL. Partial indexes:
-- only opened sessions and keyed notes carry a key, and NULL keys must never
-- collide. Reservations are removed with their session.
--
-- Portability: TEXT only; the new session and message columns are nullable;
-- the *_at columns hold RFC 3339 UTC timestamps like created_at. Runs on
-- SQLite and PostgreSQL.
--
-- Deploy order: the api-plugin reads opened_by by name (session-list
-- projection). Core applies migrations at startup before it loads plugins, so
-- ship core-with-018 before any api-plugin build that SELECTs the column (same
-- rule as migrations 013 to 016).
ALTER TABLE sessions ADD COLUMN opened_by TEXT;
ALTER TABLE sessions ADD COLUMN open_key TEXT;
ALTER TABLE sessions ADD COLUMN opener_claimed_at TEXT;
ALTER TABLE sessions ADD COLUMN opener_stored_at TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_open_key
  ON sessions(entity_id, opened_by, open_key) WHERE open_key IS NOT NULL;

ALTER TABLE messages ADD COLUMN note_key TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_note_key
  ON messages(session_id, note_key) WHERE note_key IS NOT NULL;

CREATE TABLE IF NOT EXISTS session_note_reservations (
  session_id TEXT NOT NULL,
  note_key TEXT NOT NULL,
  reserved_at TEXT NOT NULL,
  stored_at TEXT,
  PRIMARY KEY (session_id, note_key)
);
