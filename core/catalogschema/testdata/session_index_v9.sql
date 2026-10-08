-- Frozen legacy DDL from the pre-consolidation session store.
CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			working_directory TEXT NOT NULL,
			agent TEXT NOT NULL DEFAULT 'zotigo',
			profile_name TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			reasoning_effort TEXT NOT NULL DEFAULT '',
			conversation_id TEXT NOT NULL DEFAULT '',
			backend_version TEXT NOT NULL DEFAULT '',
			backend_updated_at INTEGER NOT NULL DEFAULT 0,
			backend_sync_version INTEGER NOT NULL DEFAULT 0,
			approval_policy TEXT NOT NULL DEFAULT 'auto',
			prompt_config TEXT NOT NULL DEFAULT '{}',
			capabilities TEXT NOT NULL DEFAULT '{}',
			last_prompt TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		);
CREATE INDEX IF NOT EXISTS idx_sessions_updated_at ON sessions(updated_at);
CREATE INDEX IF NOT EXISTS idx_sessions_working_directory_updated_at ON sessions(working_directory, updated_at);
CREATE TABLE IF NOT EXISTS session_images (
			session_id TEXT NOT NULL,
			name TEXT NOT NULL,
			blob_path TEXT NOT NULL,
			mime_type TEXT NOT NULL,
			size_bytes INTEGER NOT NULL,
			width INTEGER NOT NULL,
			height INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			PRIMARY KEY (session_id, name)
		);
CREATE INDEX IF NOT EXISTS idx_session_images_session_id ON session_images(session_id);
CREATE TABLE IF NOT EXISTS metadata (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
CREATE TABLE IF NOT EXISTS display_items (
	 session_id TEXT NOT NULL, sequence INTEGER NOT NULL, message_at INTEGER NOT NULL,
	 dialogue INTEGER NOT NULL, offset INTEGER NOT NULL, length INTEGER NOT NULL,
	 PRIMARY KEY(session_id, sequence));
	 CREATE INDEX IF NOT EXISTS idx_display_time ON display_items(session_id, message_at, sequence);
	 CREATE INDEX IF NOT EXISTS idx_display_dialogue_time ON display_items(session_id, dialogue, message_at);
	 CREATE TABLE IF NOT EXISTS display_index_files (session_id TEXT PRIMARY KEY, size INTEGER NOT NULL, mtime INTEGER NOT NULL, observed_size INTEGER NOT NULL DEFAULT -1);
