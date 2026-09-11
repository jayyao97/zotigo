package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func (s *Store) PreviewArchive(ctx context.Context, workspaceID string) (ArchiveImpact, error) {
	workspace, err := s.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return ArchiveImpact{}, err
	}
	checkouts, _, err := s.workspaceBindings(ctx, workspaceID)
	if err != nil {
		return ArchiveImpact{}, err
	}
	sessionIDs, err := s.WorkspaceSessionIDs(ctx, workspaceID)
	if err != nil {
		return ArchiveImpact{}, err
	}
	impact := ArchiveImpact{WorkspaceID: workspace.ID, SessionIDs: sessionIDs, WorktreePaths: []string{}, DirtyWorktreePaths: []string{}, RetainedBranches: []string{}}
	for _, checkout := range checkouts {
		source, err := s.GetSource(ctx, workspace.ProjectID, checkout.SourceID)
		if err != nil {
			return ArchiveImpact{}, err
		}
		if err := verifySourceIdentity(ctx, source); err != nil {
			return ArchiveImpact{}, err
		}
		if err := verifyCheckoutOwnership(ctx, source, checkout, checkoutOwnershipRef(workspace.ID, source.SourceKey)); err != nil {
			return ArchiveImpact{}, err
		}
		branchName := checkout.BranchName
		if workspace.Status == WorkspaceStatusReady {
			branchName, err = currentCheckoutBranch(ctx, source, checkout)
			if err != nil {
				return ArchiveImpact{}, err
			}
		}
		impact.WorktreePaths = append(impact.WorktreePaths, checkout.WorktreePath)
		impact.RetainedBranches = append(impact.RetainedBranches, branchName)
		dirty, err := gitWorktreeDirty(ctx, checkout.WorktreePath)
		if err != nil {
			return ArchiveImpact{}, err
		}
		if dirty {
			impact.DirtyWorktreePaths = append(impact.DirtyWorktreePaths, checkout.WorktreePath)
		}
	}
	sort.Strings(impact.WorktreePaths)
	sort.Strings(impact.DirtyWorktreePaths)
	sort.Strings(impact.RetainedBranches)
	return impact, nil
}

func (s *Store) ArchiveWorkspace(ctx context.Context, workspaceID string) (Workspace, error) {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	workspace, err := s.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return Workspace{}, err
	}
	if workspace.Status == WorkspaceStatusArchived {
		return workspace, nil
	}
	if workspace.Status != WorkspaceStatusReady && workspace.Status != WorkspaceStatusArchiving {
		return Workspace{}, fmt.Errorf("%w: workspace cannot be archived from %s", ErrConflict, workspace.Status)
	}
	checkouts, _, err := s.workspaceBindings(ctx, workspaceID)
	if err != nil {
		return Workspace{}, err
	}
	if workspace.Status == WorkspaceStatusReady {
		for index, checkout := range checkouts {
			dirty, err := gitWorktreeDirty(ctx, checkout.WorktreePath)
			if err != nil {
				return Workspace{}, err
			}
			if dirty {
				return Workspace{}, fmt.Errorf("%w: worktree has uncommitted changes: %s", ErrConflict, checkout.WorktreePath)
			}
			source, err := s.GetSource(ctx, workspace.ProjectID, checkout.SourceID)
			if err != nil {
				return Workspace{}, err
			}
			if err := verifySourceIdentity(ctx, source); err != nil {
				return Workspace{}, err
			}
			if err := verifyCheckoutOwnership(ctx, source, checkout, checkoutOwnershipRef(workspace.ID, source.SourceKey)); err != nil {
				return Workspace{}, err
			}
			branchName, err := currentCheckoutBranch(ctx, source, checkout)
			if err != nil {
				return Workspace{}, err
			}
			checkout.BranchName = branchName
			head, err := checkoutBranchHead(ctx, source, checkout)
			if err != nil {
				return Workspace{}, err
			}
			if err := s.setCheckoutBranchAndOwnedHead(ctx, workspace.ID, checkout.SourceID, branchName, head); err != nil {
				return Workspace{}, err
			}
			checkouts[index].BranchName = branchName
			checkouts[index].OwnedHead = head
		}
		if err := s.setWorkspaceStatus(ctx, workspaceID, WorkspaceStatusArchiving, ""); err != nil {
			return Workspace{}, err
		}
	}
	for _, checkout := range checkouts {
		if checkout.Status == "archived" {
			continue
		}
		source, err := s.GetSource(ctx, workspace.ProjectID, checkout.SourceID)
		if err != nil {
			return Workspace{}, err
		}
		if err := verifySourceIdentity(ctx, source); err != nil {
			return Workspace{}, err
		}
		if err := verifyCheckoutOwnership(ctx, source, checkout, checkoutOwnershipRef(workspace.ID, source.SourceKey)); err != nil {
			return Workspace{}, err
		}
		if err := verifyCheckoutGeneration(ctx, source, checkout); err != nil {
			return Workspace{}, err
		}
		if err := removeCheckout(ctx, source, checkout, false); err != nil {
			return Workspace{}, err
		}
		if err := s.setCheckoutStatus(ctx, workspaceID, checkout.SourceID, "archived", ""); err != nil {
			return Workspace{}, err
		}
	}
	return s.finishArchive(ctx, workspaceID)
}

