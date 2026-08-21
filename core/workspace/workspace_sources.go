package workspace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type workspaceSourcePlanDB interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Store) planWorkspaceSource(ctx context.Context, db workspaceSourcePlanDB, workspace Workspace, selection WorkspaceSourceInput) error {
	if selection.SourceID == "" {
		return ErrInvalid
	}
	source, err := scanSource(db.QueryRowContext(ctx, `
		SELECT id, project_id, kind, canonical_path, git_common_dir,
		       git_object_format, folder_mode, source_key, created_at, updated_at
		FROM sources WHERE id = ? AND project_id = ?
	`, selection.SourceID, workspace.ProjectID))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: workspace source must belong to the project", ErrInvalid)
		}
		return err
	}
	switch source.Kind {
	case SourceKindGit:
		if selection.Mode != "" {
			return ErrInvalid
		}
		baseRef := strings.TrimSpace(selection.BaseRef)
		if baseRef == "" {
			baseRef = "HEAD"
		}
		if err := verifySourceIdentity(ctx, source); err != nil {
			return err
		}
		resolved, err := runGitMutation(ctx, source.CanonicalPath, "rev-parse", "--verify", baseRef+"^{commit}")
		if err != nil {
			return fmt.Errorf("%w: base ref is unavailable", ErrConflict)
		}
		baseCommit := strings.TrimSpace(resolved)
		if selection.ExpectedCommit != "" && strings.TrimSpace(selection.ExpectedCommit) != baseCommit {
			return fmt.Errorf("%w: expected base commit changed", ErrConflict)
		}
		branchName := strings.TrimSpace(selection.BranchName)
		if branchName == "" {
			branchName = "zotigo/" + workspace.ID + "/" + source.SourceKey
		}
		if _, err := runGitMutation(ctx, source.CanonicalPath, "check-ref-format", "--branch", branchName); err != nil {
			return fmt.Errorf("%w: invalid workspace branch", ErrInvalid)
		}
		target := filepath.Join(workspace.RootPath, "code", source.SourceKey)
		if _, err := db.ExecContext(ctx, `
			INSERT INTO workspace_checkouts(
				workspace_id, source_id, worktree_path, base_ref,
				base_commit, branch_name, owned_head, status
			) VALUES(?, ?, ?, ?, ?, ?, ?, 'planned')
		`, workspace.ID, source.ID, target, baseRef, baseCommit, branchName, baseCommit); err != nil {
			if isConstraintError(err) {
				return fmt.Errorf("%w: source is already bound to the workspace", ErrConflict)
			}
			return fmt.Errorf("plan workspace checkout: %w", err)
		}
	case SourceKindFolder:
		if selection.BaseRef != "" || selection.ExpectedCommit != "" || selection.BranchName != "" ||
			(selection.Mode != FolderModeDirect && selection.Mode != FolderModeReference && selection.Mode != FolderModeCopy) {
			return ErrInvalid
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
		if _, err := db.ExecContext(ctx, `
			INSERT INTO workspace_folders(
				workspace_id, source_id, mode, target_path,
				direct_canonical_path, status
			) VALUES(?, ?, ?, ?, NULLIF(?, ''), 'planned')
		`, workspace.ID, source.ID, selection.Mode, target, directPath); err != nil {
			if isConstraintError(err) {
				return fmt.Errorf("%w: source is already bound to a workspace", ErrConflict)
			}
			return fmt.Errorf("plan workspace folder: %w", err)
		}
	default:
		return ErrInvalid
	}
	return nil
}

