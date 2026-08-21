package workspace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrNotFound    = errors.New("workspace catalog record not found")
	ErrConflict    = errors.New("workspace catalog conflict")
	ErrInvalid     = errors.New("invalid workspace catalog input")
	ErrSourceInUse = errors.New("workspace source is in use")
)

func (s *Store) CreateProject(ctx context.Context, name string) (Project, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 200 {
		return Project{}, ErrInvalid
	}
	now := time.Now().UTC()
	project := Project{
		ID:        "project_" + uuid.NewString(),
		Name:      name,
		CreatedAt: now,
		UpdatedAt: now,
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO projects(id, name, created_at, updated_at) VALUES(?, ?, ?, ?)
	`, project.ID, project.Name, unixMillis(now), unixMillis(now))
	if err != nil {
		return Project{}, fmt.Errorf("create project: %w", err)
	}
	return project, nil
}

func (s *Store) GetProject(ctx context.Context, id string) (Project, error) {
	return scanProject(s.db.QueryRowContext(ctx, `
		SELECT id, name, created_at, updated_at FROM projects WHERE id = ?
	`, id))
}

func (s *Store) ListProjects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, created_at, updated_at
		FROM projects ORDER BY updated_at DESC, id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer func() { _ = rows.Close() }()

	projects := make([]Project, 0)
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	return projects, nil
}

func (s *Store) AddSource(ctx context.Context, projectID string, input SourceInput) (Source, error) {
	if err := validateSourceInput(input); err != nil {
		return Source{}, err
	}
	now := time.Now().UTC()
	source := Source{
		ID:              "source_" + uuid.NewString(),
		ProjectID:       projectID,
		Kind:            input.Kind,
		CanonicalPath:   input.CanonicalPath,
		GitCommonDir:    input.GitCommonDir,
		GitObjectFormat: input.GitObjectFormat,
		FolderMode:      input.FolderMode,
		SourceKey:       input.SourceKey,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sources(
			id, project_id, kind, canonical_path, git_common_dir,
			git_object_format, folder_mode, source_key, created_at, updated_at
		) VALUES(?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?)
	`, source.ID, source.ProjectID, source.Kind, source.CanonicalPath,
		source.GitCommonDir, source.GitObjectFormat, source.FolderMode,
		source.SourceKey, unixMillis(now), unixMillis(now))
	if err != nil {
		if isConstraintError(err) {
			return Source{}, fmt.Errorf("%w: source already registered or project missing", ErrConflict)
		}
		return Source{}, fmt.Errorf("add source: %w", err)
	}
	return source, nil
}

func (s *Store) GetSource(ctx context.Context, projectID string, sourceID string) (Source, error) {
	return scanSource(s.db.QueryRowContext(ctx, `
		SELECT id, project_id, kind, canonical_path, git_common_dir,
		       git_object_format, folder_mode, source_key, created_at, updated_at
		FROM sources WHERE id = ? AND project_id = ?
	`, sourceID, projectID))
}

func (s *Store) ListSources(ctx context.Context, projectID string) ([]Source, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, project_id, kind, canonical_path, git_common_dir,
		       git_object_format, folder_mode, source_key, created_at, updated_at
		FROM sources WHERE project_id = ? ORDER BY source_key ASC, id ASC
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}
	defer func() { _ = rows.Close() }()

	sources := make([]Source, 0)
	for rows.Next() {
		source, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}
	return sources, nil
}

