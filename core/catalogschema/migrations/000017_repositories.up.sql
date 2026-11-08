CREATE TABLE repositories (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  remote_url TEXT NOT NULL UNIQUE,
  canonical_path TEXT NOT NULL UNIQUE,
  default_branch TEXT NOT NULL DEFAULT '',
  default_commit TEXT NOT NULL DEFAULT '',
  last_synced_at INTEGER,
  last_sync_error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE source_repositories (
  source_id TEXT PRIMARY KEY REFERENCES sources(id) ON DELETE CASCADE,
  repository_id TEXT NOT NULL REFERENCES repositories(id) ON DELETE RESTRICT,
  replacement_source_id TEXT REFERENCES sources(id) ON DELETE RESTRICT
);
CREATE TABLE source_repository_migrations (
  source_id TEXT PRIMARY KEY REFERENCES sources(id) ON DELETE CASCADE,
  repository_id TEXT REFERENCES repositories(id) ON DELETE RESTRICT,
  status TEXT NOT NULL CHECK(status IN ('local','pending','retry','complete')),
  last_error TEXT NOT NULL DEFAULT '',
  next_attempt_at INTEGER NOT NULL DEFAULT 0
);
ALTER TABLE workspace_checkouts ADD COLUMN used_cached_base INTEGER NOT NULL DEFAULT 0;
ALTER TABLE workspace_checkouts ADD COLUMN base_synced_at INTEGER;
UPDATE schema_meta SET version = 17 WHERE singleton = 1;
