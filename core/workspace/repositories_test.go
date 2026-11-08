package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func repositoryFixture(t *testing.T) (*Store, string) {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	remote := filepath.Join(t.TempDir(), "remote repo")
	if err := os.Mkdir(remote, 0o700); err != nil {
		t.Fatal(err)
	}
	runGitProvisionCommand(t, remote, "init", "-b", "trunk")
	runGitProvisionCommand(t, remote, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "initial")
	return store, remote
}

func TestManagedRepositorySharedFreshDefaultAndOverrides(t *testing.T) {
	ctx := context.Background()
	store, remote := repositoryFixture(t)
	repo, err := store.CreateRepository(ctx, remote, "")
	if err != nil {
		t.Fatal(err)
	}
	if repo.DefaultBranch != "trunk" {
		t.Fatalf("default branch=%q", repo.DefaultBranch)
	}
	again, err := store.CreateRepository(ctx, remote, "")
	if err != nil || again.ID != repo.ID {
		t.Fatalf("dedup=%+v %v", again, err)
	}
	p1, err := store.CreateProject(ctx, "First")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := store.CreateProject(ctx, "Second")
	if err != nil {
		t.Fatal(err)
	}
	s1, err := store.AddRepositorySource(ctx, p1.ID, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := store.AddRepositorySource(ctx, p2.ID, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s1.GitCommonDir != s2.GitCommonDir || s1.ID == s2.ID {
		t.Fatalf("shared repository: %+v %+v", s1, s2)
	}
	if got, err := store.AddRepositorySource(ctx, p1.ID, repo.ID); err != nil || got.ID != s1.ID {
		t.Fatalf("reattach=%+v %v", got, err)
	}
	runGitProvisionCommand(t, remote, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "latest")
	latest := strings.TrimSpace(runGitProvisionCommand(t, remote, "rev-parse", "HEAD"))
	w1, err := store.CreateWorkspacePlan(ctx, p1.ID, "Latest", []WorkspaceSourceInput{{SourceID: s1.ID}})
	if err != nil {
		t.Fatal(err)
	}
	// A later fetch must not invalidate a plan's already pinned commit.
	runGitProvisionCommand(t, remote, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "newer")
	w2, err := store.CreateWorkspacePlan(ctx, p2.ID, "Newer", []WorkspaceSourceInput{{SourceID: s2.ID, BaseRef: "trunk"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, workspace := range []Workspace{w1, w2} {
		if _, err := store.ProvisionWorkspace(ctx, workspace.ID); err != nil {
			t.Fatal(err)
		}
	}
	sources, err := store.ListWorkspaceSources(ctx, w1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sources[0].BaseCommit != latest {
		t.Fatalf("base=%s want %s", sources[0].BaseCommit, latest)
	}
	if head := strings.TrimSpace(runGitProvisionCommand(t, sources[0].TargetPath, "rev-parse", "HEAD")); head != latest {
		t.Fatalf("worktree head=%s", head)
	}
	runGitProvisionCommand(t, remote, "checkout", "-b", "release")
	w3, err := store.CreateWorkspacePlan(ctx, p1.ID, "New default", []WorkspaceSourceInput{{SourceID: s1.ID}})
	if err != nil {
		t.Fatal(err)
	}
	sources, err = store.ListWorkspaceSources(ctx, w3.ID)
	if err != nil {
		t.Fatal(err)
	}
	if sources[0].BaseRef != "refs/remotes/origin/release" {
		t.Fatalf("default not refreshed: %+v", sources)
	}
	if err := os.Rename(remote, remote+"-offline"); err != nil {
		t.Fatal(err)
	}
	offline, err := store.CreateWorkspacePlan(ctx, p1.ID, "Offline", []WorkspaceSourceInput{{SourceID: s1.ID}})
	if err != nil {
		t.Fatal(err)
	}
	offlineSources, err := store.ListWorkspaceSources(ctx, offline.ID)
	if err != nil || len(offlineSources) != 1 || !offlineSources[0].UsedCachedBase || offlineSources[0].BaseSyncedAt == nil {
		t.Fatalf("cached source=%+v err=%v", offlineSources, err)
	}
	if _, err := store.ProvisionWorkspace(ctx, offline.ID); err != nil {
		t.Fatal(err)
	}
}

func TestManagedRepositoryMigrationPreservesLegacyWorktree(t *testing.T) {
	ctx := context.Background()
	store, legacy, oldSource := createGitWorkspaceFixture(t)
	oldBindings, err := store.ListWorkspaceSources(ctx, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	worktree := oldBindings[0].TargetPath
	if err := os.WriteFile(filepath.Join(worktree, "README.md"), []byte("staged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitProvisionCommand(t, worktree, "add", "README.md")
	if err := os.WriteFile(filepath.Join(worktree, "README.md"), []byte("unstaged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "untracked"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := runGitProvisionCommand(t, worktree, "status", "--porcelain=v1")
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGitProvisionCommand(t, oldSource.CanonicalPath, "clone", "--bare", "--", oldSource.CanonicalPath, remote)
	runGitProvisionCommand(t, oldSource.CanonicalPath, "remote", "add", "origin", remote)
	runGitProvisionCommand(t, oldSource.CanonicalPath, "checkout", "-b", "unpublished")
	runGitProvisionCommand(t, oldSource.CanonicalPath, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "local only")
	localHead := strings.TrimSpace(runGitProvisionCommand(t, oldSource.CanonicalPath, "rev-parse", "HEAD"))
	if err := store.migrateRepositorySources(ctx); err != nil {
		t.Fatal(err)
	}
	sources, err := store.ListSources(ctx, legacy.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].ID == oldSource.ID || sources[0].RepositoryID == "" {
		t.Fatalf("migration=%+v", sources)
	}
	if _, err := os.Stat(sources[0].CanonicalPath); err != nil {
		t.Fatalf("migration switched before preparing clone: %v", err)
	}
	// Old clients may still submit their original source ID.
	fresh, err := store.CreateWorkspacePlan(ctx, legacy.ProjectID, "Managed", []WorkspaceSourceInput{{SourceID: oldSource.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ProvisionWorkspace(ctx, fresh.ID); err != nil {
		t.Fatal(err)
	}
	imported := "refs/zotigo/imported/" + oldSource.ID + "/heads/unpublished"
	if head := strings.TrimSpace(runGitProvisionCommand(t, sources[0].CanonicalPath, "rev-parse", imported)); head != localHead {
		t.Fatalf("local branch lost: %s", head)
	}
	if after := runGitProvisionCommand(t, worktree, "status", "--porcelain=v1"); after != before {
		t.Fatalf("dirty state changed: before %q after %q", before, after)
	}
	bindings, err := store.ListWorkspaceSources(ctx, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bindings[0].Source.CanonicalPath != oldSource.CanonicalPath || bindings[0].TargetPath != worktree {
		t.Fatal("legacy source or path changed")
	}
	if _, err := store.ProvisionWorkspace(ctx, legacy.ID); err != nil {
		t.Fatalf("legacy reprovision: %v", err)
	}
	if _, err := store.AddWorkspaceSource(ctx, legacy.ID, WorkspaceSourceInput{SourceID: sources[0].ID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate migrated repository accepted: %v", err)
	}
	if _, err := store.ArchiveWorkspace(ctx, legacy.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("dirty archive should be refused: %v", err)
	}
	if again, err := store.ListSources(ctx, legacy.ProjectID); err != nil || len(again) != 1 || again[0].ID != sources[0].ID {
		t.Fatalf("not idempotent: %+v %v", again, err)
	}
	// New workspaces remain usable after the original local clone disappears.
	if err := os.Rename(oldSource.CanonicalPath, oldSource.CanonicalPath+"-moved"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateWorkspacePlan(ctx, legacy.ProjectID, "Independent", []WorkspaceSourceInput{{SourceID: sources[0].ID}}); err != nil {
		t.Fatalf("original still required: %v", err)
	}
}

func TestManagedRepositoryNoRemoteAndAmbiguousRemoteStayLocal(t *testing.T) {
	ctx := context.Background()
	store, legacy, source := createGitWorkspaceFixture(t)
	for _, remotes := range []int{0, 2} {
		if remotes == 2 {
			runGitProvisionCommand(t, source.CanonicalPath, "remote", "add", "origin", "https://example.com/fork.git")
			runGitProvisionCommand(t, source.CanonicalPath, "remote", "add", "upstream", "https://example.com/upstream.git")
		}
		if _, err := store.ListRepositories(ctx); err != nil {
			t.Fatal(err)
		}
		sources, err := store.ListSources(ctx, legacy.ProjectID)
		if err != nil {
			t.Fatal(err)
		}
		if len(sources) != 1 || sources[0].ID != source.ID || sources[0].RepositoryID != "" {
			t.Fatalf("local source migrated: %+v", sources)
		}
	}
}

func TestManagedRepositoryFailedCloneCanRetry(t *testing.T) {
	ctx := context.Background()
	store, _ := repositoryFixture(t)
	remote := filepath.Join(t.TempDir(), "not-yet-created")
	if _, err := store.CreateRepository(ctx, remote, ""); err == nil {
		t.Fatal("missing remote accepted")
	}
	repos, err := store.ListRepositories(ctx)
	if err != nil || len(repos) != 1 {
		t.Fatalf("retry record: %+v %v", repos, err)
	}
	if err := os.Mkdir(remote, 0o700); err != nil {
		t.Fatal(err)
	}
	runGitProvisionCommand(t, remote, "init", "-b", "trunk")
	runGitProvisionCommand(t, remote, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "initial")
	retried, err := store.CreateRepository(ctx, remote, "")
	if err != nil {
		t.Fatal(err)
	}
	if retried.ID != repos[0].ID || retried.DefaultBranch != "trunk" {
		t.Fatalf("retry=%+v", retried)
	}
}

func TestManagedRepositoryMissingDefaultAllowsExplicitBranch(t *testing.T) {
	ctx := context.Background()
	store, remote := repositoryFixture(t)
	bare := filepath.Join(t.TempDir(), "remote.git")
	runGitProvisionCommand(t, remote, "clone", "--bare", remote, bare)
	runGitProvisionCommand(t, bare, "symbolic-ref", "HEAD", "refs/heads/missing")
	repo, err := store.CreateRepository(ctx, bare, "")
	if err != nil {
		t.Fatal(err)
	}
	if repo.DefaultBranch != "" {
		t.Fatalf("default=%s", repo.DefaultBranch)
	}
	project, err := store.CreateProject(ctx, "Missing default")
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.AddRepositorySource(ctx, project.ID, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateWorkspacePlan(ctx, project.ID, "Automatic", []WorkspaceSourceInput{{SourceID: source.ID}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing default=%v", err)
	}
	if _, err := store.CreateWorkspacePlan(ctx, project.ID, "Explicit", []WorkspaceSourceInput{{SourceID: source.ID, BaseRef: "trunk"}}); err != nil {
		t.Fatal(err)
	}
}

func TestManagedRepositoryRejectsCredentialURLsAndHelpers(t *testing.T) {
	for _, remote := range []string{"ext::sh -c id", "https://secret@example.com/repo.git", "https://u:secret@example.com/repo.git", "https://example.com/repo?token=secret", "-upload-pack=evil", "ftp://example.com/repo", "relative/path"} {
		if err := validateRemote(remote); !errors.Is(err, ErrInvalid) {
			t.Errorf("accepted %q: %v", remote, err)
		}
	}
	for _, remote := range []string{"git@example.com:org/repo.git", "https://example.com/org/repo.git", "ssh://git@example.com/org/repo.git", "/tmp/repo with spaces"} {
		if err := validateRemote(remote); err != nil {
			t.Errorf("rejected %q: %v", remote, err)
		}
	}
}

func TestManagedRepositoryReregistrationAndProjectDeletion(t *testing.T) {
	ctx := context.Background()
	store, remote := repositoryFixture(t)
	local := filepath.Join(t.TempDir(), "local")
	runGitProvisionCommand(t, remote, "clone", remote, local)
	inspection, err := InspectSource(ctx, local)
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, "Migrated project")
	if err != nil {
		t.Fatal(err)
	}
	input := SourceInput{Kind: SourceKindGit, CanonicalPath: inspection.CanonicalPath, GitCommonDir: inspection.GitCommonDir, GitObjectFormat: inspection.GitObjectFormat, SourceKey: inspection.SourceKey}
	source, err := store.AddSource(ctx, project.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	if source.RepositoryID != "" {
		t.Fatal("AddSource synchronously migrated local source")
	}
	if err := store.migrateRepositorySources(ctx); err != nil {
		t.Fatal(err)
	}
	sources, err := store.ListSources(ctx, project.ID)
	if err != nil || len(sources) != 1 {
		t.Fatalf("sources=%+v %v", sources, err)
	}
	source = sources[0]
	if source.RepositoryID == "" {
		t.Fatal("local remote was not adopted")
	}
	if err := store.DeleteSource(ctx, project.ID, source.ID); err != nil {
		t.Fatal(err)
	}
	readded, err := store.AddSource(ctx, project.ID, input)
	if err != nil {
		t.Fatal(err)
	}
	if readded.ID != source.ID {
		t.Fatal("lost managed source identity")
	}
	workspace, err := store.CreateWorkspacePlan(ctx, project.ID, "After re-add", []WorkspaceSourceInput{{SourceID: readded.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ProvisionWorkspace(ctx, workspace.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteProject(ctx, project.ID, project.Name); err != nil {
		t.Fatalf("delete migrated project: %v", err)
	}
	if _, err := store.GetRepository(ctx, source.RepositoryID); err != nil {
		t.Fatalf("shared repo deleted with project: %v", err)
	}
	if _, err := os.Stat(local); err != nil {
		t.Fatalf("original deleted: %v", err)
	}
}

func TestManagedRepositoryDowngradeKeepsGitAndSourceBindings(t *testing.T) {
	ctx := context.Background()
	store, remote := repositoryFixture(t)
	repo, err := store.CreateRepository(ctx, remote, "")
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, "Downgrade")
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.AddRepositorySource(ctx, project.ID, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := store.CreateWorkspacePlan(ctx, project.ID, "Keep", []WorkspaceSourceInput{{SourceID: source.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ProvisionWorkspace(ctx, workspace.ID); err != nil {
		t.Fatal(err)
	}
	bindings, err := store.ListWorkspaceSources(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	head := runGitProvisionCommand(t, bindings[0].TargetPath, "rev-parse", "HEAD")
	root := store.RootDir()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := MigrateCatalog(ctx, root, 16); err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	if got := runGitProvisionCommand(t, bindings[0].TargetPath, "rev-parse", "HEAD"); got != head {
		t.Fatal("downgrade changed worktree")
	}
	if err := MigrateCatalog(ctx, root, 17); err != nil {
		t.Fatalf("re-upgrade: %v", err)
	}
	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.ProvisionWorkspace(ctx, workspace.ID); err != nil {
		t.Fatalf("existing workspace after downgrade/re-upgrade: %v", err)
	}
}

func TestManagedRepositoryPreparedUnboundDowngradeRestoresLocalSource(t *testing.T) {
	ctx := context.Background()
	store, legacy, source := createGitWorkspaceFixture(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGitProvisionCommand(t, source.CanonicalPath, "clone", "--bare", source.CanonicalPath, remote)
	runGitProvisionCommand(t, source.CanonicalPath, "remote", "add", "origin", remote)
	if err := store.migrateRepositorySources(ctx); err != nil {
		t.Fatal(err)
	}
	replacements, err := store.ListSources(ctx, legacy.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(replacements) != 1 || replacements[0].ID == source.ID {
		t.Fatal("source not migrated")
	}
	if _, err := os.Stat(replacements[0].CanonicalPath); err != nil {
		t.Fatal("migration cache was not prepared")
	}
	root := store.RootDir()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := MigrateCatalog(ctx, root, 16); err != nil {
		t.Fatal(err)
	}
	old, err := openWriter(root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	var registeredID, registeredPath string
	if err := old.db.QueryRow(`SELECT id,canonical_path FROM sources WHERE project_id=? AND registered=1`, legacy.ProjectID).Scan(&registeredID, &registeredPath); err != nil {
		t.Fatal(err)
	}
	if registeredID != source.ID || registeredPath != source.CanonicalPath {
		t.Fatalf("downgrade registration=%s %s", registeredID, registeredPath)
	}
	if _, err := runGit(ctx, registeredPath, "rev-parse", "--verify", "HEAD"); err != nil {
		t.Fatalf("downgrade source unusable: %v", err)
	}
}

func TestManagedRepositoryUnsupportedLocalNameDoesNotBlockStartup(t *testing.T) {
	ctx := context.Background()
	store, remote := repositoryFixture(t)
	local := filepath.Join(t.TempDir(), strings.Repeat("x", 120))
	runGitProvisionCommand(t, remote, "clone", remote, local)
	inspection, err := InspectSource(ctx, local)
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, "Local naming")
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.AddSource(ctx, project.ID, SourceInput{Kind: SourceKindGit, CanonicalPath: inspection.CanonicalPath, GitCommonDir: inspection.GitCommonDir, GitObjectFormat: inspection.GitObjectFormat, SourceKey: inspection.SourceKey})
	if err != nil {
		t.Fatal(err)
	}
	if source.RepositoryID != "" {
		t.Fatal("unsupported name migrated")
	}
	root := store.RootDir()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root)
	if err != nil {
		t.Fatalf("startup blocked: %v", err)
	}
	defer reopened.Close()
	sources, err := reopened.ListSources(ctx, project.ID)
	if err != nil || len(sources) != 1 || sources[0].ID != source.ID {
		t.Fatalf("local source lost: %+v %v", sources, err)
	}
}

func TestManagedRepositoryMigrationReactivatesReplacement(t *testing.T) {
	ctx := context.Background()
	store, legacy, local := createGitWorkspaceFixture(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGitProvisionCommand(t, local.CanonicalPath, "clone", "--bare", local.CanonicalPath, remote)
	repo, err := store.CreateRepository(ctx, remote, "")
	if err != nil {
		t.Fatal(err)
	}
	managed, err := store.AddRepositorySource(ctx, legacy.ProjectID, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSource(ctx, legacy.ProjectID, managed.ID); err != nil {
		t.Fatal(err)
	}
	runGitProvisionCommand(t, local.CanonicalPath, "remote", "add", "origin", remote)
	if err := store.migrateRepositorySources(ctx); err != nil {
		t.Fatal(err)
	}
	sources, err := store.ListSources(ctx, legacy.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].ID != managed.ID {
		t.Fatalf("source disappeared after migration: %+v", sources)
	}
	if _, err := store.CreateWorkspacePlan(ctx, legacy.ProjectID, "Reactivated", []WorkspaceSourceInput{{SourceID: local.ID}}); err != nil {
		t.Fatal(err)
	}
}
