package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestProjectArchiveHidesProjectAndLeavesWorkspacesArchivedOnRestore(t *testing.T) {
	store, project, workspace, source := createFolderProjectFixture(t)
	ctx := context.Background()
	impact, err := store.PreviewProjectArchive(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(impact.WorkspaceIDs) != 1 || impact.WorkspaceIDs[0] != workspace.ID {
		t.Fatalf("archive impact = %+v", impact)
	}

	archived, err := store.ArchiveProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if archived.Status != ProjectStatusArchived || archived.ArchivedAt == nil {
		t.Fatalf("archived project = %+v", archived)
	}
	projects, err := store.ListProjects(ctx)
	if err != nil || len(projects) != 0 {
		t.Fatalf("active projects = %+v, err=%v", projects, err)
	}
	projects, err = store.ListAllProjects(ctx)
	if err != nil || len(projects) != 1 || projects[0].ID != project.ID {
		t.Fatalf("all projects = %+v, err=%v", projects, err)
	}
	if _, err := store.CreateWorkspace(ctx, project.ID, "Blocked"); !errors.Is(err, ErrConflict) {
		t.Fatalf("create workspace under archived project = %v, want conflict", err)
	}
	if _, err := store.AddSource(ctx, project.ID, SourceInput{
		Kind: SourceKindFolder, CanonicalPath: source.CanonicalPath,
		FolderMode: FolderModeReference, SourceKey: "blocked",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("add source under archived project = %v, want conflict", err)
	}
	if _, err := store.UnarchiveWorkspace(ctx, workspace.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("unarchive workspace under archived project = %v, want conflict", err)
	}

	restored, err := store.UnarchiveProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Status != ProjectStatusActive || restored.ArchivedAt != nil {
		t.Fatalf("restored project = %+v", restored)
	}
	stillArchived, err := store.GetWorkspace(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stillArchived.Status != WorkspaceStatusArchived {
		t.Fatalf("workspace status = %s, want archived", stillArchived.Status)
	}
	if _, err := store.UnarchiveWorkspace(ctx, workspace.ID); err != nil {
		t.Fatalf("restore workspace after project: %v", err)
	}
}

func TestProjectArchiveRejectsDirtyWorktreeBeforeChangingProjectStatus(t *testing.T) {
	store, workspace, source := createGitWorkspaceFixture(t)
	ctx := context.Background()
	project, err := store.GetProject(ctx, workspace.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(workspace.RootPath, "code", source.SourceKey)
	if err := os.WriteFile(filepath.Join(worktree, "dirty.txt"), []byte("dirty"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := store.ArchiveProject(ctx, project.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("archive error = %v, want conflict", err)
	}
	unchanged, err := store.GetProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Status != ProjectStatusActive {
		t.Fatalf("project status = %s, want active", unchanged.Status)
	}
	if _, err := os.Stat(worktree); err != nil {
		t.Fatalf("worktree changed after rejected archive: %v", err)
	}
}

func TestProjectDeleteRetriesAfterUnknownManagedDirectoryContent(t *testing.T) {
	store, project, workspace, source := createFolderProjectFixture(t)
	ctx := context.Background()
	projectDir := filepath.Join(store.RootDir(), "projects", project.ID)
	unknownPath := filepath.Join(projectDir, "unknown.txt")
	if err := os.WriteFile(unknownPath, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	impact, err := store.PreviewProjectDelete(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(impact.WorkspaceIDs) != 1 || impact.WorkspaceIDs[0] != workspace.ID ||
		!impact.PreservesSourceDirectories || !impact.PreservesSessions || !impact.PreservesRemoteRefs {
		t.Fatalf("delete impact = %+v", impact)
	}
	if err := store.DeleteProject(ctx, project.ID, "wrong"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("confirmation error = %v, want invalid", err)
	}
	if err := store.DeleteProject(ctx, project.ID, project.Name); !errors.Is(err, ErrConflict) {
		t.Fatalf("unknown content error = %v, want conflict", err)
	}
	deleting, err := store.GetProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if deleting.Status != ProjectStatusDeleting {
		t.Fatalf("project status = %s, want deleting", deleting.Status)
	}
	if _, err := store.GetWorkspace(ctx, workspace.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("workspace after partial delete = %v, want not found", err)
	}
	if _, err := os.Stat(source.CanonicalPath); err != nil {
		t.Fatalf("external source was removed: %v", err)
	}
	if _, err := store.CreateWorkspace(ctx, project.ID, "Blocked"); !errors.Is(err, ErrConflict) {
		t.Fatalf("create workspace under deleting project = %v, want conflict", err)
	}
	if err := os.Remove(unknownPath); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteProject(ctx, project.ID, project.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetProject(ctx, project.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted project lookup = %v, want not found", err)
	}
	for table, column := range map[string]string{"projects": "id", "sources": "project_id", "workspaces": "project_id"} {
		var count int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE `+column+` = ?`, project.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s rows = %d, want 0", table, count)
		}
	}
}

func TestProjectDeleteRejectsSymlinkedProjectsDirectory(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(context.Background(), "Symlink")
	if err != nil {
		t.Fatal(err)
	}
	externalProjects := t.TempDir()
	externalProject := filepath.Join(externalProjects, project.ID)
	if err := os.Mkdir(externalProject, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalProjects, filepath.Join(root, "projects")); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteProject(context.Background(), project.ID, project.Name); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete error = %v, want conflict", err)
	}
	if info, err := os.Stat(externalProject); err != nil || !info.IsDir() {
		t.Fatalf("external project directory was removed: %v", err)
	}
	deleting, err := store.GetProject(context.Background(), project.ID)
	if err != nil || deleting.Status != ProjectStatusDeleting {
		t.Fatalf("deleting project = %+v, err=%v", deleting, err)
	}
}

func TestProjectDeleteRejectsSymlinkedProjectDirectoryBeforeTouchingDescendants(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.CreateProject(context.Background(), "Symlink")
	if err != nil {
		t.Fatal(err)
	}
	projectsDir := filepath.Join(root, "projects")
	if err := os.Mkdir(projectsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	externalProject := t.TempDir()
	externalWorkspaces := filepath.Join(externalProject, "workspaces")
	if err := os.Mkdir(externalWorkspaces, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalProject, filepath.Join(projectsDir, project.ID)); err != nil {
		t.Fatal(err)
	}

	if err := store.DeleteProject(context.Background(), project.ID, project.Name); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete error = %v, want conflict", err)
	}
	if info, err := os.Stat(externalWorkspaces); err != nil || !info.IsDir() {
		t.Fatalf("external workspaces directory was removed: %v", err)
	}
}

func createFolderProjectFixture(t *testing.T) (*Store, Project, Workspace, Source) {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "Lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "notes.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	inspection, err := InspectSource(ctx, folder)
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.AddSource(ctx, project.ID, SourceInput{
		Kind: SourceKindFolder, CanonicalPath: inspection.CanonicalPath,
		FolderMode: FolderModeReference, SourceKey: inspection.SourceKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := store.CreateWorkspacePlan(ctx, project.ID, "Workspace", []WorkspaceSourceInput{{
		SourceID: source.ID, Mode: FolderModeReference,
	}})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = store.ProvisionWorkspace(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	return store, project, workspace, source
}
