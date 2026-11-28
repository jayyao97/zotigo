ALTER TABLE sessions ADD COLUMN dialogue_updated_at INTEGER NOT NULL DEFAULT 0;

-- Older Codex imports recorded the local import time when a remote turn lacked
-- timestamps. Those rows are indistinguishable from local activity in the index.
-- Keep their session metadata (which includes backend activity) on upgrade.
UPDATE sessions
SET dialogue_updated_at = coalesce((SELECT max(message_at) FROM display_items WHERE session_id=sessions.id AND dialogue=1), 0)
WHERE agent != 'codex';
UPDATE sessions SET updated_at = dialogue_updated_at WHERE dialogue_updated_at > updated_at;
