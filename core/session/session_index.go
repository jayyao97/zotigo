package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type sessionIndex struct {
	db *sql.DB
}

func openSessionIndex(path string) (*sessionIndex, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open session index: %w", err)
	}
	db.SetMaxOpenConns(1)
	index := &sessionIndex{db: db}
	if err := index.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return index, nil
}

func (i *sessionIndex) migrate(ctx context.Context) error {
	stmts := []string{
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA journal_mode = WAL`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			working_directory TEXT NOT NULL,
			last_prompt TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_updated_at ON sessions(updated_at)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_working_directory_updated_at ON sessions(working_directory, updated_at)`,
		`CREATE TABLE IF NOT EXISTS metadata (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
	}
	for _, stmt := range stmts {
		if _, err := i.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate session index: %w", err)
		}
	}
	return nil
}

func (i *sessionIndex) upsert(ctx context.Context, meta Metadata) error {
	_, err := i.db.ExecContext(ctx, `
		INSERT INTO sessions (id, working_directory, last_prompt, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			working_directory = excluded.working_directory,
			last_prompt = excluded.last_prompt,
			created_at = excluded.created_at,
			updated_at = excluded.updated_at
	`, meta.ID, meta.WorkingDirectory, meta.LastPrompt, formatIndexTime(meta.CreatedAt), formatIndexTime(meta.UpdatedAt))
	if err != nil {
		return fmt.Errorf("upsert session index: %w", err)
	}
	return nil
}

func (i *sessionIndex) delete(ctx context.Context, id string) error {
	if _, err := i.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete session index: %w", err)
	}
	return nil
}

func (i *sessionIndex) deleteExcept(ctx context.Context, keep map[string]struct{}) error {
	rows, err := i.db.QueryContext(ctx, `SELECT id FROM sessions`)
	if err != nil {
		return fmt.Errorf("list session index ids: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var remove []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scan session index id: %w", err)
		}
		if _, ok := keep[id]; !ok {
			remove = append(remove, id)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate session index ids: %w", err)
	}
	for _, id := range remove {
		if err := i.delete(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func (i *sessionIndex) list(ctx context.Context, filter ListFilter) ([]Metadata, error) {
	var query strings.Builder
	query.WriteString(`SELECT id, working_directory, last_prompt, created_at, updated_at FROM sessions`)
	args := make([]any, 0, 2)
	if filter.WorkingDirectory != "" {
		query.WriteString(` WHERE working_directory = ?`)
		args = append(args, filter.WorkingDirectory)
	}
	query.WriteString(` ORDER BY `)
	switch filter.OrderBy {
	case OrderByUpdatedAsc:
		query.WriteString(`updated_at ASC`)
	case OrderByCreatedDesc:
		query.WriteString(`created_at DESC`)
	case OrderByCreatedAsc:
		query.WriteString(`created_at ASC`)
	case OrderByUpdatedDesc:
		fallthrough
	default:
		query.WriteString(`updated_at DESC`)
	}
	if filter.Limit > 0 {
		query.WriteString(` LIMIT ?`)
		args = append(args, filter.Limit)
	}

	rows, err := i.db.QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("list session index: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result []Metadata
	for rows.Next() {
		var meta Metadata
		var createdAt int64
		var updatedAt int64
		if err := rows.Scan(&meta.ID, &meta.WorkingDirectory, &meta.LastPrompt, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan session index: %w", err)
		}
		meta.CreatedAt = parseIndexTime(createdAt)
		meta.UpdatedAt = parseIndexTime(updatedAt)
		result = append(result, meta)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate session index: %w", err)
	}
	return result, nil
}

func (i *sessionIndex) bootstrapped(ctx context.Context) (bool, error) {
	var value string
	err := i.db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key = 'bootstrapped'`).Scan(&value)
	if err == nil {
		return value == "1", nil
	}
	if err == sql.ErrNoRows {
		return false, nil
	}
	return false, fmt.Errorf("read session index metadata: %w", err)
}

func (i *sessionIndex) markBootstrapped(ctx context.Context) error {
	if err := i.setMetadata(ctx, "bootstrapped", "1"); err != nil {
		return fmt.Errorf("mark session index bootstrapped: %w", err)
	}
	return nil
}

func (i *sessionIndex) metadataInt64(ctx context.Context, key string) (int64, bool, error) {
	var value int64
	err := i.db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key = ?`, key).Scan(&value)
	if err == nil {
		return value, true, nil
	}
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	return 0, false, fmt.Errorf("read session index metadata %s: %w", key, err)
}

func (i *sessionIndex) setMetadata(ctx context.Context, key string, value any) error {
	if _, err := i.db.ExecContext(ctx, `
		INSERT INTO metadata (key, value)
		VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, key, value); err != nil {
		return fmt.Errorf("write session index metadata %s: %w", key, err)
	}
	return nil
}

func (i *sessionIndex) close() error {
	return i.db.Close()
}

func (s *FileStore) bootstrapSessionIndex() error {
	ctx := context.Background()
	bootstrapped, err := s.index.bootstrapped(ctx)
	if err != nil {
		return err
	}
	if !bootstrapped {
		if err := s.bootstrapSessionIndexFromFiles(ctx); err != nil {
			return err
		}
		if err := s.index.markBootstrapped(ctx); err != nil {
			return err
		}
	}
	return s.syncSessionIndexFromLegacyRegistry(ctx)
}

func (s *FileStore) bootstrapSessionIndexFromFiles(ctx context.Context) error {
	paths, err := filepath.Glob(filepath.Join(s.rootDir, "sessions", "*.json"))
	if err != nil {
		return fmt.Errorf("scan session files: %w", err)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read session for index bootstrap: %w", err)
		}
		var sess Session
		if err := json.Unmarshal(data, &sess); err != nil {
			continue
		}
		if sess.ID == "" {
			continue
		}
		if err := s.index.upsert(ctx, sess.Metadata); err != nil {
			return err
		}
	}
	return nil
}

func (s *FileStore) syncSessionIndexFromLegacyRegistry(ctx context.Context) error {
	info, err := os.Stat(s.registryPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat legacy registry for session index sync: %w", err)
	}
	mtime := info.ModTime().UnixNano()
	last, ok, err := s.index.metadataInt64(ctx, "legacy_registry_mtime")
	if err != nil {
		return err
	}
	if ok && last >= mtime {
		return nil
	}

	reg, err := s.loadRegistryStrict()
	if err != nil {
		return nil
	}
	keep := make(map[string]struct{}, len(reg.Sessions))
	for _, meta := range reg.Sessions {
		if meta.ID == "" {
			continue
		}
		if err := s.index.upsert(ctx, meta); err != nil {
			return err
		}
		keep[meta.ID] = struct{}{}
	}
	if err := s.index.deleteExcept(ctx, keep); err != nil {
		return err
	}
	return s.index.setMetadata(ctx, "legacy_registry_mtime", mtime)
}

func (s *FileStore) recordLegacyRegistryMTime(ctx context.Context) error {
	info, err := os.Stat(s.registryPath)
	if err != nil {
		return fmt.Errorf("stat legacy registry after write: %w", err)
	}
	return s.index.setMetadata(ctx, "legacy_registry_mtime", info.ModTime().UnixNano())
}

func formatIndexTime(t time.Time) int64 {
	return t.UTC().UnixNano()
}

func parseIndexTime(value int64) time.Time {
	return time.Unix(0, value).UTC()
}