func (s *Store) ListWorkspaceSources(ctx context.Context, workspaceID string) ([]WorkspaceSource, error) {
	workspace, err := s.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	checkouts, folders, err := s.workspaceBindings(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	items := make([]WorkspaceSource, 0, len(checkouts)+len(folders))
	for _, checkout := range checkouts {
		source, err := s.GetSource(ctx, workspace.ProjectID, checkout.SourceID)
		if err != nil {
			return nil, err
		}
		items = append(items, WorkspaceSource{
			Source: source, TargetPath: checkout.WorktreePath, WorktreePath: checkout.WorktreePath,
			BaseRef: checkout.BaseRef, BaseCommit: checkout.BaseCommit, BranchName: checkout.BranchName,
			Status: checkout.Status, Error: checkout.Error,
		})
	}
	for _, folder := range folders {
		source, err := s.GetSource(ctx, workspace.ProjectID, folder.SourceID)
		if err != nil {
			return nil, err
		}
		items = append(items, WorkspaceSource{
			Source: source, Mode: folder.Mode, TargetPath: folder.TargetPath,
			Status: folder.Status, Error: folder.Error,
		})
	}
	return items, nil
}

func (s *Store) AddWorkspaceSource(ctx context.Context, workspaceID string, selection WorkspaceSourceInput) (WorkspaceSource, error) {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()

	workspace, nonce, err := s.workspaceWithNonce(ctx, workspaceID)
	if err != nil {
		return WorkspaceSource{}, err
	}
	if workspace.Status != WorkspaceStatusReady {
		return WorkspaceSource{}, fmt.Errorf("%w: workspace cannot add sources from %s", ErrConflict, workspace.Status)
	}
	if err := s.requireActiveProject(ctx, workspace.ProjectID); err != nil {
		return WorkspaceSource{}, err
	}
	if err := validateOwnerMarker(workspace.RootPath, workspace.ProjectID, workspace.ID, nonce); err != nil {
		return WorkspaceSource{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkspaceSource{}, fmt.Errorf("begin workspace source plan: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.planWorkspaceSource(ctx, tx, workspace, selection); err != nil {
		return WorkspaceSource{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkspaceSource{}, fmt.Errorf("commit workspace source plan: %w", err)
	}

	checkouts, folders, err := s.workspaceBindings(ctx, workspace.ID)
	if err != nil {
		return WorkspaceSource{}, err
	}
	for _, checkout := range checkouts {
		if checkout.SourceID != selection.SourceID {
			continue
		}
		if err := s.provisionCheckout(ctx, workspace, checkout); err != nil {
			_ = s.setCheckoutStatus(ctx, workspace.ID, checkout.SourceID, "error", err.Error())
			if cleanupErr := s.cancelUnownedCheckout(ctx, workspace, checkout); cleanupErr != nil {
				return WorkspaceSource{}, errors.Join(err, cleanupErr)
			}
			return WorkspaceSource{}, err
		}
		if err := s.setCheckoutStatus(ctx, workspace.ID, checkout.SourceID, "ready", ""); err != nil {
			return WorkspaceSource{}, err
		}
		return s.workspaceSource(ctx, workspace, selection.SourceID)
	}
	for _, folder := range folders {
		if folder.SourceID != selection.SourceID {
			continue
		}
		if err := s.provisionFolder(ctx, workspace, folder); err != nil {
			_ = s.setFolderStatus(ctx, workspace.ID, folder.SourceID, "error", err.Error())
			if cleanupErr := s.cancelFailedFolderBinding(ctx, workspace, folder); cleanupErr != nil {
				return WorkspaceSource{}, errors.Join(err, cleanupErr)
			}
			return WorkspaceSource{}, err
		}
		if err := s.setFolderStatus(ctx, workspace.ID, folder.SourceID, "ready", ""); err != nil {
			return WorkspaceSource{}, err
		}
		return s.workspaceSource(ctx, workspace, selection.SourceID)
	}
	return WorkspaceSource{}, ErrNotFound
}

func (s *Store) cancelUnownedCheckout(ctx context.Context, workspace Workspace, checkout Checkout) error {
	source, err := s.GetSource(ctx, workspace.ProjectID, checkout.SourceID)
	if err != nil {
		return err
	}
	worktrees, err := listGitWorktrees(ctx, source.CanonicalPath)
	if err != nil {
		return err
	}
	for _, worktree := range worktrees {
		if samePath(worktree.Path, checkout.WorktreePath) {
			return nil
		}
	}
	ownershipRef := checkoutOwnershipRef(workspace.ID, source.SourceKey)
	ownership, err := runGitMutation(ctx, source.CanonicalPath, "for-each-ref", "--format=%(objectname)", ownershipRef)
	if err != nil {
		return fmt.Errorf("inspect failed workspace source ownership: %w", err)
	}
	if strings.TrimSpace(ownership) != "" {
		return nil
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM workspace_checkouts WHERE workspace_id = ? AND source_id = ?`, workspace.ID, checkout.SourceID)
	if err != nil {
		return fmt.Errorf("cancel failed workspace checkout: %w", err)
	}
	if count, err := result.RowsAffected(); err != nil {
		return err
	} else if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) cancelFailedFolderBinding(ctx context.Context, workspace Workspace, binding FolderBinding) error {
	if binding.Status == "ready" {
		return nil
	}
	if binding.Mode != FolderModeDirect {
		if _, err := os.Lstat(binding.TargetPath + ".staging"); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect failed workspace folder staging: %w", err)
		}
		if info, err := os.Lstat(binding.TargetPath); err == nil {
			if info.IsDir() && info.Mode()&os.ModeSymlink == 0 && validateBindingMarker(binding.TargetPath, workspace.ID, binding.SourceID) == nil {
				return nil
			}
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect failed workspace folder target: %w", err)
		}
	} else if info, err := os.Lstat(binding.TargetPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			source, sourceErr := s.GetSource(ctx, workspace.ProjectID, binding.SourceID)
			if sourceErr != nil {
				return sourceErr
			}
			link, linkErr := os.Readlink(binding.TargetPath)
			if linkErr != nil {
				return linkErr
			}
			if !filepath.IsAbs(link) {
				link = filepath.Join(filepath.Dir(binding.TargetPath), link)
			}
			if filepath.Clean(link) == filepath.Clean(source.CanonicalPath) {
				return nil
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect failed direct workspace folder target: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM workspace_folders WHERE workspace_id = ? AND source_id = ?`, workspace.ID, binding.SourceID)
	if err != nil {
		return fmt.Errorf("cancel failed workspace folder: %w", err)
	}
	if count, err := result.RowsAffected(); err != nil {
		return err
	} else if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) validateWorkspaceBindingTarget(workspace Workspace, target string) error {
	if err := s.validateManagedWorkspacePath(workspace, workspace.RootPath); err != nil {
		return err
	}
	absRoot, err := filepath.Abs(workspace.RootPath)
	if err != nil {
		return err
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	parent := filepath.Dir(absTarget)
	codeParent := filepath.Join(absRoot, "code")
	notesParent := filepath.Join(absRoot, "notes")
	if parent != codeParent && parent != notesParent {
		return fmt.Errorf("%w: workspace binding target is outside code or notes", ErrConflict)
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return fmt.Errorf("%w: inspect workspace binding parent: %v", ErrConflict, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: workspace binding parent is not a managed directory", ErrConflict)
	}
	resolvedRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return fmt.Errorf("%w: resolve workspace root: %v", ErrConflict, err)
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return fmt.Errorf("%w: resolve workspace binding parent: %v", ErrConflict, err)
	}
	if resolvedParent != filepath.Join(resolvedRoot, filepath.Base(parent)) {
		return fmt.Errorf("%w: workspace binding parent escapes its managed root", ErrConflict)
	}
	return nil
}

func (s *Store) workspaceSource(ctx context.Context, workspace Workspace, sourceID string) (WorkspaceSource, error) {
	items, err := s.ListWorkspaceSources(ctx, workspace.ID)
	if err != nil {
		return WorkspaceSource{}, err
	}
	for _, item := range items {
		if item.Source.ID == sourceID {
			return item, nil
		}
	}
	return WorkspaceSource{}, ErrNotFound
}
