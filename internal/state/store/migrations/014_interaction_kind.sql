-- interaction_kind + system_source: the "who drove this run" axis, distinct from
-- the actor (staff vs client) axis. It lets a non-interactive, programmatic call
-- made by the host backend (a background job, scheduled task, or server-side
-- feature) run through the same pipeline as a human chat, be recorded uniformly,
-- yet be filtered out of a customer's chat spend and hidden from customer-facing
-- session lists.
--
-- interaction_kind values:
--   'chat'   — a human, multi-turn conversation.
--   'system' — a single programmatic call originated by the backend, on behalf
--              of a real (entity, user) but not typed by that user.
-- An extensible string enum; readers treat any unknown value as opaque.
--
-- Two placements, decided from two different sources, deliberately decoupled:
--   sessions.interaction_kind      — the nature of the whole conversation, set
--                                    ONCE at session creation. A resumed session
--                                    keeps its kind.
--   profile_usage.interaction_kind — the nature of THIS run, read from the run's
--                                    Profile at record time. A system-triggered
--                                    run injected into a chat session records
--                                    'system' while the session stays 'chat', so
--                                    the spend-limit query can exclude system
--                                    usage from the customer's chat total.
--
-- profile_usage.system_source: a nullable per-feature label (e.g. 'csv_mapping',
-- 'job_notify', ...) set only for system runs, so cost and debugging can be
-- attributed across many in-app system features instead of one blurred 'system'
-- bucket. NULL for chat runs.
--
-- Portability: TEXT only (no enums / check constraints). NOT NULL DEFAULT 'chat'
-- backfills every existing row to 'chat'; system_source stays NULL. Runs on
-- SQLite and PostgreSQL.
--
-- Deploy order: the api-plugin reads interaction_kind by name (session filter +
-- stats aggregations). Core applies migrations at startup before it loads
-- plugins, so ship core-with-014 before any api-plugin build that SELECTs the
-- column (same rule as migration 013's metadata column).
ALTER TABLE sessions ADD COLUMN interaction_kind TEXT NOT NULL DEFAULT 'chat';
ALTER TABLE profile_usage ADD COLUMN interaction_kind TEXT NOT NULL DEFAULT 'chat';
ALTER TABLE profile_usage ADD COLUMN system_source TEXT;

-- Backs the spend-limit query (sum per entity_id + interaction_kind over a time
-- window). The existing idx_profile_usage_entity_created (entity_id, created_at)
-- does not cover the interaction_kind predicate.
CREATE INDEX IF NOT EXISTS idx_profile_usage_entity_kind_created
  ON profile_usage(entity_id, interaction_kind, created_at);

-- Session-list + stats filtering by kind (api-plugin read path).
CREATE INDEX IF NOT EXISTS idx_sessions_interaction_kind
  ON sessions(interaction_kind);
