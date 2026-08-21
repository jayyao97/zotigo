package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

func (s *Store) PreviewProjectArchive(ctx context.Context, projectID string) (ProjectArchiveImpact, error) {
	project, err := s.GetProject(ctx, projectID)
	if err != nil {
		return ProjectArchiveImpact{}, err
	}
	if project.Status == ProjectStatusDeleting {
		return ProjectArchiveImpact{}, fmt.Errorf("%w: project is deleting", ErrConflict)
	}
	workspaces, err := s.ListWorkspaces(ctx, projectID, true)
	if err != nil {
		return ProjectArchiveImpact{}, err
	}
	impact := ProjectArchiveImpact{
		ProjectID:          projectID,
		WorkspaceIDs:       []string{},
		SessionIDs:         []string{},
		WorktreePaths:      []string{},
		DirtyWorktreePaths: []string{},
		RetainedBranches:   []string{},
	}
	for _, workspace := range workspaces {
		if workspace.Status != WorkspaceStatusReady && workspace.Status != WorkspaceStatusArchiving && workspace.Status != WorkspaceStatusArchived {
			return ProjectArchiveImpact{}, fmt.Errorf("%w: workspace %s cannot be archived from %s", ErrConflict, workspace.ID, workspace.Status)
		}
		workspaceImpact, err := s.PreviewArchive(ctx, workspace.ID)
		if err != nil {
			return ProjectArchiveImpact{}, err
		}
		impact.WorkspaceIDs = append(impact.WorkspaceIDs, workspace.ID)
		impact.SessionIDs = append(impact.SessionIDs, workspaceImpact.SessionIDs...)
		impact.WorktreePaths = append(impact.WorktreePaths, workspaceImpact.WorktreePaths...)
		impact.DirtyWorktreePaths = append(impact.DirtyWorktreePaths, workspaceImpact.DirtyWorktreePaths...)
		impact.RetainedBranches = append(impact.RetainedBranches, workspaceImpact.RetainedBranches...)
	}
	sortProjectArchiveImpact(&impact)
	return impact, nil
}

func (s *Store) ArchiveProject(ctx context.Context, projectID string) (Project, error) {
	project, err := s.GetProject(ctx, projectID)
	if err != nil {
		return Project{}, err
	}
	if project.Status == ProjectStatusArchived {
		return project, nil
	}
	if project.Status != ProjectStatusActive && project.Status != ProjectStatusArchiving {
		return Project{}, fmt.Errorf("%w: project cannot be archived from %s", ErrConflict, project.Status)
	}
	if project.Status == ProjectStatusActive {
		impact, err := s.PreviewProjectArchive(ctx, projectID)
		if err != nil {
			return Project{}, err
		}
		if len(impact.DirtyWorktreePaths) > 0 {
			return Project{}, fmt.Errorf("%w: project contains dirty worktrees", ErrConflict)
		}
		if err := s.setProjectStatus(ctx, projectID, ProjectStatusArchiving, nil); err != nil {
			return Project{}, err
		}
	}
	workspaces, err := s.ListWorkspaces(ctx, projectID, true)
	if err != nil {
		return Project{}, err
	}
	for _, workspace := range workspaces {
		if workspace.Status == WorkspaceStatusArchived {
			continue
		}
		if _, err := s.ArchiveWorkspace(ctx, workspace.ID); err != nil {
			return Project{}, err
		}
	}
	now := time.Now().UTC()
	if err := s.setProjectStatus(ctx, projectID, ProjectStatusArchived, &now); err != nil {
		return Project{}, err
	}
	return s.GetProject(ctx, projectID)
}

func (s *Store) UnarchiveProject(ctx context.Context, projectID string) (Project, error) {
	project, err := s.GetProject(ctx, projectID)
	if err != nil {
		return Project{}, err
	}
	if project.Status == ProjectStatusActive {
		return project, nil
	}
	if project.Status != ProjectStatusArchived {
		return Project{}, fmt.Errorf("%w: project cannot be unarchived from %s", ErrConflict, project.Status)
	}
	if err := s.setProjectStatus(ctx, projectID, ProjectStatusActive, nil); err != nil {
		return Project{}, err
	}
	return s.GetProject(ctx, projectID)
}

