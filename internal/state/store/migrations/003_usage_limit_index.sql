-- Compound index to efficiently enforce per-entity rolling token limits.
-- Covers the query: WHERE entity_id = ? AND created_at >= ?
CREATE INDEX IF NOT EXISTS idx_profile_usage_entity_created ON profile_usage(entity_id, created_at);
