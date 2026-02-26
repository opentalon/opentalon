-- Schema version: single row, version = last applied migration number.
-- Runner inserts/updates version after applying this file.
CREATE TABLE IF NOT EXISTS schema_version (
  version INTEGER NOT NULL PRIMARY KEY
);

-- Memories: general (actor_id NULL) or per-actor. Tags stored as JSON array.
CREATE TABLE IF NOT EXISTS memories (
  id TEXT PRIMARY KEY,
  actor_id TEXT,
  content TEXT NOT NULL,
  tags TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_memories_actor_id ON memories(actor_id);
CREATE INDEX IF NOT EXISTS idx_memories_created_at ON memories(created_at);

-- Sessions: one row per conversation. messages and metadata as JSON.
CREATE TABLE IF NOT EXISTS sessions (
  id TEXT PRIMARY KEY,
  messages TEXT NOT NULL,
  summary TEXT,
  active_model TEXT,
  metadata TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