func (s *Store) PreviewProjectDelete(ctx context.Context, projectID string) (ProjectDeleteImpact, error) {
	project, err := s.GetProject(ctx, projectID)
	if err != nil {
		return ProjectDeleteImpact{}, err
	}
	if project.Status == ProjectStatusArchiving {
		return ProjectDeleteImpact{}, fmt.Errorf("%w: project is archiving", ErrConflict)
	}
	workspaces, err := s.ListWorkspaces(ctx, projectID, true)
	if err != nil {
		return ProjectDeleteImpact{}, err
	}
	impact := ProjectDeleteImpact{
		ProjectID:                  projectID,
		WorkspaceIDs:               []string{},
		SessionIDs:                 []string{},
		WorkspaceRoots:             []string{},
		WorktreePaths:              []string{},
		DirtyWorktreePaths:         []string{},
		LocalBranches:              []string{},
		PreservesSourceDirectories: true,
		PreservesSessions:          true,
		PreservesRemoteRefs:        true,
	}
	for _, workspace := range workspaces {
		if workspace.Status != WorkspaceStatusReady && workspace.Status != WorkspaceStatusArchived && workspace.Status != WorkspaceStatusDeleting {
			return ProjectDeleteImpact{}, fmt.Errorf("%w: workspace %s cannot be deleted from %s", ErrConflict, workspace.ID, workspace.Status)
		}
		workspaceImpact, err := s.PreviewDelete(ctx, workspace.ID)
		if err != nil {
			return ProjectDeleteImpact{}, err
		}
		impact.WorkspaceIDs = append(impact.WorkspaceIDs, workspace.ID)
		impact.SessionIDs = append(impact.SessionIDs, workspaceImpact.SessionIDs...)
		impact.WorkspaceRoots = append(impact.WorkspaceRoots, workspaceImpact.WorkspaceRoot)
		impact.WorktreePaths = append(impact.WorktreePaths, workspaceImpact.WorktreePaths...)
		impact.DirtyWorktreePaths = append(impact.DirtyWorktreePaths, workspaceImpact.DirtyWorktreePaths...)
		impact.LocalBranches = append(impact.LocalBranches, workspaceImpact.LocalBranches...)
	}
	sort.Strings(impact.WorkspaceIDs)
	sort.Strings(impact.SessionIDs)
	sort.Strings(impact.WorkspaceRoots)
	sort.Strings(impact.WorktreePaths)
	sort.Strings(impact.DirtyWorktreePaths)
	sort.Strings(impact.LocalBranches)
	return impact, nil
}

func (s *Store) DeleteProject(ctx context.Context, projectID string, confirmation string) error {
	project, err := s.GetProject(ctx, projectID)
	if err != nil {
		return err
	}
	if confirmation != project.Name {
		return fmt.Errorf("%w: project name confirmation does not match", ErrInvalid)
	}
	if project.Status != ProjectStatusActive && project.Status != ProjectStatusArchived && project.Status != ProjectStatusDeleting {
		return fmt.Errorf("%w: project cannot be deleted from %s", ErrConflict, project.Status)
	}
	if project.Status != ProjectStatusDeleting {
		if _, err := s.PreviewProjectDelete(ctx, projectID); err != nil {
			return err
		}
		if err := s.setProjectStatus(ctx, projectID, ProjectStatusDeleting, nil); err != nil {
			return err
		}
	}
	workspaces, err := s.ListWorkspaces(ctx, projectID, true)
	if err != nil {
		return err
	}
	for _, workspace := range workspaces {
		if err := s.DeleteWorkspace(ctx, workspace.ID, workspace.Title); err != nil {
			return err
		}
	}
	if err := s.removeEmptyProjectDirectories(projectID); err != nil {
		return err
	}
	return s.finishProjectDelete(ctx, projectID)
}

func (s *Store) setProjectStatus(ctx context.Context, projectID string, status ProjectStatus, archivedAt *time.Time) error {
	now := time.Now().UTC()
	var archivedMillis any
	if archivedAt != nil {
		archivedMillis = unixMillis(*archivedAt)
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE projects SET status = ?, archived_at = ?, updated_at = ? WHERE id = ?
	`, status, archivedMillis, unixMillis(now), projectID)
	if err != nil {
		return fmt.Errorf("update project status: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("update project status result: %w", err)
	}
	if count == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) removeEmptyProjectDirectories(projectID string) error {
	projectsDir := filepath.Join(s.rootDir, "projects")
	projectDir := filepath.Join(projectsDir, projectID)
	projectsInfo, err := os.Lstat(projectsDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !projectsInfo.IsDir() || projectsInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: projects managed path is not a directory", ErrConflict)
	}
	projectInfo, err := os.Lstat(projectDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !projectInfo.IsDir() || projectInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: project managed path is not a directory", ErrConflict)
	}
	workspacesDir := filepath.Join(projectDir, "workspaces")
	workspacesInfo, err := os.Lstat(workspacesDir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		if !workspacesInfo.IsDir() || workspacesInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: project managed path is not a directory", ErrConflict)
		}
		if err := os.Remove(workspacesDir); err != nil {
			return fmt.Errorf("%w: remove project managed directory %s: %v", ErrConflict, workspacesDir, err)
		}
	}
	if err := os.Remove(projectDir); err != nil {
		return fmt.Errorf("%w: remove project managed directory %s: %v", ErrConflict, projectDir, err)
	}
	return nil
}

func (s *Store) finishProjectDelete(ctx context.Context, projectID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, statement := range []string{
		`DELETE FROM session_organization WHERE project_id = ?`,
		`DELETE FROM workspace_checkouts WHERE workspace_id IN (SELECT id FROM workspaces WHERE project_id = ?)`,
		`DELETE FROM workspace_folders WHERE workspace_id IN (SELECT id FROM workspaces WHERE project_id = ?)`,
		`DELETE FROM workspaces WHERE project_id = ?`,
		`DELETE FROM sources WHERE project_id = ?`,
		`DELETE FROM projects WHERE id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, statement, projectID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func sortProjectArchiveImpact(impact *ProjectArchiveImpact) {
	sort.Strings(impact.WorkspaceIDs)
	sort.Strings(impact.SessionIDs)
	sort.Strings(impact.WorktreePaths)
	sort.Strings(impact.DirtyWorktreePaths)
	sort.Strings(impact.RetainedBranches)
}
