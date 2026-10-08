package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/catalogschema"
	_ "modernc.org/sqlite"
)

type sessionIndex struct {
	db         *sql.DB
	schemaLock *flock.Flock
}

func openSessionIndex(root string) (*sessionIndex, error) {
	db, err := catalogschema.OpenDatabase(root)
	if err != nil {
		return nil, fmt.Errorf("open session index: %w", err)
	}
	lease, err := catalogschema.Acquire(context.Background(), root, db, false)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &sessionIndex{db: db, schemaLock: lease}, nil
}

func (i *sessionIndex) upsert(ctx context.Context, meta Metadata) error {
	if meta.ApprovalPolicy == "" {
		meta.ApprovalPolicy = agent.ApprovalPolicyAuto
	}
	if meta.Agent == "" {
		meta.Agent = "zotigo"
	}
	promptConfig, err := json.Marshal(meta.PromptConfig)
	if err != nil {
		return fmt.Errorf("marshal session prompt config: %w", err)
	}
	capabilities, err := json.Marshal(meta.Capabilities)
	if err != nil {
		return fmt.Errorf("marshal session capabilities: %w", err)
	}
	_, err = i.db.ExecContext(ctx, `
		INSERT INTO sessions (id, working_directory, agent, profile_name, model, reasoning_effort,
			conversation_id, backend_version, backend_updated_at, backend_sync_version, approval_policy, prompt_config, capabilities, last_prompt, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			working_directory = excluded.working_directory,
			agent = excluded.agent,
			profile_name = excluded.profile_name,
			model = excluded.model,
			reasoning_effort = excluded.reasoning_effort,
			conversation_id = excluded.conversation_id,
			backend_version = excluded.backend_version,
			backend_updated_at = excluded.backend_updated_at,
			backend_sync_version = excluded.backend_sync_version,
			approval_policy = excluded.approval_policy,
			prompt_config = excluded.prompt_config,
			capabilities = excluded.capabilities,
			last_prompt = excluded.last_prompt,
			created_at = excluded.created_at,
			updated_at = excluded.updated_at
	`, meta.ID, meta.WorkingDirectory, meta.Agent, meta.ProfileName, meta.Model, meta.ReasoningEffort,
		meta.ConversationID, meta.BackendVersion, formatOptionalIndexTime(meta.BackendUpdatedAt), meta.BackendSyncVersion, meta.ApprovalPolicy, string(promptConfig), string(capabilities), meta.LastPrompt,
		formatIndexTime(meta.CreatedAt), formatIndexTime(meta.UpdatedAt))
	if err != nil {
		return fmt.Errorf("upsert session index: %w", err)
	}
	return nil
}

func (i *sessionIndex) delete(ctx context.Context, id string) error {
	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete session index: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM session_images WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("delete session image index: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete session index: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit session index delete: %w", err)
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
	query.WriteString(sessionMetadataQuery)
	args := make([]any, 0, 2)
	if filter.WorkingDirectory != "" {
		query.WriteString(` WHERE working_directory = ?`)
		args = append(args, filter.WorkingDirectory)
	}
	query.WriteString(` ORDER BY `)
	switch filter.OrderBy {
	case OrderByUpdatedAsc:
		query.WriteString(`updated_at ASC, id ASC`)
	case OrderByCreatedDesc:
		query.WriteString(`created_at DESC, id DESC`)
	case OrderByCreatedAsc:
		query.WriteString(`created_at ASC, id ASC`)
	case OrderByUpdatedDesc:
		fallthrough
	default:
		query.WriteString(`updated_at DESC, id DESC`)
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
		meta, err := scanSessionMetadata(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *meta)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate session index: %w", err)
	}
	return result, nil
}

const sessionMetadataQuery = `SELECT id, working_directory, agent, profile_name, model, reasoning_effort,
	conversation_id, backend_version, backend_updated_at, backend_sync_version, approval_policy, prompt_config, capabilities, last_prompt, created_at, updated_at FROM sessions`

