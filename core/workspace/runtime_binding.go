package workspace

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

func (s *Store) GetRuntimeWorkspaceBinding(ctx context.Context, workspaceID string, agent string) (RuntimeWorkspaceBinding, error) {
	if workspaceID == "" || agent == "" {
		return RuntimeWorkspaceBinding{}, ErrInvalid
	}
	return scanRuntimeWorkspaceBinding(s.db.QueryRowContext(ctx, `
		SELECT workspace_id, agent, state, external_id, create_key, create_name,
		       create_root, revision, backend_version, created_at, updated_at
		FROM runtime_workspace_bindings WHERE workspace_id = ? AND agent = ?
	`, workspaceID, agent))
}

func (s *Store) BeginRuntimeWorkspaceBinding(ctx context.Context, workspaceID string, agent string, createKey string, name string, root string) (RuntimeWorkspaceBinding, bool, error) {
	if workspaceID == "" || agent == "" || createKey == "" || name == "" || root == "" {
		return RuntimeWorkspaceBinding{}, false, ErrInvalid
	}
	now := unixMillis(time.Now().UTC())
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO runtime_workspace_bindings(
			workspace_id, agent, state, create_key, create_name, create_root,
			revision, created_at, updated_at
		) VALUES(?, ?, 'creating', ?, ?, ?, 1, ?, ?)
		ON CONFLICT(workspace_id, agent) DO NOTHING
	`, workspaceID, agent, createKey, name, root, now, now)
	if err != nil {
		return RuntimeWorkspaceBinding{}, false, fmt.Errorf("begin runtime workspace binding: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return RuntimeWorkspaceBinding{}, false, fmt.Errorf("inspect runtime workspace binding insert: %w", err)
	}
	binding, err := s.GetRuntimeWorkspaceBinding(ctx, workspaceID, agent)
	return binding, rows == 1, err
}

func (s *Store) ReuseRuntimeWorkspace(ctx context.Context, workspaceID string, agent string, externalID string, backendVersion string) (RuntimeWorkspaceBinding, error) {
	if workspaceID == "" || agent == "" || externalID == "" {
		return RuntimeWorkspaceBinding{}, ErrInvalid
	}
	now := unixMillis(time.Now().UTC())
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO runtime_workspace_bindings(
			workspace_id, agent, state, external_id, revision, backend_version,
			created_at, updated_at
		) VALUES(?, ?, 'bound', ?, 1, NULLIF(?, ''), ?, ?)
		ON CONFLICT(workspace_id, agent) DO NOTHING
	`, workspaceID, agent, externalID, backendVersion, now, now)
	if err != nil {
		return RuntimeWorkspaceBinding{}, fmt.Errorf("reuse runtime workspace: %w", err)
	}
	binding, err := s.GetRuntimeWorkspaceBinding(ctx, workspaceID, agent)
	if err != nil {
		return RuntimeWorkspaceBinding{}, err
	}
	if binding.State != RuntimeWorkspaceBindingBound || binding.ExternalID != externalID {
		return RuntimeWorkspaceBinding{}, fmt.Errorf("%w: runtime workspace binding changed concurrently", ErrConflict)
	}
	return binding, nil
}

