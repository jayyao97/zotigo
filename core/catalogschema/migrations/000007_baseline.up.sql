CREATE TABLE schema_meta (
  singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
  version INTEGER NOT NULL CHECK(version > 0)
);

CREATE TABLE projects (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL CHECK(length(trim(name)) BETWEEN 1 AND 200),
  storage_name TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active'
    CHECK(status IN ('active', 'archiving', 'archived', 'deleting')),
  archived_at INTEGER,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

CREATE TABLE sources (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  kind TEXT NOT NULL CHECK(kind IN ('git', 'folder')),
  canonical_path TEXT NOT NULL,
  git_common_dir TEXT,
  git_object_format TEXT,
  folder_mode TEXT,
  source_key TEXT NOT NULL,
  registered INTEGER NOT NULL DEFAULT 1 CHECK(registered IN (0, 1)),
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,

  CHECK(
    (kind = 'git' AND git_common_dir IS NOT NULL
     AND git_object_format IN ('sha1', 'sha256') AND folder_mode IS NULL)
    OR
    (kind = 'folder' AND git_common_dir IS NULL
     AND git_object_format IS NULL
     AND folder_mode IN ('direct', 'reference', 'copy'))
  )
);

CREATE UNIQUE INDEX sources_project_git_common_dir
  ON sources(project_id, git_common_dir) WHERE git_common_dir IS NOT NULL;

CREATE TABLE workspaces (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
  title TEXT NOT NULL CHECK(length(trim(title)) BETWEEN 1 AND 200),
  storage_name TEXT NOT NULL,
  root_path TEXT NOT NULL,
  owner_nonce TEXT NOT NULL,
  status TEXT NOT NULL CHECK(status IN
    ('provisioning', 'ready', 'error', 'archiving', 'archived', 'deleting', 'deleted')),
  error TEXT,
  archived_at INTEGER,
  deleted_at INTEGER,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  CHECK((status = 'error') = (error IS NOT NULL)),
  CHECK((status = 'deleted') = (deleted_at IS NOT NULL))
);

CREATE TABLE workspace_checkouts (
  workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  source_id TEXT NOT NULL REFERENCES sources(id) ON DELETE RESTRICT,
  worktree_path TEXT NOT NULL,
  base_ref TEXT NOT NULL,
  base_commit TEXT NOT NULL,
  branch_name TEXT NOT NULL,
  owned_head TEXT NOT NULL,
  status TEXT NOT NULL CHECK(status IN ('planned', 'ready', 'error', 'archived')),
  error TEXT,
  PRIMARY KEY(workspace_id, source_id),
  CHECK((status = 'error') = (error IS NOT NULL))
);

CREATE TABLE workspace_folders (
  workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  source_id TEXT NOT NULL REFERENCES sources(id) ON DELETE RESTRICT,
  mode TEXT NOT NULL CHECK(mode IN ('direct', 'reference', 'copy')),
  target_path TEXT NOT NULL,
  direct_canonical_path TEXT,
  status TEXT NOT NULL CHECK(status IN ('planned', 'ready', 'error')),
  error TEXT,
  PRIMARY KEY(workspace_id, source_id),
  CHECK((status = 'error') = (error IS NOT NULL)),
  CHECK((mode = 'direct') = (direct_canonical_path IS NOT NULL))
);

CREATE UNIQUE INDEX workspace_folders_one_direct_source
  ON workspace_folders(direct_canonical_path) WHERE direct_canonical_path IS NOT NULL;

CREATE TABLE session_organization (
  session_id TEXT PRIMARY KEY,
  project_id TEXT REFERENCES projects(id) ON DELETE RESTRICT,
  workspace_id TEXT REFERENCES workspaces(id) ON DELETE RESTRICT,
  title TEXT,
  pinned_at INTEGER,
  pinned_position INTEGER,
  workspace_position INTEGER,
  self_archived_at INTEGER,
  workspace_archived_at INTEGER,
  revision INTEGER NOT NULL DEFAULT 1 CHECK(revision > 0),
  created_at INTEGER NOT NULL,
  activity_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  CHECK((project_id IS NULL) = (workspace_id IS NULL)),
  CHECK((pinned_at IS NULL) = (pinned_position IS NULL))
);

CREATE TABLE runtime_workspace_bindings (
  workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  agent TEXT NOT NULL,
  state TEXT NOT NULL CHECK(state IN ('creating', 'bound')),
  external_id TEXT,
  create_key TEXT,
  create_name TEXT,
  create_root TEXT,
  revision INTEGER NOT NULL CHECK(revision > 0),
  backend_version TEXT,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY(workspace_id, agent),
  CHECK(
    (state = 'creating' AND external_id IS NULL AND create_key IS NOT NULL
     AND create_name IS NOT NULL AND create_root IS NOT NULL)
    OR
    (state = 'bound' AND external_id IS NOT NULL)
  )
);
CREATE UNIQUE INDEX projects_storage_name ON projects(storage_name);
CREATE UNIQUE INDEX sources_project_id_canonical_path ON sources(project_id, canonical_path);
CREATE UNIQUE INDEX sources_project_id_source_key ON sources(project_id, source_key);
CREATE UNIQUE INDEX workspaces_root_path ON workspaces(root_path);
CREATE UNIQUE INDEX workspace_checkouts_worktree_path ON workspace_checkouts(worktree_path);
CREATE UNIQUE INDEX workspace_folders_target_path ON workspace_folders(target_path);

INSERT INTO schema_meta(singleton, version) VALUES(1, 7);