func scanSessionMetadata(row interface{ Scan(...any) error }) (*Metadata, error) {
	var meta Metadata
	var createdAt, updatedAt, backendUpdatedAt int64
	var promptConfig, capabilities string
	if err := row.Scan(&meta.ID, &meta.WorkingDirectory, &meta.Agent, &meta.ProfileName,
		&meta.Model, &meta.ReasoningEffort, &meta.ConversationID, &meta.BackendVersion,
		&backendUpdatedAt, &meta.BackendSyncVersion, &meta.ApprovalPolicy, &promptConfig, &capabilities, &meta.LastPrompt, &createdAt, &updatedAt); err != nil {
		return nil, fmt.Errorf("scan session index: %w", err)
	}
	if err := json.Unmarshal([]byte(promptConfig), &meta.PromptConfig); err != nil {
		return nil, fmt.Errorf("decode session prompt config: %w", err)
	}
	if err := json.Unmarshal([]byte(capabilities), &meta.Capabilities); err != nil {
		return nil, fmt.Errorf("decode session capabilities: %w", err)
	}
	meta.CreatedAt = parseIndexTime(createdAt)
	meta.UpdatedAt = parseIndexTime(updatedAt)
	meta.BackendUpdatedAt = parseOptionalIndexTime(backendUpdatedAt)
	return &meta, nil
}

// ImageRef indexes an accepted per-session image blob without storing its bytes.
type ImageRef struct {
	SessionID string
	Name      string
	BlobPath  string
	MimeType  string

	SizeBytes int
	Width     int
	Height    int
	CreatedAt time.Time
}

func (i *sessionIndex) upsertImageRefs(ctx context.Context, refs []ImageRef) error {
	if len(refs) == 0 {
		return nil
	}
	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("upsert session image index: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO session_images (session_id, name, blob_path, mime_type, size_bytes, width, height, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id, name) DO UPDATE SET
			blob_path = excluded.blob_path,
			mime_type = excluded.mime_type,
			size_bytes = excluded.size_bytes,
			width = excluded.width,
			height = excluded.height,
			created_at = excluded.created_at
	`)
	if err != nil {
		return fmt.Errorf("prepare session image index upsert: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for _, ref := range refs {
		if _, err := stmt.ExecContext(ctx, ref.SessionID, ref.Name, ref.BlobPath, ref.MimeType, ref.SizeBytes, ref.Width, ref.Height, formatIndexTime(ref.CreatedAt)); err != nil {
			return fmt.Errorf("upsert session image index: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit session image index upsert: %w", err)
	}
	return nil
}

func (i *sessionIndex) deleteImageRefs(ctx context.Context, sessionID string, names []string) error {
	if len(names) == 0 {
		return nil
	}
	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete session image index: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `DELETE FROM session_images WHERE session_id = ? AND name = ?`)
	if err != nil {
		return fmt.Errorf("prepare session image index delete: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for _, name := range names {
		if _, err := stmt.ExecContext(ctx, sessionID, name); err != nil {
			return fmt.Errorf("delete session image index: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit session image index delete: %w", err)
	}
	return nil
}

func (i *sessionIndex) getImageRef(ctx context.Context, sessionID string, name string) (ImageRef, bool, error) {
	var ref ImageRef
	var createdAt int64
	err := i.db.QueryRowContext(ctx, `
		SELECT session_id, name, blob_path, mime_type, size_bytes, width, height, created_at
		FROM session_images
		WHERE session_id = ? AND name = ?
	`, sessionID, name).Scan(&ref.SessionID, &ref.Name, &ref.BlobPath, &ref.MimeType, &ref.SizeBytes, &ref.Width, &ref.Height, &createdAt)
	if err == nil {
		ref.CreatedAt = parseIndexTime(createdAt)
		return ref, true, nil
	}
	if err == sql.ErrNoRows {
		return ImageRef{}, false, nil
	}
	return ImageRef{}, false, fmt.Errorf("get session image index: %w", err)
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
	return errors.Join(i.db.Close(), i.schemaLock.Close())
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
		if err := decodeSession(data, &sess); err != nil {
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

func formatOptionalIndexTime(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return formatIndexTime(t)
}

func parseIndexTime(value int64) time.Time {
	return time.Unix(0, value).UTC()
}

func parseOptionalIndexTime(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return parseIndexTime(value)
}
