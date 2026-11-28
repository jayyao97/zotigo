CREATE INDEX completion_triggers_watch ON completion_triggers(json_extract(payload,'$.watch_session_id'),expires_at) WHERE state='pending' AND json_extract(payload,'$.watch_session_id') IS NOT NULL;
