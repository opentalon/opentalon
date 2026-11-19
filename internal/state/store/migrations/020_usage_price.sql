-- profile_usage.price: the customer price of the run's model calls at the whoami profile's tariffs, NULL when the profile carried none.
-- DOUBLE PRECISION, not REAL: PostgreSQL's REAL is single precision, and budgets sum this column.
-- The index backs the per-group sum over a run kind since a point in time.
-- An older core ignores the column, so a rollback needs no down migration.
ALTER TABLE profile_usage ADD COLUMN price DOUBLE PRECISION;

CREATE INDEX IF NOT EXISTS idx_profile_usage_group_kind_created
  ON profile_usage(group_id, interaction_kind, created_at);
