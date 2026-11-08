package workspace

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSourceBranchesExceedScalarGitOutputLimit(t *testing.T) {
	store, legacy, source := createGitWorkspaceFixture(t)
	commit := strings.TrimSpace(runGitProvisionCommand(t, source.CanonicalPath, "rev-parse", "HEAD"))
	var refs strings.Builder
	for index := range 1000 {
		fmt.Fprintf(&refs, "create refs/heads/%s-%04d %s\n", strings.Repeat("branch", 12), index, commit)
	}
	cmd := exec.Command("git", "-C", source.CanonicalPath, "update-ref", "--stdin")
	cmd.Stdin = strings.NewReader(refs.String())
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create branches: %s %v", output, err)
	}
	branches, err := store.ListSourceBranches(context.Background(), legacy.ProjectID, source.ID)
	if err != nil || len(branches) < 1000 {
		t.Fatalf("large branch list: count=%d err=%v", len(branches), err)
	}
}

func TestSourceBranchesExposeMigrationSnapshots(t *testing.T) {
	ctx := context.Background()
	store, legacy, original := createGitWorkspaceFixture(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGitProvisionCommand(t, original.CanonicalPath, "clone", "--bare", "--", original.CanonicalPath, remote)
	runGitProvisionCommand(t, original.CanonicalPath, "remote", "add", "origin", remote)
	runGitProvisionCommand(t, original.CanonicalPath, "checkout", "-b", "unpublished/work")
	runGitProvisionCommand(t, original.CanonicalPath, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "unpublished")
	want := strings.TrimSpace(runGitProvisionCommand(t, original.CanonicalPath, "rev-parse", "HEAD"))
	local, err := store.ListSourceBranches(ctx, legacy.ProjectID, original.ID)
	if err != nil || len(local) == 0 {
		t.Fatalf("local branches=%+v %v", local, err)
	}
	if err := store.migrateRepositorySources(ctx); err != nil {
		t.Fatal(err)
	}
	branches, err := store.ListSourceBranches(ctx, legacy.ProjectID, original.ID)
	if err != nil {
		t.Fatal(err)
	}
	var selected RepositoryBranch
	for _, branch := range branches {
		if branch.Kind == "imported" && branch.Name == "unpublished/work" {
			selected = branch
		}
		if strings.Contains(branch.Ref, "default-cache") || strings.HasSuffix(branch.Ref, "/HEAD") {
			t.Fatalf("internal/symbolic ref exposed: %+v", branch)
		}
	}
	if selected.Ref == "" || selected.SourceID != original.ID || selected.SourceName == "" {
		t.Fatalf("missing imported branch: %+v", branches)
	}
	// The snapshot stays selectable without silently following later local work.
	runGitProvisionCommand(t, original.CanonicalPath, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "later local work")
	plan, err := store.CreateWorkspacePlan(ctx, legacy.ProjectID, "Imported", []WorkspaceSourceInput{{SourceID: original.ID, BaseRef: selected.Ref}})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := store.ListWorkspaceSources(ctx, plan.ID)
	if err != nil || bindings[0].BaseCommit != want {
		t.Fatalf("snapshot=%+v %v", bindings, err)
	}
	other, err := store.CreateProject(ctx, "Other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListSourceBranches(ctx, other.ID, original.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-project source accepted: %v", err)
	}
	if err := store.DeleteSource(ctx, legacy.ProjectID, original.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListSourceBranches(ctx, legacy.ProjectID, original.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted source accepted: %v", err)
	}
}
