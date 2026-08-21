package workspace

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"
)

const schemaVersion = 3

type Store struct {
	db          *sql.DB
	rootDir     string
	operationMu sync.Mutex
}

// OpenReadOnly opens an existing catalog without running migrations or writing
// pragmas. It is intended for CLI projections; zotigod remains the sole writer.
func OpenReadOnly(rootDir string) (*Store, error) {
	if rootDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("workspace catalog home: %w", err)
		}
		rootDir = filepath.Join(home, ".zotigo")
	}
	dbPath := filepath.Join(rootDir, "catalog.sqlite")
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("open workspace catalog read-only: %w", err)
	}
	dsn := (&url.URL{Scheme: "file", Path: dbPath, RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open workspace catalog read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db, rootDir: rootDir}
	var version int
	if err := db.QueryRow(`SELECT version FROM schema_meta WHERE singleton = 1`).Scan(&version); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("read workspace catalog version: %w", err)
	}
	if version != schemaVersion {
		_ = db.Close()
		return nil, fmt.Errorf("workspace catalog version %d is not supported", version)
	}
	return store, nil
}

func Open(rootDir string) (*Store, error) {
	if rootDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("workspace catalog home: %w", err)
		}
		rootDir = filepath.Join(home, ".zotigo")
	}
	if err := os.MkdirAll(rootDir, 0o700); err != nil {
		return nil, fmt.Errorf("create workspace catalog root: %w", err)
	}

	dbPath := filepath.Join(rootDir, "catalog.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open workspace catalog: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db, rootDir: rootDir}
	if err := store.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := os.Chmod(dbPath, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("protect workspace catalog: %w", err)
	}
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) RootDir() string {
	return s.rootDir
}

