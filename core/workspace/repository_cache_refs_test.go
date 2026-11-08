package workspace

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func defaultCacheRefs(t *testing.T, repo Repository) []string {
	t.Helper()
	output := strings.TrimSpace(runGitProvisionCommand(t, repo.CanonicalPath, "for-each-ref", "--format=%(objectname)", "refs/zotigo/default-cache/"))
	if output == "" {
		return nil
	}
	return strings.Split(output, "\n")
}

func TestRepositoryDefaultCacheReferencesConverge(t *testing.T) {
	ctx := context.Background()
	store, remote := repositoryFixture(t)
	repo, err := createReadyRepository(store, ctx, remote, "cache")
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 5; index++ {
		runGitProvisionCommand(t, remote, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", fmt.Sprint(index))
		repo, err = store.refreshRepository(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		if refs := defaultCacheRefs(t, repo); len(refs) != 1 || refs[0] != repo.defaultCommit {
			t.Fatalf("unbounded cache refs: %v", refs)
		}
	}
	// Simulate a database failure after Git created the candidate ref. The
	// committed old default remains protected and the failed candidate is pruned.
	want := repo.defaultCommit
	if _, err := store.db.Exec(`CREATE TRIGGER fail_default_publish BEFORE UPDATE OF default_commit ON repositories BEGIN SELECT RAISE(FAIL,'publish failed'); END`); err != nil {
		t.Fatal(err)
	}
	runGitProvisionCommand(t, remote, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "not published")
	if _, err := store.refreshRepository(ctx, repo); err == nil {
		t.Fatal("publication unexpectedly succeeded")
	}
	if refs := defaultCacheRefs(t, repo); len(refs) != 1 || refs[0] != want {
		t.Fatalf("lost committed default: %v", refs)
	}
}

func TestRepositoryDefaultCacheReadersAndPlansSurviveCollection(t *testing.T) {
	ctx := context.Background()
	store, remote := repositoryFixture(t)
	repo, err := createReadyRepository(store, ctx, remote, "cache")
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, "Plans")
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.AddRepositorySource(ctx, project.ID, repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := store.CreateWorkspacePlan(ctx, project.ID, "Pinned", []WorkspaceSourceInput{{SourceID: source.ID}})
	if err != nil {
		t.Fatal(err)
	}
	want := repo.defaultCommit
	release := store.holdDefaultCacheRefs(ctx)
	previousBranch := repo.DefaultBranch
	for index := 0; index < 3; index++ {
		runGitProvisionCommand(t, remote, "checkout", "--orphan", fmt.Sprintf("new-%d", index))
		runGitProvisionCommand(t, remote, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", fmt.Sprintf("unrelated %d", index))
		runGitProvisionCommand(t, remote, "branch", "-D", previousBranch)
		previousBranch = fmt.Sprintf("new-%d", index)
		repo, err = store.refreshRepository(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
	}
	if refs := defaultCacheRefs(t, repo); len(refs) != 4 {
		t.Fatalf("reader refs collected prematurely: %v", refs)
	}
	runGitProvisionCommand(t, repo.CanonicalPath, "reflog", "expire", "--expire=now", "--all")
	runGitProvisionCommand(t, repo.CanonicalPath, "gc", "--prune=now")
	for _, commit := range defaultCacheRefs(t, repo) {
		runGitProvisionCommand(t, repo.CanonicalPath, "cat-file", "-e", commit+"^{commit}")
	}
	release()
	if refs := defaultCacheRefs(t, repo); len(refs) != 2 {
		t.Fatalf("reader release did not converge to current + planned: %v", refs)
	}
	runGitProvisionCommand(t, repo.CanonicalPath, "reflog", "expire", "--expire=now", "--all")
	runGitProvisionCommand(t, repo.CanonicalPath, "gc", "--prune=now")
	if _, err := store.ProvisionWorkspace(ctx, plan.ID); err != nil {
		t.Fatal(err)
	}
	bindings, err := store.ListWorkspaceSources(ctx, plan.ID)
	if err != nil || bindings[0].BaseCommit != want {
		t.Fatalf("plan lost its selected commit: %+v %v", bindings, err)
	}
	if _, err := store.refreshRepository(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if refs := defaultCacheRefs(t, repo); len(refs) != 1 || refs[0] != repo.defaultCommit {
		t.Fatalf("ready workspace kept redundant default ref: %v", refs)
	}
}
