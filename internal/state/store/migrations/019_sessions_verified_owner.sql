-- Only the core writes the mark, before a user's own chat turn. entity_id is no proof, as backfillSessionOwnership derived it from the key.
-- No backfill: a session from before this migration gets the mark with its next chat turn.
-- An older core ignores these columns, so a rollback needs no down migration.
ALTER TABLE sessions ADD COLUMN verified_entity_id TEXT;
ALTER TABLE sessions ADD COLUMN verified_group_id TEXT;
ALTER TABLE sessions ADD COLUMN verified_channel_id TEXT;
