-- Add entity and group ownership to sessions for per-user/per-group queries.
-- Column names match entities and profile_usage tables; "group" is a SQL reserved word.
ALTER TABLE sessions ADD COLUMN entity_id TEXT DEFAULT '';
ALTER TABLE sessions ADD COLUMN group_id TEXT DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_sessions_entity_id ON sessions(entity_id);
CREATE INDEX IF NOT EXISTS idx_sessions_group_id ON sessions(group_id);
