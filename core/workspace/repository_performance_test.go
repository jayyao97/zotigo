package workspace

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRepositoryMaintenanceSkipsUnusedLibraryEntries(t *testing.T) {
	ctx := context.Background()
	store, remote := repositoryFixture(t)
	repo, err := createReadyRepository(store, ctx, remote, "used")
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, "Active")
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.AddRepositorySource(ctx, project.ID, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A large idle library must not create Git processes or touch missing remotes.
	for i := range 40 {
		idle, err := store.CreateRepository(ctx, filepath.Join(t.TempDir(), fmt.Sprintf("missing-%d.git", i)), "idle")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`UPDATE repositories SET last_synced_at=? WHERE id=?`, unixMillis(time.Now().Add(-time.Hour)), idle.ID); err != nil {
			t.Fatal(err)
		}
	}
	runGitProvisionCommand(t, remote, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "new")
	old := strings.TrimSpace(runGitProvisionCommand(t, repo.CanonicalPath, "rev-parse", "refs/remotes/origin/trunk"))
	if _, err := store.db.Exec(`UPDATE repositories SET last_synced_at=? WHERE id=?`, unixMillis(time.Now().Add(-time.Hour)), repo.ID); err != nil {
		t.Fatal(err)
	}
	// Inactive projects do not refresh even when their source remains registered.
	if _, err := store.db.Exec(`UPDATE projects SET status='archived' WHERE id=?`, project.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.maintainRepositories(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(runGitProvisionCommand(t, repo.CanonicalPath, "rev-parse", "refs/remotes/origin/trunk")); got != old {
		t.Fatal("archived project's cache refreshed")
	}
	if _, err := store.db.Exec(`UPDATE projects SET status='active' WHERE id=?`, project.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.maintainRepositories(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(runGitProvisionCommand(t, repo.CanonicalPath, "rev-parse", "refs/remotes/origin/trunk")); got == old {
		t.Fatal("active project's cache not refreshed")
	}
	if err := store.DeleteSource(ctx, project.ID, source.ID); err != nil {
		t.Fatal(err)
	}
	active, err := store.activeRepositories(ctx)
	if err != nil || len(active) != 0 {
		t.Fatalf("unregistered sources: %v %v", active, err)
	}
}

func TestRepositoryRetryBackoffIsDurableAndCapped(t *testing.T) {
	ctx := context.Background()
	store, _, source := createGitWorkspaceFixture(t)
	repo, err := store.CreateRepository(ctx, filepath.Join(t.TempDir(), "missing.git"), "retry")
	if err != nil {
		t.Fatal(err)
	}
	root := store.RootDir()
	for i, delay := range []time.Duration{10 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour, 6 * time.Hour} {
		before := time.Now()
		if err := store.recordPreparationFailure(ctx, repo.ID, errors.New("network failed")); err != nil {
			t.Fatal(err)
		}
		if err := store.recordMigrationFailure(ctx, source.ID, errors.New("network failed")); err != nil {
			t.Fatal(err)
		}
		for _, query := range []string{
			`SELECT failure_count,next_attempt_at FROM repository_preparations WHERE repository_id='` + repo.ID + `'`,
			`SELECT failure_count,next_attempt_at FROM source_repository_migrations WHERE source_id='` + source.ID + `'`,
		} {
			var count int
			var next int64
			if err := store.db.QueryRow(query).Scan(&count, &next); err != nil {
				t.Fatal(err)
			}
			if count != i+1 || next < unixMillis(before.Add(delay))-1 || next > unixMillis(time.Now().Add(delay)) {
				t.Fatalf("retry %d: count=%d next=%d delay=%v", i, count, next, delay)
			}
		}
		if i == 1 {
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
		}
	}
}

func TestExplicitRepositoryPreparationDoesNotWaitForMigration(t *testing.T) {
	store, remote := repositoryFixture(t)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		http.Error(w, "offline", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	defer close(release)
	local := filepath.Join(t.TempDir(), "old-checkout")
	runGitProvisionCommand(t, remote, "clone", remote, local)
	runGitProvisionCommand(t, local, "remote", "set-url", "origin", server.URL+"/slow.git")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	project, err := store.CreateProject(ctx, "Legacy")
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := InspectSource(ctx, local)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddSource(ctx, project.ID, SourceInput{Kind: inspection.Kind, CanonicalPath: inspection.CanonicalPath, GitCommonDir: inspection.GitCommonDir, GitObjectFormat: inspection.GitObjectFormat, SourceKey: inspection.SourceKey}); err != nil {
		t.Fatal(err)
	}
	store.StartRepositoryMaintenance(ctx, nil)
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("migration clone did not start")
	}
	repo, err := store.CreateRepository(ctx, remote, "explicit")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		repo, err = store.GetRepository(ctx, repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		if repo.LastSyncedAt != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("new repository waited for unrelated migration")
}

func TestSharedRemoteCloneBackoff(t *testing.T) {
	store, remote := repositoryFixture(t)
	attempts := make(chan struct{}, 10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts <- struct{}{}
		http.Error(w, "offline", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	ctx := context.Background()
	for i := range 2 {
		local := filepath.Join(t.TempDir(), "checkout")
		runGitProvisionCommand(t, remote, "clone", remote, local)
		runGitProvisionCommand(t, local, "remote", "set-url", "origin", server.URL+"/same.git")
		project, err := store.CreateProject(ctx, fmt.Sprintf("Project%d", i))
		if err != nil {
			t.Fatal(err)
		}
		inspection, err := InspectSource(ctx, local)
		if err != nil {
			t.Fatal(err)
		}
		_, err = store.AddSource(ctx, project.ID, SourceInput{Kind: inspection.Kind, CanonicalPath: inspection.CanonicalPath, GitCommonDir: inspection.GitCommonDir, GitObjectFormat: inspection.GitObjectFormat, SourceKey: inspection.SourceKey})
		if err != nil {
			t.Fatal(err)
		}
	}
	repo, err := store.CreateRepository(ctx, server.URL+"/same.git", "shared")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.migrateRepositorySources(ctx); err == nil {
		t.Fatal("expected unavailable remote")
	}
	if err := store.prepareRegisteredRepositories(ctx); err == nil {
		t.Fatal("expected shared retry deferral")
	}
	if got := len(attempts); got != 1 {
		t.Fatalf("same remote contacted %d times in one pass; expected one retry budget", got)
	}
	root := store.RootDir()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.refreshRepository(ctx, repo); err == nil {
		t.Fatal("restart lost shared backoff")
	}
	if len(attempts) != 1 {
		t.Fatal("restart repeated clone during cooldown")
	}
	if _, err := reopened.db.Exec(`UPDATE repositories SET clone_next_attempt_at=0 WHERE id=?`, repo.ID); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	if _, err := reopened.refreshRepository(ctx, repo); err == nil {
		t.Fatal("expected next clone failure")
	}
	var failures int
	var next int64
	if err := reopened.db.QueryRow(`SELECT clone_failure_count,clone_next_attempt_at FROM repositories WHERE id=?`, repo.ID).Scan(&failures, &next); err != nil {
		t.Fatal(err)
	}
	if failures != 2 || next < unixMillis(before.Add(30*time.Minute))-1 || len(attempts) != 2 {
		t.Fatalf("shared retry count=%d next=%d requests=%d", failures, next, len(attempts))
	}
}

func TestExplicitQueueSkipsMigrationHeldGate(t *testing.T) {
	store, remote := repositoryFixture(t)
	second := filepath.Join(t.TempDir(), "second.git")
	runGitProvisionCommand(t, remote, "clone", "--bare", remote, second)
	ctx, cancel := context.WithCancel(context.Background())
	a, err := store.CreateRepository(ctx, remote, "migration-held")
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.CreateRepository(ctx, second, "unrelated")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE repository_preparations SET next_attempt_at=-1 WHERE repository_id=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	release, err := store.lockRepository(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- store.prepareRegisteredRepositories(ctx) }()
	defer func() { cancel(); release(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, err := store.GetRepository(ctx, b.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.LastSyncedAt != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("unrelated explicit preparation blocked behind migration-held repository gate")
}
