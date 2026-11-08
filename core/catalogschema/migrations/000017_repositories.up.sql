CREATE TABLE repositories (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  remote_url TEXT NOT NULL UNIQUE,
  canonical_path TEXT NOT NULL UNIQUE,
  default_branch TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE source_repositories (
  source_id TEXT PRIMARY KEY REFERENCES sources(id) ON DELETE CASCADE,
  repository_id TEXT NOT NULL REFERENCES repositories(id) ON DELETE RESTRICT,
  replacement_source_id TEXT REFERENCES sources(id) ON DELETE RESTRICT,
  imported INTEGER NOT NULL DEFAULT 0 CHECK(imported IN (0,1))
);
UPDATE schema_meta SET version = 17 WHERE singleton = 1;
