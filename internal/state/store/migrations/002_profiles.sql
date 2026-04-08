-- Tracks known entities (upserted on each verified profile request).
CREATE TABLE IF NOT EXISTS entities (
  id TEXT PRIMARY KEY,
  group_id TEXT,
  first_seen TEXT NOT NULL,
  last_seen TEXT NOT NULL
);

-- Group → plugin/skill assignments (source of truth for tool access).
-- plugin_id matches the plugin name in config (e.g. "jira", "github").
-- source: "config" < "whoami" < "admin" — higher-priority sources are never downgraded.
CREATE TABLE IF NOT EXISTS group_plugins (
  group_id  TEXT NOT NULL,
  plugin_id TEXT NOT NULL,
  source    TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (group_id, plugin_id)
);
CREATE INDEX IF NOT EXISTS idx_group_plugins_group ON group_plugins(group_id);

-- Usage statistics per profile per LLM call.
CREATE TABLE IF NOT EXISTS profile_usage (
  id            TEXT PRIMARY KEY,
  entity_id     TEXT NOT NULL,
  group_id      TEXT,
  channel_id    TEXT NOT NULL,
  session_id    TEXT NOT NULL,
  input_tokens  INTEGER DEFAULT 0,
  output_tokens INTEGER DEFAULT 0,
  tool_calls    INTEGER DEFAULT 0,
  created_at    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_profile_usage_entity_id ON profile_usage(entity_id);
CREATE INDEX IF NOT EXISTS idx_profile_usage_created_at ON profile_usage(created_at);
