-- Add model tracking and cost fields to profile_usage.
ALTER TABLE profile_usage ADD COLUMN model_id TEXT NOT NULL DEFAULT '';
ALTER TABLE profile_usage ADD COLUMN input_cost REAL NOT NULL DEFAULT 0;
ALTER TABLE profile_usage ADD COLUMN output_cost REAL NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_profile_usage_model_id ON profile_usage(model_id);
