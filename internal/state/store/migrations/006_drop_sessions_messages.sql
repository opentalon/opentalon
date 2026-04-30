-- Drop the vestigial sessions.messages column; all message data now lives
-- in the messages table (added in migration 005).
ALTER TABLE sessions DROP COLUMN messages;