func (s *Store) CompleteRuntimeWorkspaceBinding(ctx context.Context, binding RuntimeWorkspaceBinding, externalID string, backendVersion string) (RuntimeWorkspaceBinding, error) {
	if binding.State != RuntimeWorkspaceBindingCreating || externalID == "" {
		return RuntimeWorkspaceBinding{}, ErrInvalid
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE runtime_workspace_bindings
		SET state = 'bound', external_id = ?, revision = revision + 1,
		    backend_version = NULLIF(?, ''), updated_at = ?
		WHERE workspace_id = ? AND agent = ? AND state = 'creating'
		  AND revision = ? AND create_key = ?
	`, externalID, backendVersion, unixMillis(time.Now().UTC()), binding.WorkspaceID,
		binding.Agent, binding.Revision, binding.CreateKey)
	if err != nil {
		return RuntimeWorkspaceBinding{}, fmt.Errorf("complete runtime workspace binding: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return RuntimeWorkspaceBinding{}, fmt.Errorf("inspect runtime workspace binding update: %w", err)
	}
	if rows != 1 {
		return RuntimeWorkspaceBinding{}, fmt.Errorf("%w: runtime workspace binding changed concurrently", ErrConflict)
	}
	return s.GetRuntimeWorkspaceBinding(ctx, binding.WorkspaceID, binding.Agent)
}

func (s *Store) RebuildRuntimeWorkspaceBinding(ctx context.Context, binding RuntimeWorkspaceBinding, createKey string, name string, root string) (RuntimeWorkspaceBinding, error) {
	if binding.State != RuntimeWorkspaceBindingBound || createKey == "" || name == "" || root == "" {
		return RuntimeWorkspaceBinding{}, ErrInvalid
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE runtime_workspace_bindings
		SET state = 'creating', external_id = NULL, create_key = ?, create_name = ?,
		    create_root = ?, revision = revision + 1, backend_version = NULL, updated_at = ?
		WHERE workspace_id = ? AND agent = ? AND state = 'bound'
		  AND external_id = ? AND revision = ?
	`, createKey, name, root, unixMillis(time.Now().UTC()), binding.WorkspaceID,
		binding.Agent, binding.ExternalID, binding.Revision)
	if err != nil {
		return RuntimeWorkspaceBinding{}, fmt.Errorf("rebuild runtime workspace binding: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return RuntimeWorkspaceBinding{}, fmt.Errorf("inspect runtime workspace binding rebuild: %w", err)
	}
	if rows != 1 {
		return RuntimeWorkspaceBinding{}, fmt.Errorf("%w: runtime workspace binding changed concurrently", ErrConflict)
	}
	return s.GetRuntimeWorkspaceBinding(ctx, binding.WorkspaceID, binding.Agent)
}

func (s *Store) ReplaceRuntimeWorkspaceBinding(ctx context.Context, binding RuntimeWorkspaceBinding, externalID string, backendVersion string) (RuntimeWorkspaceBinding, error) {
	if binding.State != RuntimeWorkspaceBindingBound || externalID == "" {
		return RuntimeWorkspaceBinding{}, ErrInvalid
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE runtime_workspace_bindings
		SET external_id = ?, revision = revision + 1, backend_version = NULLIF(?, ''), updated_at = ?
		WHERE workspace_id = ? AND agent = ? AND state = 'bound'
		  AND external_id = ? AND revision = ?
	`, externalID, backendVersion, unixMillis(time.Now().UTC()), binding.WorkspaceID,
		binding.Agent, binding.ExternalID, binding.Revision)
	if err != nil {
		return RuntimeWorkspaceBinding{}, fmt.Errorf("replace runtime workspace binding: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return RuntimeWorkspaceBinding{}, fmt.Errorf("inspect runtime workspace binding replacement: %w", err)
	}
	if rows != 1 {
		return RuntimeWorkspaceBinding{}, fmt.Errorf("%w: runtime workspace binding changed concurrently", ErrConflict)
	}
	return s.GetRuntimeWorkspaceBinding(ctx, binding.WorkspaceID, binding.Agent)
}

func (s *Store) RotateRuntimeWorkspaceCreateKey(ctx context.Context, binding RuntimeWorkspaceBinding, createKey string) (RuntimeWorkspaceBinding, error) {
	if binding.State != RuntimeWorkspaceBindingCreating || createKey == "" {
		return RuntimeWorkspaceBinding{}, ErrInvalid
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE runtime_workspace_bindings
		SET create_key = ?, revision = revision + 1, updated_at = ?
		WHERE workspace_id = ? AND agent = ? AND state = 'creating'
		  AND create_key = ? AND revision = ?
	`, createKey, unixMillis(time.Now().UTC()), binding.WorkspaceID, binding.Agent,
		binding.CreateKey, binding.Revision)
	if err != nil {
		return RuntimeWorkspaceBinding{}, fmt.Errorf("rotate runtime workspace create key: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return RuntimeWorkspaceBinding{}, fmt.Errorf("inspect runtime workspace create key rotation: %w", err)
	}
	if rows != 1 {
		return RuntimeWorkspaceBinding{}, fmt.Errorf("%w: runtime workspace binding changed concurrently", ErrConflict)
	}
	return s.GetRuntimeWorkspaceBinding(ctx, binding.WorkspaceID, binding.Agent)
}

func scanRuntimeWorkspaceBinding(row scanner) (RuntimeWorkspaceBinding, error) {
	var binding RuntimeWorkspaceBinding
	var externalID, createKey, createName, createRoot, backendVersion sql.NullString
	var createdAt, updatedAt int64
	if err := row.Scan(&binding.WorkspaceID, &binding.Agent, &binding.State,
		&externalID, &createKey, &createName, &createRoot, &binding.Revision,
		&backendVersion, &createdAt, &updatedAt); err != nil {
		return RuntimeWorkspaceBinding{}, scanError("runtime workspace binding", err)
	}
	binding.ExternalID = externalID.String
	binding.CreateKey = createKey.String
	binding.CreateName = createName.String
	binding.CreateRoot = createRoot.String
	binding.BackendVersion = backendVersion.String
	binding.CreatedAt = fromUnixMillis(createdAt)
	binding.UpdatedAt = fromUnixMillis(updatedAt)
	return binding, nil
}
