package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	zotigosession "github.com/jayyao97/zotigo/core/session"
)

func TestStorePersistsCatalogIndependentlyFromSessionIndex(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, "Zotigo")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	sessionStore, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := sessionStore.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "session_index.sqlite")); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	got, err := reopened.GetProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != project.Name {
		t.Fatalf("project name = %q, want %q", got.Name, project.Name)
	}
}

func TestProjectSourceAndWorkspaceCRUD(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()

	project, err := store.CreateProject(ctx, "  My Project  ")
	if err != nil {
		t.Fatal(err)
	}
	if project.Name != "My Project" {
		t.Fatalf("project name = %q", project.Name)
	}

	gitPath := filepath.Join(t.TempDir(), "repo")
	gitCommonDir := filepath.Join(gitPath, ".git")
	gitSource, err := store.AddSource(ctx, project.ID, SourceInput{
		Kind:            SourceKindGit,
		CanonicalPath:   gitPath,
		GitCommonDir:    gitCommonDir,
		GitObjectFormat: "sha1",
		SourceKey:       "repo-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	folderPath := filepath.Join(t.TempDir(), "notes")
	_, err = store.AddSource(ctx, project.ID, SourceInput{
		Kind:          SourceKindFolder,
		CanonicalPath: folderPath,
		FolderMode:    FolderModeReference,
		SourceKey:     "notes-key",
	})
	if err != nil {
		t.Fatal(err)
	}

	sources, err := store.ListSources(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 {
		t.Fatalf("source count = %d, want 2", len(sources))
	}
	gotGit, err := store.GetSource(ctx, project.ID, gitSource.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotGit.GitCommonDir != gitCommonDir || gotGit.FolderMode != "" {
		t.Fatalf("unexpected git source: %+v", gotGit)
	}

	workspace, err := store.CreateWorkspace(ctx, project.ID, "  Implement catalog  ")
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Status != WorkspaceStatusProvisioning || workspace.Title != "Implement catalog" {
		t.Fatalf("unexpected workspace: %+v", workspace)
	}
	wantRoot := filepath.Join(store.RootDir(), "projects", project.ID, "workspaces", workspace.ID)
	if workspace.RootPath != wantRoot {
		t.Fatalf("workspace root = %q, want %q", workspace.RootPath, wantRoot)
	}
	listed, err := store.ListWorkspaces(ctx, project.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != workspace.ID {
		t.Fatalf("workspaces = %+v", listed)
	}
}

func TestSourceConstraintsAndReferences(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "One")
	if err != nil {
		t.Fatal(err)
	}
	otherProject, err := store.CreateProject(ctx, "Two")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "repo")
	commonDir := filepath.Join(path, ".git")
	input := SourceInput{
		Kind:            SourceKindGit,
		CanonicalPath:   path,
		GitCommonDir:    commonDir,
		GitObjectFormat: "sha1",
		SourceKey:       "repo",
	}
	source, err := store.AddSource(ctx, project.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddSource(ctx, project.ID, input); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate source error = %v, want conflict", err)
	}
	if _, err := store.AddSource(ctx, otherProject.ID, input); err != nil {
		t.Fatalf("same source in another project: %v", err)
	}
	if _, err := store.AddSource(ctx, project.ID, SourceInput{
		Kind:          SourceKindGit,
		CanonicalPath: filepath.Join(t.TempDir(), "bad"),
		SourceKey:     "bad",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid source error = %v, want invalid", err)
	}

	workspace, err := store.CreateWorkspace(ctx, project.ID, "Uses source")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.db.ExecContext(ctx, `
		INSERT INTO workspace_checkouts(
			workspace_id, source_id, worktree_path, base_ref, base_commit,
			branch_name, owned_head, status
		) VALUES(?, ?, ?, 'main', 'abc', 'zotigo/test', 'abc', 'planned')
	`, workspace.ID, source.ID, filepath.Join(workspace.RootPath, "code", source.SourceKey))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSource(ctx, project.ID, source.ID); !errors.Is(err, ErrSourceInUse) {
		t.Fatalf("delete referenced source error = %v, want source in use", err)
	}
}

func TestCatalogPermissionsAndUnknownVersion(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(root, "catalog.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("catalog mode = %o, want 600", info.Mode().Perm())
		}
	}
	if _, err := store.db.Exec(`UPDATE schema_meta SET version = 2 WHERE singleton = 1`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root); err == nil {
		t.Fatal("opening a newer catalog version succeeded")
	}
}

func TestOpenReadOnlyDoesNotPermitCatalogWrites(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateProject(context.Background(), "Existing"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := OpenReadOnly(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = readOnly.Close() })
	projects, err := readOnly.ListProjects(context.Background())
	if err != nil || len(projects) != 1 {
		t.Fatalf("read-only projects = %+v, err=%v", projects, err)
	}
	if _, err := readOnly.CreateProject(context.Background(), "Rejected"); err == nil {
		t.Fatal("read-only catalog accepted a write")
	}
}
