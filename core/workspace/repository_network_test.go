package workspace

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func blockRepositoryFetch(t *testing.T) (wait func(), release func()) {
	return blockRepositoryFetchAt(t, "")
}

func blockRepositoryFetchAt(t *testing.T, directory string) (wait func(), release func()) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test Git wrapper uses POSIX shell")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	started, released := filepath.Join(dir, "started"), filepath.Join(dir, "released")
	script := "#!/bin/sh\nif [ \"$3\" = fetch ] && { [ -z \"$ZOTIGO_TEST_FETCH_DIRECTORY\" ] || [ \"$2\" = \"$ZOTIGO_TEST_FETCH_DIRECTORY\" ]; }; then\n : > \"$ZOTIGO_TEST_FETCH_STARTED\"\n while [ ! -f \"$ZOTIGO_TEST_FETCH_RELEASED\" ]; do sleep 0.01; done\nfi\nexec \"$ZOTIGO_TEST_REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZOTIGO_TEST_REAL_GIT", git)
	t.Setenv("ZOTIGO_TEST_FETCH_DIRECTORY", directory)
	t.Setenv("ZOTIGO_TEST_FETCH_STARTED", started)
	t.Setenv("ZOTIGO_TEST_FETCH_RELEASED", released)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	release = func() { _ = os.WriteFile(released, nil, 0600) }
	t.Cleanup(release)
	wait = func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(started); err == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("fetch did not start")
	}
	return wait, release
}

