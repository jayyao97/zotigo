package workspace

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

func (s *Store) AssignSession(ctx context.Context, sessionID string, workspaceID string) (SessionOrganization, error) {
	if sessionID == "" || workspaceID == "" {
		return SessionOrganization{}, ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SessionOrganization{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var projectID string
	var status WorkspaceStatus
	if err := tx.QueryRowContext(ctx, `SELECT project_id, status FROM workspaces WHERE id = ?`, workspaceID).Scan(&projectID, &status); err != nil {
		if err == sql.ErrNoRows {
			return SessionOrganization{}, ErrNotFound
		}
		return SessionOrganization{}, err
	}
	if status != WorkspaceStatusReady {
		return SessionOrganization{}, fmt.Errorf("%w: workspace is not ready", ErrConflict)
	}
	var position int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(workspace_position), 0) + 1000
		FROM session_organization WHERE workspace_id = ?
	`, workspaceID).Scan(&position); err != nil {
		return SessionOrganization{}, err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO session_organization(
			session_id, project_id, workspace_id, workspace_position,
			created_at, updated_at
		) VALUES(?, ?, ?, ?, ?, ?)
	`, sessionID, projectID, workspaceID, position, unixMillis(now), unixMillis(now)); err != nil {
		if isConstraintError(err) {
			return SessionOrganization{}, fmt.Errorf("%w: session is already organized", ErrConflict)
		}
		return SessionOrganization{}, err
	}
	if err := tx.Commit(); err != nil {
		return SessionOrganization{}, err
	}
	return s.GetSessionOrganization(ctx, sessionID)
}

func (s *Store) EnsureSessionOrganization(ctx context.Context, sessionID string) (SessionOrganization, error) {
	if organization, err := s.GetSessionOrganization(ctx, sessionID); err == nil {
		return organization, nil
	} else if err != ErrNotFound {
		return SessionOrganization{}, err
	}
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO session_organization(session_id, created_at, updated_at)
		VALUES(?, ?, ?) ON CONFLICT(session_id) DO NOTHING
	`, sessionID, unixMillis(now), unixMillis(now)); err != nil {
		return SessionOrganization{}, err
	}
	return s.GetSessionOrganization(ctx, sessionID)
}

func (s *Store) GetSessionOrganization(ctx context.Context, sessionID string) (SessionOrganization, error) {
	return scanOrganization(s.db.QueryRowContext(ctx, organizationSelect+` WHERE session_id = ?`, sessionID))
}

func (s *Store) ListSessionOrganizations(ctx context.Context) ([]SessionOrganization, error) {
	rows, err := s.db.QueryContext(ctx, organizationSelect+` ORDER BY updated_at DESC, session_id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list session organization: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]SessionOrganization, 0)
	for rows.Next() {
		organization, err := scanOrganization(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, organization)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) SetSessionTitle(ctx context.Context, sessionID string, title string) (SessionOrganization, error) {
	title = strings.TrimSpace(title)
	if title == "" || len(title) > 200 {
		return SessionOrganization{}, ErrInvalid
	}
	return s.updateOrganization(ctx, sessionID, `title = ?`, title)
}