func (s *Store) migrate(ctx context.Context) error {
	for _, pragma := range []string{
		`PRAGMA foreign_keys = ON`,
		`PRAGMA journal_mode = WAL`,
		`PRAGMA busy_timeout = 5000`,
	} {
		if _, err := s.db.ExecContext(ctx, pragma); err != nil {
			return fmt.Errorf("configure workspace catalog: %w", err)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin workspace catalog migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	statements := []string{
		`CREATE TABLE IF NOT EXISTS schema_meta (
			singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
			version INTEGER NOT NULL CHECK(version > 0)
		)`,
		`CREATE TABLE IF NOT EXISTS projects (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL CHECK(length(trim(name)) BETWEEN 1 AND 200),
			status TEXT NOT NULL DEFAULT 'active'
				CHECK(status IN ('active', 'archiving', 'archived', 'deleting')),
			archived_at INTEGER,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS sources (
			id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
			kind TEXT NOT NULL CHECK(kind IN ('git', 'folder')),
			canonical_path TEXT NOT NULL,
			git_common_dir TEXT,
			git_object_format TEXT,
			folder_mode TEXT,
			source_key TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			UNIQUE(project_id, canonical_path),
			UNIQUE(project_id, source_key),
			CHECK(
				(kind = 'git' AND git_common_dir IS NOT NULL
				 AND git_object_format IN ('sha1', 'sha256') AND folder_mode IS NULL)
				OR
				(kind = 'folder' AND git_common_dir IS NULL
				 AND git_object_format IS NULL
				 AND folder_mode IN ('direct', 'reference', 'copy'))
			)
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS sources_project_git_common_dir
			ON sources(project_id, git_common_dir) WHERE git_common_dir IS NOT NULL`,
		`CREATE TABLE IF NOT EXISTS workspaces (
			id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
			title TEXT NOT NULL CHECK(length(trim(title)) BETWEEN 1 AND 200),
			root_path TEXT NOT NULL UNIQUE,
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
		)`,
		`CREATE TABLE IF NOT EXISTS workspace_checkouts (
			workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
			source_id TEXT NOT NULL REFERENCES sources(id) ON DELETE RESTRICT,
			worktree_path TEXT NOT NULL UNIQUE,
			base_ref TEXT NOT NULL,
			base_commit TEXT NOT NULL,
			branch_name TEXT NOT NULL,
			owned_head TEXT NOT NULL,
			status TEXT NOT NULL CHECK(status IN ('planned', 'ready', 'error', 'archived')),
			error TEXT,
			PRIMARY KEY(workspace_id, source_id),
			CHECK((status = 'error') = (error IS NOT NULL))
		)`,
		`CREATE TABLE IF NOT EXISTS workspace_folders (
			workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
			source_id TEXT NOT NULL REFERENCES sources(id) ON DELETE RESTRICT,
			mode TEXT NOT NULL CHECK(mode IN ('direct', 'reference', 'copy')),
			target_path TEXT NOT NULL UNIQUE,
			direct_canonical_path TEXT,
			status TEXT NOT NULL CHECK(status IN ('planned', 'ready', 'error')),
			error TEXT,
			PRIMARY KEY(workspace_id, source_id),
			CHECK((status = 'error') = (error IS NOT NULL)),
			CHECK((mode = 'direct') = (direct_canonical_path IS NOT NULL))
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS workspace_folders_one_direct_source
			ON workspace_folders(direct_canonical_path) WHERE direct_canonical_path IS NOT NULL`,
		`CREATE TABLE IF NOT EXISTS session_organization (
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
			updated_at INTEGER NOT NULL,
			CHECK((project_id IS NULL) = (workspace_id IS NULL)),
			CHECK((pinned_at IS NULL) = (pinned_position IS NULL))
		)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate workspace catalog: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO schema_meta(singleton, version) VALUES(1, ?)
		ON CONFLICT(singleton) DO NOTHING
	`, schemaVersion); err != nil {
		return fmt.Errorf("record workspace catalog version: %w", err)
	}
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT version FROM schema_meta WHERE singleton = 1`).Scan(&version); err != nil {
		return fmt.Errorf("read workspace catalog version: %w", err)
	}
	if version == 1 {
		if _, err := tx.ExecContext(ctx, `
			ALTER TABLE projects ADD COLUMN status TEXT NOT NULL DEFAULT 'active'
			CHECK(status IN ('active', 'archiving', 'archived', 'deleting'))
		`); err != nil {
			return fmt.Errorf("migrate projects status: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `ALTER TABLE projects ADD COLUMN archived_at INTEGER`); err != nil {
			return fmt.Errorf("migrate projects archived_at: %w", err)
		}
		version = 2
	}
	if version == 2 {
		var hasOwnedHead bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM pragma_table_info('workspace_checkouts') WHERE name = 'owned_head'
			)
		`).Scan(&hasOwnedHead); err != nil {
			return fmt.Errorf("inspect workspace checkout ownership schema: %w", err)
		}
		if !hasOwnedHead {
			for _, statement := range []string{
				`ALTER TABLE workspace_checkouts RENAME TO workspace_checkouts_v2`,
				`CREATE TABLE workspace_checkouts (
					workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
					source_id TEXT NOT NULL REFERENCES sources(id) ON DELETE RESTRICT,
					worktree_path TEXT NOT NULL UNIQUE,
					base_ref TEXT NOT NULL,
					base_commit TEXT NOT NULL,
					branch_name TEXT NOT NULL,
					owned_head TEXT NOT NULL,
					status TEXT NOT NULL CHECK(status IN ('planned', 'ready', 'error', 'archived')),
					error TEXT,
					PRIMARY KEY(workspace_id, source_id),
					CHECK((status = 'error') = (error IS NOT NULL))
				)`,
				`INSERT INTO workspace_checkouts(
					workspace_id, source_id, worktree_path, base_ref, base_commit,
					branch_name, owned_head, status, error
				) SELECT
					workspace_id, source_id, worktree_path, base_ref, base_commit,
					branch_name, base_commit, status, error
				FROM workspace_checkouts_v2`,
				`DROP TABLE workspace_checkouts_v2`,
			} {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("migrate workspace checkout ownership: %w", err)
				}
			}
		}
		version = 3
	}
	if version != schemaVersion {
		return fmt.Errorf("workspace catalog version %d is not supported", version)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE schema_meta SET version = ? WHERE singleton = 1`, version); err != nil {
		return fmt.Errorf("update workspace catalog version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit workspace catalog migration: %w", err)
	}
	return nil
}
