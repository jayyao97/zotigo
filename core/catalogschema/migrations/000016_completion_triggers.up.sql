CREATE TABLE completion_triggers (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, payload TEXT NOT NULL, expires_at INTEGER NOT NULL, state TEXT NOT NULL CHECK(state IN ('pending','fired','cancelled')));
CREATE INDEX completion_triggers_session ON completion_triggers(session_id,state,expires_at);
UPDATE schema_meta SET version = 16 WHERE singleton = 1;