func (s *Store) UnarchiveWorkspace(ctx context.Context, workspaceID string) (Workspace, error) {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	workspace, err := s.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return Workspace{}, err
	}
	if err := s.requireActiveProject(ctx, workspace.ProjectID); err != nil {
		return Workspace{}, err
	}
	if workspace.Status == WorkspaceStatusReady {
		return workspace, nil
	}
	if workspace.Status != WorkspaceStatusArchived {
		return Workspace{}, fmt.Errorf("%w: workspace cannot be unarchived from %s", ErrConflict, workspace.Status)
	}
	checkouts, _, err := s.workspaceBindings(ctx, workspaceID)
	if err != nil {
		return Workspace{}, err
	}
	for _, checkout := range checkouts {
		if checkout.Status == "ready" {
			continue
		}
		source, err := s.GetSource(ctx, workspace.ProjectID, checkout.SourceID)
		if err != nil {
			return Workspace{}, err
		}
		if err := verifySourceIdentity(ctx, source); err != nil {
			return Workspace{}, err
		}
		if err := verifyCheckoutOwnership(ctx, source, checkout, checkoutOwnershipRef(workspace.ID, source.SourceKey)); err != nil {
			return Workspace{}, err
		}
		if err := verifyCheckoutGeneration(ctx, source, checkout); err != nil {
			return Workspace{}, err
		}
		if err := recreateCheckout(ctx, workspace, source, checkout); err != nil {
			return Workspace{}, err
		}
		if err := s.setCheckoutStatus(ctx, workspaceID, checkout.SourceID, "ready", ""); err != nil {
			return Workspace{}, err
		}
	}
	return s.finishUnarchive(ctx, workspaceID)
}

func (s *Store) PreviewDelete(ctx context.Context, workspaceID string) (DeleteImpact, error) {
	workspace, nonce, err := s.workspaceWithNonce(ctx, workspaceID)
	if err != nil {
		return DeleteImpact{}, err
	}
	checkouts, _, err := s.workspaceBindings(ctx, workspaceID)
	if err != nil {
		return DeleteImpact{}, err
	}
	if err := s.preflightDelete(ctx, workspace, nonce, checkouts); err != nil {
		return DeleteImpact{}, err
	}
	sessionIDs, err := s.WorkspaceSessionIDs(ctx, workspaceID)
	if err != nil {
		return DeleteImpact{}, err
	}
	impact := DeleteImpact{
		WorkspaceID:            workspace.ID,
		SessionIDs:             sessionIDs,
		WorkspaceRoot:          workspace.RootPath,
		WorktreePaths:          []string{},
		DirtyWorktreePaths:     []string{},
		LocalBranches:          []string{},
		PreservesSources:       true,
		PreservesSessions:      true,
		PreservesRemoteRefs:    true,
		PreservesLocalBranches: true,
	}
	for _, checkout := range checkouts {
		impact.WorktreePaths = append(impact.WorktreePaths, checkout.WorktreePath)
		dirty, err := gitWorktreeDirty(ctx, checkout.WorktreePath)
		if err != nil {
			return DeleteImpact{}, err
		}
		if dirty {
			impact.DirtyWorktreePaths = append(impact.DirtyWorktreePaths, checkout.WorktreePath)
		}
	}
	sort.Strings(impact.WorktreePaths)
	sort.Strings(impact.DirtyWorktreePaths)
	return impact, nil
}

