-- messages.announcement: the on-demand tools that became callable ("added") and stopped being callable ("removed") with the request this user or tool message ended, as JSON. NULL: no change.
-- messages.message_key: the identity a store gives a message when it writes it; SetAnnouncement addresses the row by it.
-- No backfill: rows from before this migration keep both NULL, and a session from before it gets its records with its next request.
-- An older core ignores both columns, so a rollback needs no down migration.
ALTER TABLE messages ADD COLUMN announcement TEXT;
ALTER TABLE messages ADD COLUMN message_key TEXT;
