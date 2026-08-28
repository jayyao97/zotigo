package workspace

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestProvisionWorkspaceFolderBindings(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "Folders")
	if err != nil {
		t.Fatal(err)
	}

	type fixture struct {
		mode   FolderMode
		source Source
	}
	fixtures := make([]fixture, 0, 3)
	for _, mode := range []FolderMode{FolderModeDirect, FolderModeCopy, FolderModeReference} {
		path := t.TempDir()
		if err := os.WriteFile(filepath.Join(path, "content.txt"), []byte(string(mode)), 0o666); err != nil {
			t.Fatal(err)
		}
		inspection, err := InspectSource(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		source, err := store.AddSource(ctx, project.ID, SourceInput{
			Kind:          SourceKindFolder,
			CanonicalPath: inspection.CanonicalPath,
			FolderMode:    mode,
			SourceKey:     inspection.SourceKey,
		})
		if err != nil {
			t.Fatal(err)
		}
		fixtures = append(fixtures, fixture{mode: mode, source: source})
	}
	selections := make([]WorkspaceSourceInput, 0, len(fixtures))
	for _, fixture := range fixtures {
		selections = append(selections, WorkspaceSourceInput{SourceID: fixture.source.ID, Mode: fixture.mode})
	}
	workspace, err := store.CreateWorkspacePlan(ctx, project.ID, "Folder modes", selections)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = store.ProvisionWorkspace(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Status != WorkspaceStatusReady {
		t.Fatalf("workspace status = %q", workspace.Status)
	}
	for _, fixture := range fixtures {
		parent := "code"
		if fixture.mode == FolderModeReference {
			parent = "notes"
		}
		target := filepath.Join(workspace.RootPath, parent, workspaceSourceName(fixture.source))
		info, err := os.Lstat(target)
		if err != nil {
			t.Fatal(err)
		}
		if fixture.mode == FolderModeDirect {
			if info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("direct target mode = %v", info.Mode())
			}
			continue
		}
		contentInfo, err := os.Stat(filepath.Join(target, "content.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if fixture.mode == FolderModeReference && contentInfo.Mode().Perm()&0o222 != 0 {
			t.Fatalf("reference file mode = %o, want read-only", contentInfo.Mode().Perm())
		}
	}
	if _, err := store.ProvisionWorkspace(ctx, workspace.ID); err != nil {
		t.Fatalf("idempotent provision: %v", err)
	}
}

func TestProvisionRejectsAbsoluteSymlinkInCopiedBindings(t *testing.T) {
	for _, mode := range []FolderMode{FolderModeCopy, FolderModeReference} {
		t.Run(string(mode), func(t *testing.T) {
			store, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			ctx := context.Background()
			project, err := store.CreateProject(ctx, "Symlink")
			if err != nil {
				t.Fatal(err)
			}
			sourcePath := t.TempDir()
			filePath := filepath.Join(sourcePath, "content.txt")
			if err := os.WriteFile(filePath, []byte("source"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filePath, filepath.Join(sourcePath, "absolute-link")); err != nil {
				t.Fatal(err)
			}
			inspection, err := InspectSource(ctx, sourcePath)
			if err != nil {
				t.Fatal(err)
			}
			source, err := store.AddSource(ctx, project.ID, SourceInput{Kind: SourceKindFolder, CanonicalPath: inspection.CanonicalPath, FolderMode: mode, SourceKey: inspection.SourceKey})
			if err != nil {
				t.Fatal(err)
			}
			workspace, err := store.CreateWorkspacePlan(ctx, project.ID, "Reject symlink", []WorkspaceSourceInput{{SourceID: source.ID, Mode: mode}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.ProvisionWorkspace(ctx, workspace.ID); !errors.Is(err, ErrConflict) {
				t.Fatalf("provision error = %v, want conflict", err)
			}
		})
	}
}

func TestProvisionWorkspaceGitWorktree(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "Git")
	if err != nil {
		t.Fatal(err)
	}
	repository := t.TempDir()
	runGitProvisionCommand(t, repository, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitProvisionCommand(t, repository, "add", "README.md")
	runGitProvisionCommand(t, repository, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
	baseCommit := strings.TrimSpace(runGitProvisionCommand(t, repository, "rev-parse", "HEAD"))
	inspection, err := InspectSource(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.AddSource(ctx, project.ID, SourceInput{
		Kind:            SourceKindGit,
		CanonicalPath:   inspection.CanonicalPath,
		GitCommonDir:    inspection.GitCommonDir,
		GitObjectFormat: inspection.GitObjectFormat,
		SourceKey:       inspection.SourceKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := store.CreateWorkspacePlan(ctx, project.ID, "Git worktree", []WorkspaceSourceInput{{
		SourceID:       source.ID,
		BaseRef:        "main",
		ExpectedCommit: baseCommit,
		BranchName:     "zotigo/test-workspace",
	}})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = store.ProvisionWorkspace(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(workspace.RootPath, "code", workspaceSourceName(source))
	if data, err := os.ReadFile(filepath.Join(worktree, "README.md")); err != nil || string(data) != "source\n" {
		t.Fatalf("worktree content = %q, err=%v", data, err)
	}
	if branch := strings.TrimSpace(runGitProvisionCommand(t, worktree, "branch", "--show-current")); branch != "zotigo/test-workspace" {
		t.Fatalf("worktree branch = %q", branch)
	}
	if branch := strings.TrimSpace(runGitProvisionCommand(t, repository, "branch", "--show-current")); branch != "main" {
		t.Fatalf("source branch = %q", branch)
	}
}

func TestProvisionWorkspaceUsesSemanticStorageNames(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "示例 / Project")
	if err != nil {
		t.Fatal(err)
	}
	repository := filepath.Join(t.TempDir(), "zotigo-repo")
	if err := os.Mkdir(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitProvisionCommand(t, repository, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitProvisionCommand(t, repository, "add", "README.md")
	runGitProvisionCommand(t, repository, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
	inspection, err := InspectSource(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.AddSource(ctx, project.ID, SourceInput{
		Kind: inspection.Kind, CanonicalPath: inspection.CanonicalPath, GitCommonDir: inspection.GitCommonDir,
		GitObjectFormat: inspection.GitObjectFormat, SourceKey: inspection.SourceKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := store.CreateWorkspacePlan(ctx, project.ID, "主要 Workspace", []WorkspaceSourceInput{{
		SourceID: source.ID, BaseRef: "main",
	}})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = store.ProvisionWorkspace(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantProjectName := storageName(project.Name, project.ID, "project_", "project")
	wantWorkspaceName := storageName(workspace.Title, workspace.ID, "workspace_", "workspace")
	wantRoot := filepath.Join(root, "projects", wantProjectName, "workspaces", wantWorkspaceName)
	if workspace.RootPath != wantRoot {
		t.Fatalf("workspace root = %q, want %q", workspace.RootPath, wantRoot)
	}
	worktree := filepath.Join(workspace.RootPath, "code", "zotigo-repo")
	if _, err := os.Stat(filepath.Join(worktree, "README.md")); err != nil {
		t.Fatalf("repository-named worktree: %v", err)
	}
	wantBranch := "zotigo/" + wantWorkspaceName
	if branch := strings.TrimSpace(runGitProvisionCommand(t, worktree, "branch", "--show-current")); branch != wantBranch {
		t.Fatalf("worktree branch = %q, want %q", branch, wantBranch)
	}
}

func TestMigratesExistingLegacyWorktreeWithoutRenaming(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	projectID := "project_11111111-1111-1111-1111-111111111111"
	workspaceID := "workspace_22222222-2222-2222-2222-222222222222"
	sourceID := "source_33333333-3333-3333-3333-333333333333"
	nonce := "legacy-owner-nonce"
	repository := filepath.Join(t.TempDir(), "legacy-repo")
	if err := os.Mkdir(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitProvisionCommand(t, repository, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("legacy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitProvisionCommand(t, repository, "add", "README.md")
	runGitProvisionCommand(t, repository, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
	inspection, err := InspectSource(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	baseCommit := strings.TrimSpace(runGitProvisionCommand(t, repository, "rev-parse", "main"))
	legacyBranch := "zotigo/" + workspaceID + "/" + inspection.SourceKey
	legacyRoot := filepath.Join(root, "projects", projectID, "workspaces", workspaceID)
	legacyWorktree := filepath.Join(legacyRoot, "code", "legacy-repo")
	for _, directory := range []string{filepath.Join(legacyRoot, "code"), filepath.Join(legacyRoot, "artifacts"), filepath.Join(legacyRoot, "notes")} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	marker, err := json.Marshal(ownerMarker{Version: 1, ProjectID: projectID, WorkspaceID: workspaceID, Nonce: nonce})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyRoot, ownerMarkerName), marker, 0o600); err != nil {
		t.Fatal(err)
	}
	runGitProvisionCommand(t, repository, "worktree", "add", "-b", legacyBranch, legacyWorktree, baseCommit)
	runGitProvisionCommand(t, repository, "worktree", "lock", "--reason", "legacy workspace", legacyWorktree)
	runGitProvisionCommand(t, repository, "update-ref", checkoutOwnershipRef(workspaceID, inspection.SourceKey), baseCommit)

	db, err := sql.Open("sqlite", filepath.Join(root, "catalog.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE schema_meta (
			singleton INTEGER PRIMARY KEY CHECK(singleton = 1),
			version INTEGER NOT NULL CHECK(version > 0)
		)`,
		`INSERT INTO schema_meta(singleton, version) VALUES(1, 4)`,
		`CREATE TABLE projects (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL CHECK(length(trim(name)) BETWEEN 1 AND 200),
			status TEXT NOT NULL DEFAULT 'active'
				CHECK(status IN ('active', 'archiving', 'archived', 'deleting')),
			archived_at INTEGER,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE sources (
			id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
			kind TEXT NOT NULL CHECK(kind IN ('git', 'folder')),
			canonical_path TEXT NOT NULL,
			git_common_dir TEXT,
			git_object_format TEXT,
			folder_mode TEXT,
			source_key TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			UNIQUE(project_id, canonical_path),
			UNIQUE(project_id, source_key)
		)`,
		`CREATE TABLE workspaces (
			id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE RESTRICT,
			title TEXT NOT NULL CHECK(length(trim(title)) BETWEEN 1 AND 200),
			root_path TEXT NOT NULL UNIQUE,
			owner_nonce TEXT NOT NULL,
			status TEXT NOT NULL,
			error TEXT,
			archived_at INTEGER,
			deleted_at INTEGER,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE workspace_checkouts (
			workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
			source_id TEXT NOT NULL REFERENCES sources(id) ON DELETE RESTRICT,
			worktree_path TEXT NOT NULL UNIQUE,
			base_ref TEXT NOT NULL,
			base_commit TEXT NOT NULL,
			branch_name TEXT NOT NULL,
			owned_head TEXT NOT NULL,
			status TEXT NOT NULL,
			error TEXT,
			PRIMARY KEY(workspace_id, source_id)
		)`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	for _, insertion := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO projects(id, name, status, created_at, updated_at) VALUES(?, 'Legacy', 'active', 0, 0)`, []any{projectID}},
		{`INSERT INTO sources(id, project_id, kind, canonical_path, git_common_dir, git_object_format, source_key, created_at, updated_at)
			VALUES(?, ?, 'git', ?, ?, ?, ?, 0, 0)`, []any{sourceID, projectID, inspection.CanonicalPath, inspection.GitCommonDir, inspection.GitObjectFormat, inspection.SourceKey}},
		{`INSERT INTO workspaces(id, project_id, title, root_path, owner_nonce, status, created_at, updated_at)
			VALUES(?, ?, 'Legacy workspace', ?, ?, 'ready', 0, 0)`, []any{workspaceID, projectID, legacyRoot, nonce}},
		{`INSERT INTO workspace_checkouts(workspace_id, source_id, worktree_path, base_ref, base_commit, branch_name, owned_head, status)
			VALUES(?, ?, ?, 'main', ?, ?, ?, 'ready')`, []any{workspaceID, sourceID, legacyWorktree, baseCommit, legacyBranch, baseCommit}},
	} {
		if _, err := db.ExecContext(ctx, insertion.query, insertion.args...); err != nil {
			_ = db.Close()
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
	workspace, err := store.GetWorkspace(ctx, workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.GetProject(ctx, projectID)
	if err != nil {
		t.Fatal(err)
	}
	if project.storageName != projectID || workspace.storageName != workspaceID || workspace.RootPath != legacyRoot {
		t.Fatalf("migrated legacy records changed storage identity: project=%+v workspace=%+v", project, workspace)
	}
	if _, err := store.ProvisionWorkspace(ctx, workspace.ID); err != nil {
		t.Fatalf("retry migrated legacy workspace: %v", err)
	}
	bindings, err := store.ListWorkspaceSources(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].WorktreePath != legacyWorktree || bindings[0].BranchName != legacyBranch {
		t.Fatalf("migrated legacy binding changed: %+v", bindings)
	}
	if _, err := store.ArchiveWorkspace(ctx, workspace.ID); err != nil {
		t.Fatalf("archive migrated legacy workspace: %v", err)
	}
	if _, err := store.UnarchiveWorkspace(ctx, workspace.ID); err != nil {
		t.Fatalf("unarchive migrated legacy workspace: %v", err)
	}
	if branch := strings.TrimSpace(runGitProvisionCommand(t, legacyWorktree, "branch", "--show-current")); branch != legacyBranch {
		t.Fatalf("legacy worktree branch = %q, want %q", branch, legacyBranch)
	}
}

func TestReadyWorkspaceRetryRecreatesMissingWorktree(t *testing.T) {
	store, workspace, source := createGitWorkspaceFixture(t)
	worktree := filepath.Join(workspace.RootPath, "code", workspaceSourceName(source))
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ProvisionWorkspace(context.Background(), workspace.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(worktree, "README.md")); err != nil {
		t.Fatalf("recreated worktree: %v", err)
	}
}

func TestReadyWorkspaceRetryRenamesLegacyHashedWorktree(t *testing.T) {
	store, workspace, source := createGitWorkspaceFixture(t)
	ctx := context.Background()
	named := filepath.Join(workspace.RootPath, "code", workspaceSourceName(source))
	legacy := filepath.Join(workspace.RootPath, "code", source.SourceKey)
	runGitProvisionCommand(t, source.CanonicalPath, "worktree", "unlock", named)
	runGitProvisionCommand(t, source.CanonicalPath, "worktree", "move", named, legacy)
	runGitProvisionCommand(t, source.CanonicalPath, "worktree", "lock", "--reason", "legacy fixture", legacy)
	if _, err := store.db.ExecContext(ctx, `UPDATE workspace_checkouts SET worktree_path = ? WHERE workspace_id = ? AND source_id = ?`, legacy, workspace.ID, source.ID); err != nil {
		t.Fatalf("record legacy worktree: %v", err)
	}

	if _, err := store.ProvisionWorkspace(ctx, workspace.ID); err != nil {
		t.Fatalf("retry ready workspace: %v", err)
	}
	if _, err := os.Stat(filepath.Join(named, "README.md")); err != nil {
		t.Fatalf("repository-named worktree: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy hash path still exists: %v", err)
	}
	bindings, err := store.ListWorkspaceSources(ctx, workspace.ID)
	if err != nil || len(bindings) != 1 || bindings[0].WorktreePath != named {
		t.Fatalf("catalog bindings = %#v, err=%v", bindings, err)
	}
}

func TestReadyWorkspaceRetryReconcilesInterruptedLegacyWorktreeRename(t *testing.T) {
	store, workspace, source := createGitWorkspaceFixture(t)
	ctx := context.Background()
	named := filepath.Join(workspace.RootPath, "code", workspaceSourceName(source))
	legacy := filepath.Join(workspace.RootPath, "code", source.SourceKey)
	runGitProvisionCommand(t, source.CanonicalPath, "worktree", "unlock", named)
	runGitProvisionCommand(t, source.CanonicalPath, "worktree", "move", named, legacy)
	if _, err := store.db.ExecContext(ctx, `UPDATE workspace_checkouts SET worktree_path = ? WHERE workspace_id = ? AND source_id = ?`, legacy, workspace.ID, source.ID); err != nil {
		t.Fatalf("record legacy worktree: %v", err)
	}

	// Simulate interruption after Git moved the worktree but before SQLite
	// recorded the repository-named path.
	runGitProvisionCommand(t, source.CanonicalPath, "worktree", "move", legacy, named)

	if _, err := store.ProvisionWorkspace(ctx, workspace.ID); err != nil {
		t.Fatalf("recover interrupted worktree rename: %v", err)
	}
	if _, err := os.Stat(filepath.Join(named, "README.md")); err != nil {
		t.Fatalf("repository-named worktree: %v", err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy hash path still exists: %v", err)
	}
	bindings, err := store.ListWorkspaceSources(ctx, workspace.ID)
	if err != nil || len(bindings) != 1 || bindings[0].WorktreePath != named {
		t.Fatalf("catalog bindings = %#v, err=%v", bindings, err)
	}
}

func TestAddWorkspaceSourceToReadyWorkspace(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "Project")
	if err != nil {
		t.Fatal(err)
	}
	repository := t.TempDir()
	runGitProvisionCommand(t, repository, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitProvisionCommand(t, repository, "add", "README.md")
	runGitProvisionCommand(t, repository, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
	inspection, err := InspectSource(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.AddSource(ctx, project.ID, SourceInput{
		Kind: inspection.Kind, CanonicalPath: inspection.CanonicalPath, GitCommonDir: inspection.GitCommonDir,
		GitObjectFormat: inspection.GitObjectFormat, SourceKey: inspection.SourceKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := store.CreateWorkspace(ctx, project.ID, "中文工作区")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = store.ProvisionWorkspace(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	runGitProvisionCommand(t, repository, "branch", "occupied", "main")
	disposable, err := store.CreateWorkspace(ctx, project.ID, "Disposable")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ProvisionWorkspace(ctx, disposable.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddWorkspaceSource(ctx, disposable.ID, WorkspaceSourceInput{
		SourceID: source.ID, BaseRef: "main", BranchName: "occupied",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("occupied branch error = %v", err)
	}
	disposableBindings, err := store.ListWorkspaceSources(ctx, disposable.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(disposableBindings) != 0 {
		t.Fatalf("pre-side-effect failure persisted binding: %+v", disposableBindings)
	}
	if _, err := store.PreviewArchive(ctx, disposable.ID); err != nil {
		t.Fatalf("preview archive after failed add: %v", err)
	}
	if _, err := store.PreviewDelete(ctx, disposable.ID); err != nil {
		t.Fatalf("preview delete after failed add: %v", err)
	}
	if err := store.DeleteWorkspace(ctx, disposable.ID, disposable.Title); err != nil {
		t.Fatalf("delete after failed add: %v", err)
	}
	crashRetry, err := store.CreateWorkspace(ctx, project.ID, "Crash retry")
	if err != nil {
		t.Fatal(err)
	}
	crashRetry, err = store.ProvisionWorkspace(ctx, crashRetry.ID)
	if err != nil {
		t.Fatal(err)
	}
	crashBase := strings.TrimSpace(runGitProvisionCommand(t, repository, "rev-parse", "main"))
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO workspace_checkouts(
			workspace_id, source_id, worktree_path, base_ref, base_commit, branch_name, owned_head, status
		) VALUES(?, ?, ?, 'main', ?, 'occupied', ?, 'planned')
	`, crashRetry.ID, source.ID, filepath.Join(crashRetry.RootPath, "code", source.SourceKey), crashBase, crashBase); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ProvisionWorkspace(ctx, crashRetry.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("crash retry branch conflict = %v", err)
	}
	crashBindings, err := store.ListWorkspaceSources(ctx, crashRetry.ID)
	if err != nil || len(crashBindings) != 0 {
		t.Fatalf("crash retry retained unowned binding = %+v, err=%v", crashBindings, err)
	}
	if _, err := store.PreviewDelete(ctx, crashRetry.ID); err != nil {
		t.Fatalf("crash retry poisoned lifecycle: %v", err)
	}
	if _, err := store.AddWorkspaceSource(ctx, workspace.ID, WorkspaceSourceInput{
		SourceID: source.ID, BaseRef: "main", BranchName: "invalid..branch",
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid branch error = %v", err)
	}
	bindings, err := store.ListWorkspaceSources(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 0 {
		t.Fatalf("invalid branch persisted binding: %+v", bindings)
	}

	partial, err := store.CreateWorkspace(ctx, project.ID, "Partial refs")
	if err != nil {
		t.Fatal(err)
	}
	partial, err = store.ProvisionWorkspace(ctx, partial.ID)
	if err != nil {
		t.Fatal(err)
	}
	baseCommit := strings.TrimSpace(runGitProvisionCommand(t, repository, "rev-parse", "main"))
	partialBranch := "zotigo/partial-ref"
	partialTarget := filepath.Join(partial.RootPath, "code", workspaceSourceName(source))
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO workspace_checkouts(
			workspace_id, source_id, worktree_path, base_ref, base_commit, branch_name, owned_head, status, error
		) VALUES(?, ?, ?, 'main', ?, ?, ?, 'error', 'partial worktree creation')
	`, partial.ID, source.ID, partialTarget, baseCommit, partialBranch, baseCommit); err != nil {
		t.Fatal(err)
	}
	ownershipRef := checkoutOwnershipRef(partial.ID, source.SourceKey)
	runGitProvisionCommand(t, repository, "update-ref", "refs/heads/"+partialBranch, baseCommit)
	runGitProvisionCommand(t, repository, "update-ref", ownershipRef, baseCommit)
	if err := os.WriteFile(filepath.Join(repository, "SECOND.md"), []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitProvisionCommand(t, repository, "add", "SECOND.md")
	runGitProvisionCommand(t, repository, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "second")
	advancedCommit := strings.TrimSpace(runGitProvisionCommand(t, repository, "rev-parse", "HEAD"))
	runGitProvisionCommand(t, repository, "update-ref", "refs/heads/"+partialBranch, advancedCommit)
	if _, err := store.ProvisionWorkspace(ctx, partial.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("advanced partial branch error = %v", err)
	}
	if _, err := os.Lstat(partialTarget); !os.IsNotExist(err) {
		t.Fatalf("partial branch retry created worktree: %v", err)
	}

	binding, err := store.AddWorkspaceSource(ctx, workspace.ID, WorkspaceSourceInput{SourceID: source.ID, BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	wantBranch := "zotigo/" + workspace.storageName
	if binding.BranchName != wantBranch || binding.Status != "ready" {
		t.Fatalf("binding = %+v, want branch %q and ready", binding, wantBranch)
	}
	if strings.Contains(binding.BranchName, workspace.ID) {
		t.Fatalf("default branch %q contains full workspace ID", binding.BranchName)
	}
	if _, err := os.Stat(filepath.Join(binding.WorktreePath, "README.md")); err != nil {
		t.Fatalf("added worktree: %v", err)
	}
	bindings, err = store.ListWorkspaceSources(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].Source.ID != source.ID {
		t.Fatalf("workspace sources = %+v", bindings)
	}
	if _, err := store.AddWorkspaceSource(ctx, workspace.ID, WorkspaceSourceInput{SourceID: source.ID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate source error = %v", err)
	}
	otherProject, err := store.CreateProject(ctx, "Other")
	if err != nil {
		t.Fatal(err)
	}
	otherWorkspace, err := store.CreateWorkspace(ctx, otherProject.ID, "Other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ProvisionWorkspace(ctx, otherWorkspace.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddWorkspaceSource(ctx, otherWorkspace.ID, WorkspaceSourceInput{SourceID: source.ID}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cross-project source error = %v", err)
	}
	otherBindings, err := store.ListWorkspaceSources(ctx, otherWorkspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(otherBindings) != 0 {
		t.Fatalf("cross-project attempt added bindings: %+v", otherBindings)
	}
	runGitProvisionCommand(t, repository, "update-ref", "-d", checkoutOwnershipRef(workspace.ID, source.SourceKey))
	if _, err := store.db.ExecContext(ctx, `
		UPDATE workspace_checkouts SET status = 'error', error = 'ownership removed'
		WHERE workspace_id = ? AND source_id = ?
	`, workspace.ID, source.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ProvisionWorkspace(ctx, workspace.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("missing ownership retry error = %v", err)
	}
	retained, err := store.ListWorkspaceSources(ctx, workspace.ID)
	if err != nil || len(retained) != 1 {
		t.Fatalf("missing ownership forgot exact worktree binding = %+v, err=%v", retained, err)
	}
}

func TestAddWorkspaceSourceRejectsSymlinkedManagedPaths(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "Symlink safety")
	if err != nil {
		t.Fatal(err)
	}
	sourcePath := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourcePath, "content.txt"), []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	inspection, err := InspectSource(ctx, sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.AddSource(ctx, project.ID, SourceInput{
		Kind: SourceKindFolder, CanonicalPath: inspection.CanonicalPath,
		FolderMode: FolderModeCopy, SourceKey: inspection.SourceKey,
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("code parent", func(t *testing.T) {
		workspace, err := store.CreateWorkspace(ctx, project.ID, "Symlink code")
		if err != nil {
			t.Fatal(err)
		}
		workspace, err = store.ProvisionWorkspace(ctx, workspace.ID)
		if err != nil {
			t.Fatal(err)
		}
		codePath := filepath.Join(workspace.RootPath, "code")
		if err := os.Remove(codePath); err != nil {
			t.Fatal(err)
		}
		external := t.TempDir()
		if err := os.Symlink(external, codePath); err != nil {
			t.Fatal(err)
		}
		if _, err := store.AddWorkspaceSource(ctx, workspace.ID, WorkspaceSourceInput{SourceID: source.ID, Mode: FolderModeCopy}); !errors.Is(err, ErrConflict) {
			t.Fatalf("symlinked code error = %v", err)
		}
		if entries, err := os.ReadDir(external); err != nil || len(entries) != 0 {
			t.Fatalf("external code directory changed: entries=%v err=%v", entries, err)
		}
		bindings, err := store.ListWorkspaceSources(ctx, workspace.ID)
		if err != nil || len(bindings) != 0 {
			t.Fatalf("symlinked code bindings = %+v, err=%v", bindings, err)
		}
	})

	t.Run("workspace root", func(t *testing.T) {
		workspace, err := store.CreateWorkspace(ctx, project.ID, "Symlink root")
		if err != nil {
			t.Fatal(err)
		}
		workspace, err = store.ProvisionWorkspace(ctx, workspace.ID)
		if err != nil {
			t.Fatal(err)
		}
		marker, err := os.ReadFile(filepath.Join(workspace.RootPath, ownerMarkerName))
		if err != nil {
			t.Fatal(err)
		}
		backup := workspace.RootPath + ".backup"
		if err := os.Rename(workspace.RootPath, backup); err != nil {
			t.Fatal(err)
		}
		external := t.TempDir()
		if err := os.WriteFile(filepath.Join(external, ownerMarkerName), marker, 0o600); err != nil {
			t.Fatal(err)
		}
		for _, directory := range []string{"code", "artifacts", "notes"} {
			if err := os.Mkdir(filepath.Join(external, directory), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(external, workspace.RootPath); err != nil {
			t.Fatal(err)
		}
		if _, err := store.AddWorkspaceSource(ctx, workspace.ID, WorkspaceSourceInput{SourceID: source.ID, Mode: FolderModeCopy}); !errors.Is(err, ErrConflict) {
			t.Fatalf("symlinked root error = %v", err)
		}
		if entries, err := os.ReadDir(filepath.Join(external, "code")); err != nil || len(entries) != 0 {
			t.Fatalf("external workspace directory changed: entries=%v err=%v", entries, err)
		}
		bindings, err := store.ListWorkspaceSources(ctx, workspace.ID)
		if err != nil || len(bindings) != 0 {
			t.Fatalf("symlinked root bindings = %+v, err=%v", bindings, err)
		}
	})

	t.Run("copy target", func(t *testing.T) {
		workspace, err := store.CreateWorkspace(ctx, project.ID, "Symlink target")
		if err != nil {
			t.Fatal(err)
		}
		workspace, err = store.ProvisionWorkspace(ctx, workspace.ID)
		if err != nil {
			t.Fatal(err)
		}
		external := t.TempDir()
		if err := writeBindingMarker(external, workspace.ID, source.ID); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(workspace.RootPath, "code", workspaceSourceName(source))
		if err := os.Symlink(external, target); err != nil {
			t.Fatal(err)
		}
		if _, err := store.AddWorkspaceSource(ctx, workspace.ID, WorkspaceSourceInput{SourceID: source.ID, Mode: FolderModeCopy}); !errors.Is(err, ErrConflict) {
			t.Fatalf("symlinked target error = %v", err)
		}
		bindings, err := store.ListWorkspaceSources(ctx, workspace.ID)
		if err != nil || len(bindings) != 0 {
			t.Fatalf("symlinked target bindings = %+v, err=%v", bindings, err)
		}
	})

	t.Run("crash retry", func(t *testing.T) {
		workspace, err := store.CreateWorkspace(ctx, project.ID, "Folder crash retry")
		if err != nil {
			t.Fatal(err)
		}
		workspace, err = store.ProvisionWorkspace(ctx, workspace.ID)
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(workspace.RootPath, "code", workspaceSourceName(source))
		if err := os.WriteFile(target, []byte("occupied"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `
			INSERT INTO workspace_folders(workspace_id, source_id, mode, target_path, status)
			VALUES(?, ?, 'copy', ?, 'planned')
		`, workspace.ID, source.ID, target); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ProvisionWorkspace(ctx, workspace.ID); !errors.Is(err, ErrConflict) {
			t.Fatalf("folder crash retry error = %v", err)
		}
		bindings, err := store.ListWorkspaceSources(ctx, workspace.ID)
		if err != nil || len(bindings) != 0 {
			t.Fatalf("folder crash retry retained binding = %+v, err=%v", bindings, err)
		}
	})
}

func TestFolderBindingRecoveryRetainsOwnedTargets(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "Folder recovery")
	if err != nil {
		t.Fatal(err)
	}
	newSource := func(t *testing.T, mode FolderMode) (Source, string) {
		t.Helper()
		path := t.TempDir()
		if err := os.WriteFile(filepath.Join(path, "content.txt"), []byte("source"), 0o644); err != nil {
			t.Fatal(err)
		}
		inspection, err := InspectSource(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		source, err := store.AddSource(ctx, project.ID, SourceInput{
			Kind: SourceKindFolder, CanonicalPath: inspection.CanonicalPath,
			FolderMode: mode, SourceKey: inspection.SourceKey,
		})
		if err != nil {
			t.Fatal(err)
		}
		return source, path
	}

	for _, mode := range []FolderMode{FolderModeCopy, FolderModeDirect} {
		t.Run("ready "+string(mode), func(t *testing.T) {
			source, sourcePath := newSource(t, mode)
			workspace, err := store.CreateWorkspacePlan(ctx, project.ID, "Ready "+string(mode), []WorkspaceSourceInput{{SourceID: source.ID, Mode: mode}})
			if err != nil {
				t.Fatal(err)
			}
			workspace, err = store.ProvisionWorkspace(ctx, workspace.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(sourcePath, sourcePath+".missing"); err != nil {
				t.Fatal(err)
			}
			for attempt := 0; attempt < 2; attempt++ {
				if _, err := store.ProvisionWorkspace(ctx, workspace.ID); err == nil {
					t.Fatalf("retry %d error = %v", attempt, err)
				}
				bindings, err := store.ListWorkspaceSources(ctx, workspace.ID)
				if err != nil || len(bindings) != 1 {
					t.Fatalf("retry %d forgot owned binding = %+v, err=%v", attempt, bindings, err)
				}
			}
		})
	}

	t.Run("planned published copy", func(t *testing.T) {
		source, sourcePath := newSource(t, FolderModeCopy)
		workspace, err := store.CreateWorkspace(ctx, project.ID, "Published copy")
		if err != nil {
			t.Fatal(err)
		}
		workspace, err = store.ProvisionWorkspace(ctx, workspace.ID)
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(workspace.RootPath, "code", workspaceSourceName(source))
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeBindingMarker(target, workspace.ID, source.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `
			INSERT INTO workspace_folders(workspace_id, source_id, mode, target_path, status)
			VALUES(?, ?, 'copy', ?, 'planned')
		`, workspace.ID, source.ID, target); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(sourcePath, sourcePath+".missing"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ProvisionWorkspace(ctx, workspace.ID); err == nil {
			t.Fatalf("published copy retry error = %v", err)
		}
		bindings, err := store.ListWorkspaceSources(ctx, workspace.ID)
		if err != nil || len(bindings) != 1 {
			t.Fatalf("published copy binding = %+v, err=%v", bindings, err)
		}
	})
}

func runGitProvisionCommand(t *testing.T, directory string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = directory
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return string(output)
}