func (s *Store) DeleteWorkspace(ctx context.Context, workspaceID string, confirmation string) error {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	workspace, nonce, err := s.workspaceWithNonce(ctx, workspaceID)
	if err != nil {
		return err
	}
	if confirmation != workspace.Title {
		return fmt.Errorf("%w: workspace title confirmation does not match", ErrInvalid)
	}
	if workspace.Status != WorkspaceStatusReady && workspace.Status != WorkspaceStatusError && workspace.Status != WorkspaceStatusArchived && workspace.Status != WorkspaceStatusDeleting {
		return fmt.Errorf("%w: workspace cannot be deleted from %s", ErrConflict, workspace.Status)
	}
	checkouts, _, err := s.workspaceBindings(ctx, workspaceID)
	if err != nil {
		return err
	}
	if err := s.preflightDelete(ctx, workspace, nonce, checkouts); err != nil {
		return err
	}
	if workspace.Status != WorkspaceStatusDeleting {
		if err := s.setWorkspaceStatus(ctx, workspaceID, WorkspaceStatusDeleting, ""); err != nil {
			return err
		}
	}
	for _, checkout := range checkouts {
		source, err := s.GetSource(ctx, workspace.ProjectID, checkout.SourceID)
		if err != nil {
			return err
		}
		if err := verifySourceIdentity(ctx, source); err != nil {
			return err
		}
		ownershipRef := checkoutOwnershipRef(workspace.ID, source.SourceKey)
		if err := verifyCheckoutRemoved(ctx, source, checkout); err == nil {
			// Older interrupted deletes may already have removed the ownership ref.
			if err := verifyCheckoutOwnership(ctx, source, checkout, ownershipRef); err != nil {
				continue
			}
		} else if err := verifyCheckoutOwnership(ctx, source, checkout, ownershipRef); err != nil {
			return err
		}
		if err := removeCheckout(ctx, source, checkout, true); err != nil {
			return err
		}
		if _, err := runGitMutation(ctx, source.CanonicalPath, "update-ref", "-d", ownershipRef, checkout.BaseCommit); err != nil {
			return fmt.Errorf("delete workspace ownership ref: %w", err)
		}
	}
	trash := filepath.Join(filepath.Dir(workspace.RootPath), ".trash-"+workspace.ID)
	if _, err := os.Lstat(workspace.RootPath); err == nil {
		if err := s.validateManagedWorkspacePath(ctx, workspace, workspace.RootPath); err != nil {
			return err
		}
		if err := validateOwnerMarker(workspace.RootPath, workspace.ProjectID, workspace.ID, nonce); err != nil {
			return err
		}
		if _, err := os.Lstat(trash); err == nil {
			return fmt.Errorf("%w: workspace trash target is occupied", ErrConflict)
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.Rename(workspace.RootPath, trash); err != nil {
			return fmt.Errorf("move workspace to trash: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Lstat(trash); err == nil {
		if err := s.validateManagedWorkspacePath(ctx, workspace, trash); err != nil {
			return err
		}
		if err := validateOwnerMarker(trash, workspace.ProjectID, workspace.ID, nonce); err != nil {
			return err
		}
		if err := os.RemoveAll(trash); err != nil {
			return fmt.Errorf("remove workspace trash: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return s.finishDelete(ctx, workspaceID)
}

// Validate every resource before deleting the first worktree. Branch names and
// heads are deliberately irrelevant: deletion never removes branch refs.
func (s *Store) preflightDelete(ctx context.Context, workspace Workspace, nonce string, checkouts []Checkout) error {
	if workspace.Status != WorkspaceStatusReady && workspace.Status != WorkspaceStatusError && workspace.Status != WorkspaceStatusArchived && workspace.Status != WorkspaceStatusDeleting {
		return fmt.Errorf("%w: workspace cannot be deleted from %s", ErrConflict, workspace.Status)
	}
	rootExists := false
	trash := filepath.Join(filepath.Dir(workspace.RootPath), ".trash-"+workspace.ID)
	for _, path := range []string{workspace.RootPath, trash} {
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if path == workspace.RootPath {
			rootExists = true
		} else if rootExists || workspace.Status != WorkspaceStatusDeleting {
			return fmt.Errorf("%w: workspace trash target is occupied", ErrConflict)
		}
		if err := s.validateManagedWorkspacePath(ctx, workspace, path); err != nil {
			return err
		}
		if err := validateOwnerMarker(path, workspace.ProjectID, workspace.ID, nonce); err != nil {
			return err
		}
	}
	if !rootExists && workspace.Status != WorkspaceStatusError && workspace.Status != WorkspaceStatusDeleting {
		return fmt.Errorf("%w: workspace root is missing", ErrConflict)
	}
	for _, checkout := range checkouts {
		// Even retry metadata must not point at an unrelated directory.
		parent := filepath.Dir(filepath.Clean(checkout.WorktreePath))
		if parent != filepath.Join(workspace.RootPath, "code") && parent != filepath.Join(workspace.RootPath, "notes") {
			return fmt.Errorf("%w: checkout is outside workspace", ErrConflict)
		}
		if rootExists {
			if err := s.validateWorkspaceBindingTarget(ctx, workspace, checkout.WorktreePath); err != nil {
				return err
			}
		}
		source, err := s.GetSource(ctx, workspace.ProjectID, checkout.SourceID)
		if err != nil {
			return err
		}
		if err := verifySourceIdentity(ctx, source); err != nil {
			return err
		}
		removed := verifyCheckoutRemoved(ctx, source, checkout) == nil
		if (workspace.Status == WorkspaceStatusError || workspace.Status == WorkspaceStatusDeleting) && removed {
			continue
		}
		if err := verifyCheckoutOwnership(ctx, source, checkout, checkoutOwnershipRef(workspace.ID, source.SourceKey)); err != nil {
			return err
		}
		if removed {
			continue
		}
		if !rootExists {
			return fmt.Errorf("%w: checkout remains after workspace root removal", ErrConflict)
		}
		info, err := os.Lstat(checkout.WorktreePath)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return fmt.Errorf("%w: checkout path is not a directory", ErrConflict)
		}
		if err == nil {
			commonDir, err := runGitMutation(ctx, checkout.WorktreePath, "rev-parse", "--path-format=absolute", "--git-common-dir")
			if err != nil || !samePath(strings.TrimSpace(commonDir), source.GitCommonDir) {
				return fmt.Errorf("%w: checkout repository identity changed", ErrConflict)
			}
		}
		worktrees, err := listGitWorktrees(ctx, source.CanonicalPath)
		if err != nil {
			return err
		}
		found := false
		for _, candidate := range worktrees {
			if samePath(candidate.Path, checkout.WorktreePath) {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("%w: checkout path is not the registered worktree", ErrConflict)
		}
	}
	return nil
}

func verifyCheckoutRemoved(ctx context.Context, source Source, checkout Checkout) error {
	worktrees, err := listGitWorktrees(ctx, source.CanonicalPath)
	if err != nil {
		return err
	}
	for _, candidate := range worktrees {
		if samePath(candidate.Path, checkout.WorktreePath) {
			return fmt.Errorf("%w: workspace checkout still exists", ErrConflict)
		}
	}
	if _, err := os.Lstat(checkout.WorktreePath); err == nil {
		return fmt.Errorf("%w: workspace checkout path still exists", ErrConflict)
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func checkoutBranchHead(ctx context.Context, source Source, checkout Checkout) (string, error) {
	branchRef := "refs/heads/" + checkout.BranchName
	head, err := runGitMutation(ctx, source.CanonicalPath, "rev-parse", "--verify", branchRef)
	if err != nil {
		return "", fmt.Errorf("%w: workspace branch is missing", ErrConflict)
	}
	return strings.TrimSpace(head), nil
}

func currentCheckoutBranch(ctx context.Context, source Source, checkout Checkout) (string, error) {
	worktrees, err := listGitWorktrees(ctx, source.CanonicalPath)
	if err != nil {
		return "", err
	}
	for _, candidate := range worktrees {
		if !samePath(candidate.Path, checkout.WorktreePath) {
			continue
		}
		const localBranchPrefix = "refs/heads/"
		if !strings.HasPrefix(candidate.Branch, localBranchPrefix) {
			return "", fmt.Errorf("%w: workspace worktree is not on a branch", ErrConflict)
		}
		return strings.TrimPrefix(candidate.Branch, localBranchPrefix), nil
	}
	return "", fmt.Errorf("%w: checkout path is not the registered worktree", ErrConflict)
}

func verifyCheckoutGeneration(ctx context.Context, source Source, checkout Checkout) error {
	head, err := checkoutBranchHead(ctx, source, checkout)
	if err != nil {
		return err
	}
	if head != checkout.OwnedHead {
		return fmt.Errorf("%w: workspace branch generation changed", ErrConflict)
	}
	return nil
}

func (s *Store) validateManagedWorkspacePath(ctx context.Context, workspace Workspace, candidate string) error {
	project, err := s.GetProject(ctx, workspace.ProjectID)
	if err != nil {
		return err
	}
	expectedRoot := filepath.Join(s.rootDir, "projects", project.storageName, "workspaces", workspace.storageName)
	expectedCandidate := expectedRoot
	if filepath.Base(candidate) == ".trash-"+workspace.ID {
		expectedCandidate = filepath.Join(filepath.Dir(expectedRoot), ".trash-"+workspace.ID)
	}
	absCandidate, err := filepath.Abs(candidate)
	if err != nil {
		return err
	}
	absExpected, err := filepath.Abs(expectedCandidate)
	if err != nil {
		return err
	}
	if filepath.Clean(absCandidate) != filepath.Clean(absExpected) {
		return fmt.Errorf("%w: workspace path is outside its managed location", ErrConflict)
	}
	paths := []string{
		filepath.Join(s.rootDir, "projects"),
		filepath.Join(s.rootDir, "projects", project.storageName),
		filepath.Join(s.rootDir, "projects", project.storageName, "workspaces"),
		candidate,
	}
	for _, path := range paths {
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("%w: inspect managed workspace path: %v", ErrConflict, err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: managed workspace path contains a symlink or non-directory", ErrConflict)
		}
	}
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(candidate))
	if err != nil {
		return fmt.Errorf("%w: resolve workspace parent: %v", ErrConflict, err)
	}
	resolvedCandidate, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return fmt.Errorf("%w: resolve workspace path: %v", ErrConflict, err)
	}
	if filepath.Dir(resolvedCandidate) != resolvedParent {
		return fmt.Errorf("%w: workspace path escapes its managed parent", ErrConflict)
	}
	return nil
}

func removeCheckout(ctx context.Context, source Source, checkout Checkout, force bool) error {
	worktrees, err := listGitWorktrees(ctx, source.CanonicalPath)
	if err != nil {
		return err
	}
	branchRef := "refs/heads/" + checkout.BranchName
	found := false
	for _, candidate := range worktrees {
		if samePath(candidate.Path, checkout.WorktreePath) {
			if !force && candidate.Branch != branchRef {
				return fmt.Errorf("%w: worktree branch does not match", ErrConflict)
			}
			found = true
		} else if !force && candidate.Branch == branchRef {
			return fmt.Errorf("%w: workspace branch is checked out elsewhere", ErrConflict)
		}
	}
	if !found {
		if _, err := os.Lstat(checkout.WorktreePath); err == nil {
			return fmt.Errorf("%w: checkout path is not the registered worktree", ErrConflict)
		} else if !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	_, _ = runGitMutation(ctx, source.CanonicalPath, "worktree", "unlock", checkout.WorktreePath)
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, checkout.WorktreePath)
	if _, err := runGitMutation(ctx, source.CanonicalPath, args...); err != nil {
		return fmt.Errorf("remove workspace worktree: %w", err)
	}
	return nil
}

func recreateCheckout(ctx context.Context, workspace Workspace, source Source, checkout Checkout) error {
	worktrees, err := listGitWorktrees(ctx, source.CanonicalPath)
	if err != nil {
		return err
	}
	branchRef := "refs/heads/" + checkout.BranchName
	for _, candidate := range worktrees {
		if samePath(candidate.Path, checkout.WorktreePath) && candidate.Branch == branchRef {
			return nil
		}
		if candidate.Branch == branchRef {
			return fmt.Errorf("%w: workspace branch is checked out elsewhere", ErrConflict)
		}
	}
	if _, err := os.Lstat(checkout.WorktreePath); err == nil {
		return fmt.Errorf("%w: workspace checkout path is occupied", ErrConflict)
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := runGitMutation(ctx, source.CanonicalPath, "rev-parse", "--verify", branchRef+"^{commit}"); err != nil {
		return fmt.Errorf("%w: archived workspace branch is missing", ErrConflict)
	}
	_, err = runGitMutation(ctx, source.CanonicalPath, "worktree", "add", "--lock", "--reason",
		"zotigo workspace "+workspace.ID, checkout.WorktreePath, checkout.BranchName)
	return err
}

func gitWorktreeDirty(ctx context.Context, path string) (bool, error) {
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	output, err := runGitMutation(ctx, path, "status", "--porcelain", "-z")
	if err != nil {
		return false, err
	}
	return output != "", nil
}

func (s *Store) finishArchive(ctx context.Context, workspaceID string) (Workspace, error) {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Workspace{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		UPDATE workspaces SET status = 'archived', archived_at = ?, error = NULL, updated_at = ? WHERE id = ?
	`, unixMillis(now), unixMillis(now), workspaceID); err != nil {
		return Workspace{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE session_organization
		SET workspace_archived_at = ?, pinned_at = NULL, pinned_position = NULL,
		    revision = revision + 1, updated_at = ?
		WHERE workspace_id = ?
	`, unixMillis(now), unixMillis(now), workspaceID); err != nil {
		return Workspace{}, err
	}
	if err := tx.Commit(); err != nil {
		return Workspace{}, err
	}
	return s.GetWorkspace(ctx, workspaceID)
}

func (s *Store) finishUnarchive(ctx context.Context, workspaceID string) (Workspace, error) {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Workspace{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		UPDATE workspaces SET status = 'ready', archived_at = NULL, error = NULL, updated_at = ? WHERE id = ?
	`, unixMillis(now), workspaceID); err != nil {
		return Workspace{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE session_organization
		SET workspace_archived_at = NULL, revision = revision + 1, updated_at = ?
		WHERE workspace_id = ?
	`, unixMillis(now), workspaceID); err != nil {
		return Workspace{}, err
	}
	if err := tx.Commit(); err != nil {
		return Workspace{}, err
	}
	return s.GetWorkspace(ctx, workspaceID)
}

func (s *Store) finishDelete(ctx context.Context, workspaceID string) error {
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range []string{
		`DELETE FROM session_organization WHERE workspace_id = ?`,
		`DELETE FROM workspace_checkouts WHERE workspace_id = ?`,
		`DELETE FROM workspace_folders WHERE workspace_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, statement, workspaceID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE workspaces SET status = 'deleted', deleted_at = ?, error = NULL, updated_at = ? WHERE id = ?
	`, unixMillis(now), unixMillis(now), workspaceID); err != nil {
		return err
	}
	return tx.Commit()
}