func TestRepositoryNetworkRechecksCatalogAfterFetch(t *testing.T) {
	for _, action := range []string{"delete-source", "archive-project", "archive-workspace", "duplicate"} {
		t.Run(action, func(t *testing.T) {
			store, remote := repositoryFixture(t)
			ctx := context.Background()
			repo, err := store.CreateRepository(ctx, remote, "cache")
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
			workspace, err := store.CreateWorkspacePlan(ctx, project.ID, "Empty", nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.ProvisionWorkspace(ctx, workspace.ID); err != nil {
				t.Fatal(err)
			}
			if action == "duplicate" {
				if _, err := store.AddWorkspaceSource(ctx, workspace.ID, WorkspaceSourceInput{SourceID: source.ID}); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(remote, remote+"-offline"); err != nil {
					t.Fatal(err)
				}
				_, err := store.AddWorkspaceSource(ctx, workspace.ID, WorkspaceSourceInput{SourceID: source.ID})
				if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "already bound") {
					t.Fatalf("duplicate fetched before rejection: %v", err)
				}
				return
			}
			wait, release := blockRepositoryFetch(t)
			result := make(chan error, 1)
			go func() {
				if action == "archive-workspace" {
					_, err := store.AddWorkspaceSource(ctx, workspace.ID, WorkspaceSourceInput{SourceID: source.ID})
					result <- err
				} else {
					_, err := store.CreateWorkspacePlan(ctx, project.ID, "Pending", []WorkspaceSourceInput{{SourceID: source.ID}})
					result <- err
				}
			}()
			wait()
			mutation := make(chan error, 1)
			go func() {
				var err error
				switch action {
				case "delete-source":
					err = store.DeleteSource(ctx, project.ID, source.ID)
				case "archive-project":
					_, err = store.ArchiveProject(ctx, project.ID)
				case "archive-workspace":
					_, err = store.ArchiveWorkspace(ctx, workspace.ID)
				}
				mutation <- err
			}()
			select {
			case err := <-mutation:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("fetch holds global operation lock")
			}
			release()
			select {
			case err := <-result:
				if !errors.Is(err, ErrConflict) && !errors.Is(err, ErrInvalid) {
					t.Fatalf("stale catalog accepted: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("operation did not finish")
			}
		})
	}
}

func TestRepositoryCloneDoesNotBlockCatalogOperations(t *testing.T) {
	store, remote := repositoryFixture(t)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		http.Error(w, "test remote unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := store.CreateRepository(ctx, server.URL+"/slow.git", "slow"); result <- err }()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("clone did not reach the remote")
	}
	completed := make(chan error, 1)
	go func() {
		_, _, err := store.CreateCompletionTrigger(context.Background(), CompletionTrigger{ID: "during-clone", SessionID: "s", ExpiresAt: time.Now().Add(time.Hour)})
		if err == nil {
			_, err = store.CreateRepository(context.Background(), remote, "fast")
		}
		completed <- err
	}()
	select {
	case err := <-completed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("slow clone blocked completion trigger or another repository")
	}
	waitCtx, stop := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer stop()
	if _, err := store.CreateRepository(waitCtx, server.URL+"/slow.git", "slow"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting for same repository did not cancel: %v", err)
	}
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("cancelled clone succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("clone did not stop after cancellation")
	}
}

func TestRepositoryPreparationPinsCommitAndRechecksSource(t *testing.T) {
	store, remote := repositoryFixture(t)
	ctx := context.Background()
	repo, err := store.CreateRepository(ctx, remote, "cache")
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
	prepared, err := store.prepareRepositorySelection(ctx, project.ID, WorkspaceSourceInput{SourceID: source.ID})
	if err != nil {
		t.Fatal(err)
	}
	runGitProvisionCommand(t, remote, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "advance")
	if _, err := store.refreshRepository(ctx, repo); err != nil {
		t.Fatal(err)
	}
	workspace, err := store.CreateWorkspacePlan(ctx, project.ID, "Empty", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.planWorkspaceSource(ctx, store.db, workspace, prepared); err != nil {
		t.Fatal(err)
	}
	bindings, err := store.ListWorkspaceSources(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bindings[0].BaseCommit != prepared.resolvedBaseCommit {
		t.Fatal("concurrent refresh changed pinned commit")
	}
	if err := store.DeleteSource(ctx, project.ID, source.ID); err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateWorkspacePlan(ctx, project.ID, "Other", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.planWorkspaceSource(ctx, store.db, other, prepared); !errors.Is(err, ErrInvalid) {
		t.Fatalf("deleted source accepted: %v", err)
	}
}

func TestRepositoryBaseNamedOriginIsNotTreatedAsAlias(t *testing.T) {
	store, remote := repositoryFixture(t)
	runGitProvisionCommand(t, remote, "checkout", "-b", "origin/topic")
	runGitProvisionCommand(t, remote, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "topic")
	ctx := context.Background()
	repo, err := store.CreateRepository(ctx, remote, "cache")
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
	prepared, err := store.prepareRepositorySelection(ctx, project.ID, WorkspaceSourceInput{SourceID: source.ID, BaseRef: "origin/topic"})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.BaseRef != "refs/remotes/origin/origin/topic" {
		t.Fatalf("actual branch was rewritten: %s", prepared.BaseRef)
	}
}

func TestMigratedRepositoryCloneDissociatesReference(t *testing.T) {
	store, remote := repositoryFixture(t)
	local := filepath.Join(t.TempDir(), "local")
	runGitProvisionCommand(t, filepath.Dir(local), "clone", remote, local)
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "Project")
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
	if err := store.migrateRepositorySources(ctx); err != nil {
		t.Fatal(err)
	}
	sources, err := store.ListSources(ctx, project.ID)
	if err != nil || len(sources) != 1 {
		t.Fatalf("sources=%+v %v", sources, err)
	}
	source = sources[0]
	_, err = store.CreateWorkspacePlan(ctx, project.ID, "Managed", []WorkspaceSourceInput{{SourceID: source.ID}})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := store.GetRepository(ctx, source.RepositoryID)
	if err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(filepath.Join(repo.CanonicalPath, ".git", "objects", "info", "alternates")); err == nil && strings.TrimSpace(string(contents)) != "" {
		t.Fatalf("managed clone still depends on original object store: %s", contents)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Rename(local, local+"-removed"); err != nil {
		t.Fatal(err)
	}
	runGitProvisionCommand(t, repo.CanonicalPath, "fsck", "--full")
}