func (s *Store) DeleteSource(ctx context.Context, projectID string, sourceID string) error {
	var references int
	if err := s.db.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM workspace_checkouts WHERE source_id = ?) +
			(SELECT COUNT(*) FROM workspace_folders WHERE source_id = ?)
	`, sourceID, sourceID).Scan(&references); err != nil {
		return fmt.Errorf("check source references: %w", err)
	}
	if references > 0 {
		return ErrSourceInUse
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM sources WHERE id = ? AND project_id = ?`, sourceID, projectID)
	if err != nil {
		return fmt.Errorf("delete source: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete source result: %w", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CreateWorkspace(ctx context.Context, projectID string, title string) (Workspace, error) {
	return s.CreateWorkspacePlan(ctx, projectID, title, nil)
}

func (s *Store) CreateWorkspacePlan(ctx context.Context, projectID string, title string, selections []WorkspaceSourceInput) (Workspace, error) {
	title = strings.TrimSpace(title)
	if title == "" || len(title) > 200 {
		return Workspace{}, ErrInvalid
	}
	id := "workspace_" + uuid.NewString()
	rootPath := filepath.Join(s.rootDir, "projects", projectID, "workspaces", id)
	now := time.Now().UTC()
	workspace := Workspace{
		ID:        id,
		ProjectID: projectID,
		Title:     title,
		RootPath:  rootPath,
		Status:    WorkspaceStatusProvisioning,
		CreatedAt: now,
		UpdatedAt: now,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Workspace{}, fmt.Errorf("begin workspace plan: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workspaces(
			id, project_id, title, root_path, owner_nonce, status, created_at, updated_at
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?)
	`, workspace.ID, workspace.ProjectID, workspace.Title, workspace.RootPath,
		uuid.NewString(), workspace.Status, unixMillis(now), unixMillis(now)); err != nil {
		if isConstraintError(err) {
			return Workspace{}, fmt.Errorf("%w: project missing or workspace path reserved", ErrConflict)
		}
		return Workspace{}, fmt.Errorf("create workspace: %w", err)
	}
	seen := make(map[string]struct{}, len(selections))
	for _, selection := range selections {
		if selection.SourceID == "" {
			return Workspace{}, ErrInvalid
		}
		if _, exists := seen[selection.SourceID]; exists {
			return Workspace{}, fmt.Errorf("%w: duplicate workspace source", ErrInvalid)
		}
		seen[selection.SourceID] = struct{}{}
		source, err := scanSource(tx.QueryRowContext(ctx, `
			SELECT id, project_id, kind, canonical_path, git_common_dir,
			       git_object_format, folder_mode, source_key, created_at, updated_at
			FROM sources WHERE id = ? AND project_id = ?
		`, selection.SourceID, projectID))
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return Workspace{}, fmt.Errorf("%w: workspace source not found", ErrInvalid)
			}
			return Workspace{}, err
		}
		switch source.Kind {
		case SourceKindGit:
			if selection.Mode != "" {
				return Workspace{}, ErrInvalid
			}
			baseRef := strings.TrimSpace(selection.BaseRef)
			if baseRef == "" {
				baseRef = "HEAD"
			}
			if err := verifySourceIdentity(ctx, source); err != nil {
				return Workspace{}, err
			}
			resolved, err := runGitMutation(ctx, source.CanonicalPath, "rev-parse", "--verify", baseRef+"^{commit}")
			if err != nil {
				return Workspace{}, fmt.Errorf("%w: base ref is unavailable", ErrConflict)
			}
			baseCommit := strings.TrimSpace(resolved)
			if selection.ExpectedCommit != "" && strings.TrimSpace(selection.ExpectedCommit) != baseCommit {
				return Workspace{}, fmt.Errorf("%w: expected base commit changed", ErrConflict)
			}
			branchName := selection.BranchName
			if branchName == "" {
				branchName = "zotigo/" + workspace.ID + "/" + source.SourceKey
			}
			target := filepath.Join(workspace.RootPath, "code", source.SourceKey)
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO workspace_checkouts(
					workspace_id, source_id, worktree_path, base_ref,
					base_commit, branch_name, owned_head, status
				) VALUES(?, ?, ?, ?, ?, ?, ?, 'planned')
			`, workspace.ID, source.ID, target, baseRef, baseCommit, branchName, baseCommit); err != nil {
				return Workspace{}, fmt.Errorf("plan workspace checkout: %w", err)
			}
		case SourceKindFolder:
			if selection.BaseRef != "" || selection.ExpectedCommit != "" || selection.BranchName != "" ||
				(selection.Mode != FolderModeDirect && selection.Mode != FolderModeReference && selection.Mode != FolderModeCopy) {
				return Workspace{}, ErrInvalid
			}
			parent := "code"
			if selection.Mode == FolderModeReference {
				parent = "notes"
			}
			target := filepath.Join(workspace.RootPath, parent, source.SourceKey)
			directPath := ""
			if selection.Mode == FolderModeDirect {
				directPath = source.CanonicalPath
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO workspace_folders(
					workspace_id, source_id, mode, target_path,
					direct_canonical_path, status
				) VALUES(?, ?, ?, ?, NULLIF(?, ''), 'planned')
			`, workspace.ID, source.ID, selection.Mode, target, directPath); err != nil {
				if isConstraintError(err) {
					return Workspace{}, fmt.Errorf("%w: folder source is already directly bound", ErrConflict)
				}
				return Workspace{}, fmt.Errorf("plan workspace folder: %w", err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return Workspace{}, fmt.Errorf("commit workspace plan: %w", err)
	}
	return workspace, nil
}

func (s *Store) GetWorkspace(ctx context.Context, id string) (Workspace, error) {
	return scanWorkspace(s.db.QueryRowContext(ctx, `
		SELECT id, project_id, title, root_path, status, error,
		       archived_at, deleted_at, created_at, updated_at
		FROM workspaces WHERE id = ? AND status != 'deleted'
	`, id))
}

func (s *Store) ListWorkspaces(ctx context.Context, projectID string, includeArchived bool) ([]Workspace, error) {
	query := `
		SELECT id, project_id, title, root_path, status, error,
		       archived_at, deleted_at, created_at, updated_at
		FROM workspaces WHERE project_id = ? AND status != 'deleted'`
	if !includeArchived {
		query += ` AND status NOT IN ('archiving', 'archived', 'deleting')`
	}
	query += ` ORDER BY updated_at DESC, id ASC`
	rows, err := s.db.QueryContext(ctx, query, projectID)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	defer func() { _ = rows.Close() }()

	workspaces := make([]Workspace, 0)
	for rows.Next() {
		workspace, err := scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		workspaces = append(workspaces, workspace)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	return workspaces, nil
}

func (s *Store) workspaceWithNonce(ctx context.Context, id string) (Workspace, string, error) {
	workspace, err := s.GetWorkspace(ctx, id)
	if err != nil {
		return Workspace{}, "", err
	}
	var nonce string
	if err := s.db.QueryRowContext(ctx, `SELECT owner_nonce FROM workspaces WHERE id = ?`, id).Scan(&nonce); err != nil {
		return Workspace{}, "", scanError("workspace owner", err)
	}
	return workspace, nonce, nil
}

func (s *Store) setWorkspaceStatus(ctx context.Context, id string, status WorkspaceStatus, errorText string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE workspaces
		SET status = ?, error = NULLIF(?, ''), updated_at = ?
		WHERE id = ? AND status != 'deleted'
	`, status, errorText, unixMillis(time.Now().UTC()), id)
	if err != nil {
		return fmt.Errorf("update workspace status: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update workspace status result: %w", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) workspaceBindings(ctx context.Context, workspaceID string) ([]Checkout, []FolderBinding, error) {
	checkoutRows, err := s.db.QueryContext(ctx, `
		SELECT workspace_id, source_id, worktree_path, base_ref, base_commit,
		       branch_name, owned_head, status, error
		FROM workspace_checkouts WHERE workspace_id = ? ORDER BY source_id
	`, workspaceID)
	if err != nil {
		return nil, nil, fmt.Errorf("list workspace checkouts: %w", err)
	}
	checkouts := make([]Checkout, 0)
	for checkoutRows.Next() {
		var checkout Checkout
		var errorText sql.NullString
		if err := checkoutRows.Scan(&checkout.WorkspaceID, &checkout.SourceID,
			&checkout.WorktreePath, &checkout.BaseRef, &checkout.BaseCommit,
			&checkout.BranchName, &checkout.OwnedHead, &checkout.Status, &errorText); err != nil {
			_ = checkoutRows.Close()
			return nil, nil, fmt.Errorf("scan workspace checkout: %w", err)
		}
		checkout.Error = errorText.String
		checkouts = append(checkouts, checkout)
	}
	if err := checkoutRows.Err(); err != nil {
		_ = checkoutRows.Close()
		return nil, nil, fmt.Errorf("list workspace checkouts: %w", err)
	}
	if err := checkoutRows.Close(); err != nil {
		return nil, nil, fmt.Errorf("close workspace checkouts: %w", err)
	}

	folderRows, err := s.db.QueryContext(ctx, `
		SELECT workspace_id, source_id, mode, target_path, status, error
		FROM workspace_folders WHERE workspace_id = ? ORDER BY source_id
	`, workspaceID)
	if err != nil {
		return nil, nil, fmt.Errorf("list workspace folders: %w", err)
	}
	defer func() { _ = folderRows.Close() }()
	folders := make([]FolderBinding, 0)
	for folderRows.Next() {
		var folder FolderBinding
		var errorText sql.NullString
		if err := folderRows.Scan(&folder.WorkspaceID, &folder.SourceID, &folder.Mode,
			&folder.TargetPath, &folder.Status, &errorText); err != nil {
			return nil, nil, fmt.Errorf("scan workspace folder: %w", err)
		}
		folder.Error = errorText.String
		folders = append(folders, folder)
	}
	if err := folderRows.Err(); err != nil {
		return nil, nil, fmt.Errorf("list workspace folders: %w", err)
	}
	return checkouts, folders, nil
}

func (s *Store) setCheckoutOwnedHead(ctx context.Context, workspaceID string, sourceID string, head string) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE workspace_checkouts SET owned_head = ? WHERE workspace_id = ? AND source_id = ?
	`, head, workspaceID, sourceID)
	if err != nil {
		return fmt.Errorf("update workspace checkout head: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) setCheckoutStatus(ctx context.Context, workspaceID string, sourceID string, status string, errorText string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workspace_checkouts SET status = ?, error = NULLIF(?, '')
		WHERE workspace_id = ? AND source_id = ?
	`, status, errorText, workspaceID, sourceID)
	if err != nil {
		return fmt.Errorf("update workspace checkout: %w", err)
	}
	return nil
}

func (s *Store) setFolderStatus(ctx context.Context, workspaceID string, sourceID string, status string, errorText string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE workspace_folders SET status = ?, error = NULLIF(?, '')
		WHERE workspace_id = ? AND source_id = ?
	`, status, errorText, workspaceID, sourceID)
	if err != nil {
		return fmt.Errorf("update workspace folder: %w", err)
	}
	return nil
}

func validateSourceInput(input SourceInput) error {
	if input.CanonicalPath == "" || !filepath.IsAbs(input.CanonicalPath) || input.SourceKey == "" {
		return ErrInvalid
	}
	switch input.Kind {
	case SourceKindGit:
		if input.GitCommonDir == "" || !filepath.IsAbs(input.GitCommonDir) ||
			(input.GitObjectFormat != "sha1" && input.GitObjectFormat != "sha256") || input.FolderMode != "" {
			return ErrInvalid
		}
	case SourceKindFolder:
		if input.GitCommonDir != "" || input.GitObjectFormat != "" ||
			(input.FolderMode != FolderModeDirect && input.FolderMode != FolderModeReference && input.FolderMode != FolderModeCopy) {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanProject(row scanner) (Project, error) {
	var project Project
	var createdAt, updatedAt int64
	if err := row.Scan(&project.ID, &project.Name, &createdAt, &updatedAt); err != nil {
		return Project{}, scanError("project", err)
	}
	project.CreatedAt = fromUnixMillis(createdAt)
	project.UpdatedAt = fromUnixMillis(updatedAt)
	return project, nil
}

func scanSource(row scanner) (Source, error) {
	var source Source
	var gitCommonDir, gitObjectFormat, folderMode sql.NullString
	var createdAt, updatedAt int64
	if err := row.Scan(&source.ID, &source.ProjectID, &source.Kind,
		&source.CanonicalPath, &gitCommonDir, &gitObjectFormat, &folderMode,
		&source.SourceKey, &createdAt, &updatedAt); err != nil {
		return Source{}, scanError("source", err)
	}
	source.GitCommonDir = gitCommonDir.String
	source.GitObjectFormat = gitObjectFormat.String
	source.FolderMode = FolderMode(folderMode.String)
	source.CreatedAt = fromUnixMillis(createdAt)
	source.UpdatedAt = fromUnixMillis(updatedAt)
	return source, nil
}

func scanWorkspace(row scanner) (Workspace, error) {
	var workspace Workspace
	var errorText sql.NullString
	var archivedAt, deletedAt sql.NullInt64
	var createdAt, updatedAt int64
	if err := row.Scan(&workspace.ID, &workspace.ProjectID, &workspace.Title,
		&workspace.RootPath, &workspace.Status, &errorText, &archivedAt, &deletedAt,
		&createdAt, &updatedAt); err != nil {
		return Workspace{}, scanError("workspace", err)
	}
	workspace.Error = errorText.String
	workspace.ArchivedAt = nullableTime(archivedAt)
	workspace.DeletedAt = nullableTime(deletedAt)
	workspace.CreatedAt = fromUnixMillis(createdAt)
	workspace.UpdatedAt = fromUnixMillis(updatedAt)
	return workspace, nil
}

func scanError(kind string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return fmt.Errorf("scan %s: %w", kind, err)
}

func unixMillis(value time.Time) int64 {
	return value.UnixMilli()
}

func fromUnixMillis(value int64) time.Time {
	return time.UnixMilli(value).UTC()
}

func nullableTime(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	timestamp := fromUnixMillis(value.Int64)
	return &timestamp
}

func isConstraintError(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "constraint failed")
}
