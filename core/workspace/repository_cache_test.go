package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRepositoryCacheFallbackSnapshotAndValidation(t *testing.T) {
	ctx := context.Background()
	store, remote := repositoryFixture(t)
	repo, err := createReadyRepository(store, ctx, remote, "cache")
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, "Offline")
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.AddRepositorySource(ctx, project.ID, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(remote, remote+"-offline"); err != nil {
		t.Fatal(err)
	}
	workspace, err := store.CreateWorkspacePlan(ctx, project.ID, "Cached", []WorkspaceSourceInput{{SourceID: source.ID}})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := store.ListWorkspaceSources(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bindings[0].UsedCachedBase || bindings[0].BaseSyncedAt == nil {
		t.Fatalf("missing cache snapshot: %+v", bindings)
	}
	snapshot := *bindings[0].BaseSyncedAt
	if _, err := store.CreateWorkspacePlan(ctx, project.ID, "Missing", []WorkspaceSourceInput{{SourceID: source.ID, BaseRef: "missing"}}); err == nil {
		t.Fatal("created an unavailable branch from cache")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.CreateWorkspacePlan(canceled, project.ID, "Canceled", []WorkspaceSourceInput{{SourceID: source.ID}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled request=%v", err)
	}
	runGitProvisionCommand(t, repo.CanonicalPath, "remote", "set-url", "origin", remote+"-other")
	if _, err := store.CreateWorkspacePlan(ctx, project.ID, "Identity", []WorkspaceSourceInput{{SourceID: source.ID}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("accepted changed identity: %v", err)
	}
	runGitProvisionCommand(t, repo.CanonicalPath, "remote", "set-url", "origin", remote)
	if err := os.Rename(remote+"-offline", remote); err != nil {
		t.Fatal(err)
	}
	if _, err := store.refreshRepository(ctx, repo); err != nil {
		t.Fatal(err)
	}
	bindings, err = store.ListWorkspaceSources(ctx, workspace.ID)
	if err != nil || !bindings[0].UsedCachedBase || !bindings[0].BaseSyncedAt.Equal(snapshot) {
		t.Fatalf("snapshot changed: %+v %v", bindings, err)
	}
}

func TestRepositoryCacheLockTimeoutUsesLastSuccessfulCache(t *testing.T) {
	ctx := context.Background()
	store, remote := repositoryFixture(t)
	repo, err := createReadyRepository(store, ctx, remote, "cache")
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := store.lockRepository(ctx, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	cached, used, release, err := store.refreshRepositoryForUseWithin(ctx, repo, 20*time.Millisecond)
	defer release()
	if err != nil || !used || cached.LastSyncedAt == nil {
		t.Fatalf("fallback=%+v %v %v", cached, used, err)
	}
	if _, err := store.db.Exec(`UPDATE repositories SET last_synced_at=NULL WHERE id=?`, repo.ID); err != nil {
		t.Fatal(err)
	}
	_, _, release, err = store.refreshRepositoryForUseWithin(ctx, repo, 20*time.Millisecond)
	release()
	if err == nil {
		t.Fatal("accepted cache without any successful sync")
	}
}

func TestRepositoryMigrationFailureKeepsLocalAndRetriesAfterRestart(t *testing.T) {
	ctx := context.Background()
	store, legacy, source := createGitWorkspaceFixture(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGitProvisionCommand(t, source.CanonicalPath, "clone", "--bare", source.CanonicalPath, remote)
	runGitProvisionCommand(t, source.CanonicalPath, "remote", "add", "origin", remote)
	if err := os.Rename(remote, remote+"-offline"); err != nil {
		t.Fatal(err)
	}
	if err := store.migrateRepositorySources(ctx); err == nil {
		t.Fatal("offline migration succeeded")
	}
	sources, err := store.ListSources(ctx, legacy.ProjectID)
	if err != nil || len(sources) != 1 || sources[0].ID != source.ID || sources[0].RepositoryID != "" {
		t.Fatalf("failed migration replaced source: %+v %v", sources, err)
	}
	var status string
	if err := store.db.QueryRow(`SELECT status FROM source_repository_migrations WHERE source_id=?`, source.ID).Scan(&status); err != nil || status != "retry" {
		t.Fatalf("retry state=%s %v", status, err)
	}
	if _, err := store.CreateWorkspacePlan(ctx, legacy.ProjectID, "Still local", []WorkspaceSourceInput{{SourceID: source.ID}}); err != nil {
		t.Fatal(err)
	}
	root := store.RootDir()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(remote+"-offline", remote); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.db.Exec(`UPDATE source_repository_migrations SET next_attempt_at=0 WHERE source_id=?`, source.ID); err != nil {
		t.Fatal(err)
	}
	if err := reopened.migrateRepositorySources(ctx); err != nil {
		t.Fatal(err)
	}
	sources, err = reopened.ListSources(ctx, legacy.ProjectID)
	if err != nil || len(sources) != 1 || sources[0].RepositoryID == "" || sources[0].LastSyncedAt == nil {
		t.Fatalf("retry failed: %+v %v", sources, err)
	}
}

func TestRepositoryMaintenanceDoesNotProbeCheckedLocalSourcesOrOpen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Git recording wrapper uses POSIX shell")
	}
	ctx := context.Background()
	store, _, source := createGitWorkspaceFixture(t)
	if err := store.migrateRepositorySources(ctx); err != nil {
		t.Fatal(err)
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "git-called")
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\n: > \"$ZOTIGO_TEST_GIT_MARKER\"\nexec \"$ZOTIGO_TEST_REAL_GIT\" \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZOTIGO_TEST_GIT_MARKER", marker)
	t.Setenv("ZOTIGO_TEST_REAL_GIT", git)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := store.migrateRepositorySources(ctx); err != nil {
		t.Fatal(err)
	}
	root := store.RootDir()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// A missing original path cannot prevent catalog startup either.
	if err := os.Rename(source.CanonicalPath, source.CanonicalPath+"-missing"); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.ListRepositories(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("unexpected Git scan: %v", err)
	}
}

func TestRepositoryMaintenanceRefreshesCachedRemote(t *testing.T) {
	ctx := context.Background()
	store, remote := repositoryFixture(t)
	repo, err := createReadyRepository(store, ctx, remote, "cache")
	if err != nil {
		t.Fatal(err)
	}
	runGitProvisionCommand(t, remote, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "new")
	want := strings.TrimSpace(runGitProvisionCommand(t, remote, "rev-parse", "HEAD"))
	if _, err := store.db.Exec(`UPDATE repositories SET last_synced_at=? WHERE id=?`, unixMillis(time.Now().Add(-time.Hour)), repo.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.maintainRepositories(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(runGitProvisionCommand(t, repo.CanonicalPath, "rev-parse", "refs/remotes/origin/trunk")); got != want {
		t.Fatalf("background cache=%s want %s", got, want)
	}
}

func TestRepositoryDefaultBackupSurvivesPartialRefresh(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Git wrapper uses POSIX shell")
	}
	ctx := context.Background()
	store, remote := repositoryFixture(t)
	runGitProvisionCommand(t, remote, "branch", "-m", "origin/trunk")
	repo, err := createReadyRepository(store, ctx, remote, "cache")
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, "Project")
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.AddRepositorySource(ctx, project.ID, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := repo.defaultCommit
	runGitProvisionCommand(t, remote, "checkout", "--orphan", "replacement")
	runGitProvisionCommand(t, remote, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "unrelated default")
	runGitProvisionCommand(t, remote, "branch", "-D", "origin/trunk")
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\nif [ \"$3\" = ls-remote ]; then exit 1; fi\nexec \"$ZOTIGO_TEST_REAL_GIT\" \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZOTIGO_TEST_REAL_GIT", git)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, base := range []string{"", "HEAD", "origin/trunk", "origin/origin/trunk", "refs/heads/origin/trunk", "refs/remotes/origin/origin/trunk"} {
		workspace, err := store.CreateWorkspacePlan(ctx, project.ID, "Cached default", []WorkspaceSourceInput{{SourceID: source.ID, BaseRef: base}})
		if err != nil {
			t.Fatalf("default %q: %v", base, err)
		}
		bindings, err := store.ListWorkspaceSources(ctx, workspace.ID)
		if err != nil || len(bindings) != 1 || bindings[0].BaseCommit != want || !bindings[0].UsedCachedBase {
			t.Fatalf("default %q: %+v %v", base, bindings, err)
		}
	}
	if _, err := runGit(ctx, repo.CanonicalPath, "rev-parse", "--verify", "refs/remotes/origin/origin/trunk"); err == nil {
		t.Fatal("test did not prune original remote branch")
	}
	runGitProvisionCommand(t, repo.CanonicalPath, "reflog", "expire", "--expire=now", "--all")
	runGitProvisionCommand(t, repo.CanonicalPath, "gc", "--prune=now")
	if got := strings.TrimSpace(runGitProvisionCommand(t, repo.CanonicalPath, "rev-parse", "refs/zotigo/default-cache/"+want)); got != want {
		t.Fatal("successful default backup was collected")
	}
}

func TestRepositoryMaintenanceLifecycleWakesAndRefreshes(t *testing.T) {
	ctx := context.Background()
	store, remote := repositoryFixture(t)
	repo, err := createReadyRepository(store, ctx, remote, "cache")
	if err != nil {
		t.Fatal(err)
	}
	store.startRepositoryMaintenance(ctx, 20*time.Millisecond, nil)
	wait := func(check func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if check() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("background maintenance did not finish")
	}
	local := filepath.Join(t.TempDir(), "local")
	runGitProvisionCommand(t, remote, "clone", remote, local)
	project, err := store.CreateProject(ctx, "New local")
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := InspectSource(ctx, local)
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.AddSource(ctx, project.ID, SourceInput{Kind: inspection.Kind, CanonicalPath: inspection.CanonicalPath, GitCommonDir: inspection.GitCommonDir, GitObjectFormat: inspection.GitObjectFormat, SourceKey: inspection.SourceKey})
	if err != nil {
		t.Fatal(err)
	}
	if source.RepositoryID != "" {
		t.Fatal("local add waited for migration")
	}
	wait(func() bool {
		sources, err := store.ListSources(ctx, project.ID)
		return err == nil && len(sources) == 1 && sources[0].RepositoryID == repo.ID
	})
	runGitProvisionCommand(t, remote, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "periodic")
	want := strings.TrimSpace(runGitProvisionCommand(t, remote, "rev-parse", "HEAD"))
	if _, err := store.db.Exec(`UPDATE repositories SET last_synced_at=? WHERE id=?`, unixMillis(time.Now().Add(-time.Hour)), repo.ID); err != nil {
		t.Fatal(err)
	}
	wait(func() bool { r, err := store.GetRepository(ctx, repo.ID); return err == nil && r.defaultCommit == want })
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-store.maintenanceDone:
	default:
		t.Fatal("Close left background maintenance running")
	}
}

func TestWorkspaceCreationSurvivesConcurrentLocalMigration(t *testing.T) {
	ctx := context.Background()
	store, legacy, local := createGitWorkspaceFixture(t)
	localRemote := filepath.Join(t.TempDir(), "local-remote.git")
	runGitProvisionCommand(t, local.CanonicalPath, "clone", "--bare", local.CanonicalPath, localRemote)
	runGitProvisionCommand(t, local.CanonicalPath, "remote", "add", "origin", localRemote)
	// A local-only commit distinguishes the already-started local request
	// from a later request using the managed remote's default branch.
	runGitProvisionCommand(t, local.CanonicalPath, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "local only")
	want := strings.TrimSpace(runGitProvisionCommand(t, local.CanonicalPath, "rev-parse", "HEAD"))
	other := filepath.Join(t.TempDir(), "other")
	runGitProvisionCommand(t, local.CanonicalPath, "clone", localRemote, other)
	repo, err := createReadyRepository(store, ctx, other, "other-cache")
	if err != nil {
		t.Fatal(err)
	}
	managed, err := store.AddRepositorySource(ctx, legacy.ProjectID, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	wait, release := blockRepositoryFetchAt(t, repo.CanonicalPath)
	type result struct {
		workspace Workspace
		err       error
	}
	done := make(chan result, 1)
	go func() {
		workspace, err := store.CreateWorkspacePlan(ctx, legacy.ProjectID, "Concurrent", []WorkspaceSourceInput{{SourceID: local.ID}, {SourceID: managed.ID}})
		done <- result{workspace, err}
	}()
	wait()
	if err := store.migrateRepositorySources(ctx); err != nil {
		release()
		t.Fatal(err)
	}
	release()
	created := <-done
	if created.err != nil {
		t.Fatal(created.err)
	}
	bindings, err := store.ListWorkspaceSources(ctx, created.workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, binding := range bindings {
		if binding.Source.ID == local.ID {
			found = true
			if binding.BaseCommit != want {
				t.Fatalf("local request changed base: %s want %s", binding.BaseCommit, want)
			}
		}
	}
	if !found {
		t.Fatal("already-started local selection changed identity")
	}
	if _, err := store.ProvisionWorkspace(ctx, created.workspace.ID); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteSourceResolvesMigratedOriginalID(t *testing.T) {
	ctx := context.Background()
	store, legacy, local := createGitWorkspaceFixture(t)
	prepared, err := store.prepareRepositorySelection(ctx, legacy.ProjectID, WorkspaceSourceInput{SourceID: local.ID})
	if err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGitProvisionCommand(t, local.CanonicalPath, "clone", "--bare", local.CanonicalPath, remote)
	runGitProvisionCommand(t, local.CanonicalPath, "remote", "add", "origin", remote)
	if err := store.migrateRepositorySources(ctx); err != nil {
		t.Fatal(err)
	}
	sources, err := store.ListSources(ctx, legacy.ProjectID)
	if err != nil || len(sources) != 1 || sources[0].ID == local.ID {
		t.Fatalf("migration=%+v %v", sources, err)
	}
	managedID := sources[0].ID
	if err := store.DeleteSource(ctx, legacy.ProjectID, local.ID); err != nil {
		t.Fatal(err)
	}
	sources, err = store.ListSources(ctx, legacy.ProjectID)
	if err != nil || len(sources) != 0 {
		t.Fatalf("stale ID left managed source registered: %+v %v", sources, err)
	}
	bindings, err := store.ListWorkspaceSources(ctx, legacy.ID)
	if err != nil || len(bindings) != 1 || bindings[0].Source.ID != local.ID || bindings[0].Source.CanonicalPath != local.CanonicalPath {
		t.Fatalf("deleted existing workspace binding: %+v %v", bindings, err)
	}
	if _, err := store.CreateWorkspacePlan(ctx, legacy.ProjectID, "Deleted", []WorkspaceSourceInput{{SourceID: local.ID}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("deleted replacement accepted: %v", err)
	}
	empty, err := store.CreateWorkspacePlan(ctx, legacy.ProjectID, "Prepared before deletion", nil)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	planErr := store.planWorkspaceSource(ctx, tx, empty, prepared)
	_ = tx.Rollback()
	if !errors.Is(planErr, ErrInvalid) {
		t.Fatalf("prepared local source bypassed replacement deletion: %v", planErr)
	}
	readded, err := store.AddSource(ctx, legacy.ProjectID, SourceInput{Kind: local.Kind, CanonicalPath: local.CanonicalPath, GitCommonDir: local.GitCommonDir, GitObjectFormat: local.GitObjectFormat, SourceKey: local.SourceKey})
	if err != nil || readded.ID != managedID {
		t.Fatalf("re-registration=%+v %v", readded, err)
	}
	if _, err := store.CreateWorkspacePlan(ctx, legacy.ProjectID, "Restored", []WorkspaceSourceInput{{SourceID: local.ID}}); err != nil {
		t.Fatal(err)
	}
}
