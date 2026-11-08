package workspace

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestRepositoryRegistrationReturnsWhileCloneIsBlocked(t *testing.T) {
	store, _ := repositoryFixture(t)
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.StartRepositoryMaintenance(ctx, nil)
	repo, err := store.CreateRepository(ctx, server.URL+"/slow.git", "slow")
	if err != nil || repo.LastSyncedAt != nil || repo.LastSyncError != "" {
		t.Fatalf("registration=%+v %v", repo, err)
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("background clone did not start")
	}
	project, err := store.CreateProject(ctx, "Pending")
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := store.AddRepositorySource(ctx, project.ID, repo.ID)
		result <- err
	}()
	select {
	case err := <-result:
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("pending source=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("attaching a pending repository waited for clone")
	}
	go func() {
		_, err := store.CreateRepository(ctx, server.URL+"/slow.git", "slow")
		result <- err
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("repeated registration waited for clone")
	}
}

func TestRepositoryPreparationPersistsAndRetriesAfterRestart(t *testing.T) {
	ctx := context.Background()
	store, remote := repositoryFixture(t)
	if err := os.Rename(remote, remote+"-offline"); err != nil {
		t.Fatal(err)
	}
	repo, err := store.CreateRepository(ctx, remote, "retry")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.prepareRegisteredRepositories(ctx); err == nil {
		t.Fatal("offline preparation succeeded")
	}
	failed, err := store.GetRepository(ctx, repo.ID)
	if err != nil || failed.LastSyncedAt != nil || failed.LastSyncError == "" {
		t.Fatalf("failure not visible: %+v %v", failed, err)
	}
	var next int64
	if err := store.db.QueryRow(`SELECT next_attempt_at FROM repository_preparations WHERE repository_id=?`, repo.ID).Scan(&next); err != nil || next <= unixMillis(time.Now()) {
		t.Fatalf("retry not deferred: %d %v", next, err)
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
	var persisted int64
	if err := reopened.db.QueryRow(`SELECT next_attempt_at FROM repository_preparations WHERE repository_id=?`, repo.ID).Scan(&persisted); err != nil || persisted != next {
		t.Fatalf("retry did not survive restart: %d %v", persisted, err)
	}
	// A repeated registration requests an immediate retry instead of making the
	// caller wait for a long clone or for the backoff interval to elapse.
	pending, err := reopened.CreateRepository(ctx, remote, "retry")
	if err != nil || pending.ID != repo.ID || pending.LastSyncError != "" {
		t.Fatalf("retry registration=%+v %v", pending, err)
	}
	reopened.StartRepositoryMaintenance(ctx, nil)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ready, err := reopened.GetRepository(ctx, repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		if ready.LastSyncedAt != nil {
			if ready.DefaultBranch != "trunk" || ready.LastSyncError != "" {
				t.Fatalf("prepared=%+v", ready)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("persisted preparation did not complete after restart")
}

func TestRepositoryPendingPreparationResumesOnStartup(t *testing.T) {
	ctx := context.Background()
	store, remote := repositoryFixture(t)
	repo, err := store.CreateRepository(ctx, remote, "pending")
	if err != nil {
		t.Fatal(err)
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
	reopened.StartRepositoryMaintenance(ctx, nil)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ready, err := reopened.GetRepository(ctx, repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		if ready.LastSyncedAt != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("pending registration did not resume without another request")
}
