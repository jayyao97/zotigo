CREATE TABLE repositories (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  remote_url TEXT NOT NULL,
  canonical_path TEXT NOT NULL,
  default_branch TEXT NOT NULL DEFAULT '',
  default_commit TEXT NOT NULL DEFAULT '',
  clone_failure_count INTEGER NOT NULL DEFAULT 0,
  clone_next_attempt_at INTEGER NOT NULL DEFAULT 0,
  last_synced_at INTEGER,
  last_sync_error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX repositories_remote_url ON repositories(remote_url);
CREATE UNIQUE INDEX repositories_canonical_path ON repositories(canonical_path);
CREATE TABLE repository_preparations (
  repository_id TEXT PRIMARY KEY REFERENCES repositories(id) ON DELETE CASCADE,
  next_attempt_at INTEGER NOT NULL DEFAULT 0,
  failure_count INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE source_repositories (
  source_id TEXT PRIMARY KEY REFERENCES sources(id) ON DELETE CASCADE,
  repository_id TEXT NOT NULL REFERENCES repositories(id) ON DELETE RESTRICT,
  replacement_source_id TEXT REFERENCES sources(id) ON DELETE SET NULL
);
CREATE TABLE source_repository_migrations (
  source_id TEXT PRIMARY KEY REFERENCES sources(id) ON DELETE CASCADE,
  repository_id TEXT REFERENCES repositories(id) ON DELETE RESTRICT,
  status TEXT NOT NULL CHECK(status IN ('local','pending','retry','complete')),
  last_error TEXT NOT NULL DEFAULT '',
  next_attempt_at INTEGER NOT NULL DEFAULT 0,
  failure_count INTEGER NOT NULL DEFAULT 0
);
ALTER TABLE workspace_checkouts ADD COLUMN used_cached_base INTEGER NOT NULL DEFAULT 0;
ALTER TABLE workspace_checkouts ADD COLUMN base_synced_at INTEGER;
