-- messages.announcement: the on-demand tools that became callable ("added") and stopped being callable ("removed") with the request this user or tool message ended, as JSON. NULL: no change.
-- No backfill: a session from before this migration gets its records with its next request.
-- An older core ignores the column, so a rollback needs no down migration.
ALTER TABLE messages ADD COLUMN announcement TEXT;