func (s *Store) SetSessionPinned(ctx context.Context, sessionID string, pinned bool) (SessionOrganization, error) {
	organization, err := s.GetSessionOrganization(ctx, sessionID)
	if err != nil {
		return SessionOrganization{}, err
	}
	if organization.EffectiveArchived() {
		return SessionOrganization{}, fmt.Errorf("%w: archived session cannot be pinned", ErrConflict)
	}
	if !pinned {
		return s.updateOrganization(ctx, sessionID, `pinned_at = NULL, pinned_position = NULL`)
	}
	var position int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(pinned_position), 0) + 1000 FROM session_organization`).Scan(&position); err != nil {
		return SessionOrganization{}, err
	}
	now := unixMillis(time.Now().UTC())
	return s.updateOrganization(ctx, sessionID, `pinned_at = ?, pinned_position = ?`, now, position)
}

func (s *Store) SetSessionPosition(ctx context.Context, sessionID string, position int64) (SessionOrganization, error) {
	if position < 0 {
		return SessionOrganization{}, ErrInvalid
	}
	organization, err := s.GetSessionOrganization(ctx, sessionID)
	if err != nil {
		return SessionOrganization{}, err
	}
	if organization.WorkspaceID == nil {
		return SessionOrganization{}, fmt.Errorf("%w: unassigned session has no workspace position", ErrConflict)
	}
	return s.updateOrganization(ctx, sessionID, `workspace_position = ?`, position)
}

func (s *Store) SetSessionArchived(ctx context.Context, sessionID string, archived bool) (SessionOrganization, error) {
	if archived {
		now := unixMillis(time.Now().UTC())
		return s.updateOrganization(ctx, sessionID,
			`self_archived_at = ?, pinned_at = NULL, pinned_position = NULL`, now)
	}
	return s.updateOrganization(ctx, sessionID, `self_archived_at = NULL`)
}

func (s *Store) WorkspaceSessionIDs(ctx context.Context, workspaceID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT session_id FROM session_organization WHERE workspace_id = ? ORDER BY session_id
	`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	result := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, rows.Err()
}

func (s *Store) DeletedWorkspaceOwnsPath(ctx context.Context, path string) (bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM workspaces WHERE status = 'deleted' AND root_path = ?
	`, path).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *Store) updateOrganization(ctx context.Context, sessionID string, assignment string, args ...any) (SessionOrganization, error) {
	queryArgs := append(args, unixMillis(time.Now().UTC()), sessionID)
	result, err := s.db.ExecContext(ctx, `
		UPDATE session_organization SET `+assignment+`, revision = revision + 1, updated_at = ?
		WHERE session_id = ?
	`, queryArgs...)
	if err != nil {
		return SessionOrganization{}, fmt.Errorf("update session organization: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return SessionOrganization{}, err
	}
	if count == 0 {
		return SessionOrganization{}, ErrNotFound
	}
	return s.GetSessionOrganization(ctx, sessionID)
}

const organizationSelect = `
	SELECT session_id, project_id, workspace_id, title, pinned_at,
	       pinned_position, workspace_position, self_archived_at,
	       workspace_archived_at, revision, created_at, updated_at
	FROM session_organization`

func scanOrganization(row scanner) (SessionOrganization, error) {
	var organization SessionOrganization
	var projectID, workspaceID, title sql.NullString
	var pinnedAt, pinnedPosition, workspacePosition sql.NullInt64
	var selfArchivedAt, workspaceArchivedAt sql.NullInt64
	var createdAt, updatedAt int64
	if err := row.Scan(&organization.SessionID, &projectID, &workspaceID, &title,
		&pinnedAt, &pinnedPosition, &workspacePosition, &selfArchivedAt,
		&workspaceArchivedAt, &organization.Revision, &createdAt, &updatedAt); err != nil {
		return SessionOrganization{}, scanError("session organization", err)
	}
	organization.ProjectID = nullStringPointer(projectID)
	organization.WorkspaceID = nullStringPointer(workspaceID)
	organization.Title = nullStringPointer(title)
	organization.PinnedAt = nullableTime(pinnedAt)
	organization.PinnedPosition = nullIntPointer(pinnedPosition)
	organization.WorkspacePosition = nullIntPointer(workspacePosition)
	organization.SelfArchivedAt = nullableTime(selfArchivedAt)
	organization.WorkspaceArchivedAt = nullableTime(workspaceArchivedAt)
	organization.CreatedAt = fromUnixMillis(createdAt)
	organization.UpdatedAt = fromUnixMillis(updatedAt)
	return organization, nil
}

func nullStringPointer(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	result := value.String
	return &result
}

func nullIntPointer(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	result := value.Int64
	return &result
}
