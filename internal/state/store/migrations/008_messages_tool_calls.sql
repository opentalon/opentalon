-- Persist native tool-call structured data alongside the existing message
-- (role, content) pair. When the orchestrator routes a message through a
-- provider that supports native function calling, the assistant turn carries
-- ToolCalls (one or more name+args pairs) and the role=tool reply carries
-- ToolCallID. Without persistence both vanish at AddMessage time and any
-- after-the-fact session analysis is reduced to parsing free-text content --
-- which only works for the legacy text-based tool-calling path. Messages
-- written via the text-based path leave both columns NULL.
--
-- Schema portability: TEXT-only (tool_calls holds a JSON-encoded array,
-- mirroring messages.content; tool_call_id is a short opaque id from the
-- provider). No jsonb / generated columns -- the same migration runs on
-- SQLite and PostgreSQL. See top-of-file comment in 007_ai_debug_events.sql.
ALTER TABLE messages ADD COLUMN tool_calls TEXT;
ALTER TABLE messages ADD COLUMN tool_call_id TEXT;
