package workspace

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func TestRenameProject(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "Original")
	if err != nil {
		t.Fatal(err)
	}

	renamed, err := store.RenameProject(ctx, project.ID, "  Renamed  ")
	if err != nil {
		t.Fatal(err)
	}
	if renamed.ID != project.ID || renamed.Name != "Renamed" {
		t.Fatalf("renamed project = %+v", renamed)
	}
	if _, err := store.RenameProject(ctx, project.ID, "  "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("blank name error = %v", err)
	}
	if _, err := store.RenameProject(ctx, "missing", "Renamed"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing project error = %v", err)
	}
	got, err := store.GetProject(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Renamed" {
		t.Fatalf("stored project name = %q", got.Name)
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
	if _, err := store.db.Exec(`UPDATE schema_meta SET version = ? WHERE singleton = 1`, schemaVersion+1); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root); err == nil {
		t.Fatal("opening a newer catalog version succeeded")
	}
}

func TestMigratesV1ProjectLifecycleSchema(t *testing.T) {
	root := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(root, "catalog.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE schema_meta (singleton INTEGER PRIMARY KEY CHECK(singleton = 1), version INTEGER NOT NULL CHECK(version > 0))`,
		`INSERT INTO schema_meta(singleton, version) VALUES(1, 1)`,
		`CREATE TABLE projects (id TEXT PRIMARY KEY, name TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		`INSERT INTO projects(id, name, created_at, updated_at) VALUES('project_v1', 'Existing', 0, 0)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	project, err := store.GetProject(context.Background(), "project_v1")
	if err != nil {
		t.Fatal(err)
	}
	if project.Status != ProjectStatusActive || project.ArchivedAt != nil {
		t.Fatalf("migrated project = %+v", project)
	}
	if _, err := store.db.Exec(`UPDATE projects SET status = 'invalid' WHERE id = 'project_v1'`); err == nil {
		t.Fatal("migrated schema accepted an invalid project status")
	}
}

func TestMigratesV2CheckoutOwnershipSchema(t *testing.T) {
	root := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(root, "catalog.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE schema_meta (singleton INTEGER PRIMARY KEY CHECK(singleton = 1), version INTEGER NOT NULL CHECK(version > 0))`,
		`INSERT INTO schema_meta(singleton, version) VALUES(1, 2)`,
		`CREATE TABLE projects (
			id TEXT PRIMARY KEY, name TEXT NOT NULL, status TEXT NOT NULL,
			archived_at INTEGER, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE sources (
			id TEXT PRIMARY KEY, project_id TEXT NOT NULL, kind TEXT NOT NULL,
			canonical_path TEXT NOT NULL, git_common_dir TEXT, git_object_format TEXT,
			folder_mode TEXT, source_key TEXT NOT NULL, created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL, UNIQUE(project_id, canonical_path),
			UNIQUE(project_id, source_key)
		)`,
		`CREATE TABLE workspaces (
			id TEXT PRIMARY KEY, project_id TEXT NOT NULL, title TEXT NOT NULL,
			root_path TEXT NOT NULL UNIQUE, owner_nonce TEXT NOT NULL, status TEXT NOT NULL,
			error TEXT, archived_at INTEGER, deleted_at INTEGER, created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE workspace_checkouts (
			workspace_id TEXT NOT NULL, source_id TEXT NOT NULL, worktree_path TEXT NOT NULL UNIQUE,
			base_ref TEXT NOT NULL, base_commit TEXT NOT NULL, branch_name TEXT NOT NULL,
			status TEXT NOT NULL, error TEXT, PRIMARY KEY(workspace_id, source_id)
		)`,
		`INSERT INTO projects(id, name, status, created_at, updated_at)
			VALUES('project_v2', 'Existing', 'active', 0, 0)`,
		`INSERT INTO sources(
			id, project_id, kind, canonical_path, git_common_dir, git_object_format,
			source_key, created_at, updated_at
		) VALUES('source_v2', 'project_v2', 'git', '/tmp/existing', '/tmp/existing/.git', 'sha1', 'existing', 0, 0)`,
		`INSERT INTO workspaces(
			id, project_id, title, root_path, owner_nonce, status, created_at, updated_at
		) VALUES('workspace_v2', 'project_v2', 'Existing', '/tmp/workspace', 'nonce', 'ready', 0, 0)`,
		`INSERT INTO workspace_checkouts(
			workspace_id, source_id, worktree_path, base_ref, base_commit, branch_name, status
		) VALUES('workspace_v2', 'source_v2', '/tmp/worktree', 'main', 'abc123', 'zotigo/existing', 'ready')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var version int
	var ownedHead string
	if err := store.db.QueryRow(`SELECT version FROM schema_meta WHERE singleton = 1`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("schema version = %d, want %d", version, schemaVersion)
	}
	if err := store.db.QueryRow(`
		SELECT owned_head FROM workspace_checkouts
		WHERE workspace_id = 'workspace_v2' AND source_id = 'source_v2'
	`).Scan(&ownedHead); err != nil {
		t.Fatal(err)
	}
	if ownedHead != "abc123" {
		t.Fatalf("owned_head = %q, want base commit", ownedHead)
	}

	repository := t.TempDir()
	runGitProvisionCommand(t, repository, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitProvisionCommand(t, repository, "add", "README.md")
	runGitProvisionCommand(t, repository, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
	inspection, err := InspectSource(context.Background(), repository)
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(context.Background(), "Migrated")
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.AddSource(context.Background(), project.ID, SourceInput{
		Kind:            SourceKindGit,
		CanonicalPath:   inspection.CanonicalPath,
		GitCommonDir:    inspection.GitCommonDir,
		GitObjectFormat: inspection.GitObjectFormat,
		SourceKey:       inspection.SourceKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := store.CreateWorkspacePlan(context.Background(), project.ID, "After migration", []WorkspaceSourceInput{{
		SourceID:   source.ID,
		BaseRef:    "HEAD",
		BranchName: "zotigo/after-migration",
	}})
	if err != nil {
		t.Fatal(err)
	}
	checkout, _, err := store.workspaceBindings(context.Background(), workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(checkout) != 1 || checkout[0].OwnedHead == "" || checkout[0].OwnedHead != strings.TrimSpace(checkout[0].BaseCommit) {
		t.Fatalf("checkout after migration = %+v", checkout)
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
