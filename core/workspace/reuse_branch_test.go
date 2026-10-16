package workspace

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestProvisionReusesDeletedWorkspaceBranch(t *testing.T) {
	store, previous, source := createGitWorkspaceFixture(t)
	ctx := context.Background()
	path := filepath.Join(previous.RootPath, "code", workspaceSourceName(source))
	runGitProvisionCommand(t, path, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "preserve this commit")
	head := strings.TrimSpace(runGitProvisionCommand(t, path, "rev-parse", "HEAD"))
	if err := store.DeleteWorkspace(ctx, previous.ID, previous.Title); err != nil {
		t.Fatal(err)
	}
	next, err := store.CreateWorkspacePlan(ctx, previous.ProjectID, "Reuse", []WorkspaceSourceInput{{SourceID: source.ID, BaseRef: "main", BranchName: "zotigo/test-workspace"}})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := store.ProvisionWorkspace(ctx, next.ID); err != nil {
			t.Fatal(err)
		}
	}
	path = filepath.Join(next.RootPath, "code", workspaceSourceName(source))
	if got := strings.TrimSpace(runGitProvisionCommand(t, path, "rev-parse", "HEAD")); got != head {
		t.Fatalf("reused branch head = %s, want %s", got, head)
	}
	// Simulate interruption after refs are recorded but before worktree creation.
	runGitProvisionCommand(t, source.CanonicalPath, "worktree", "unlock", path)
	runGitProvisionCommand(t, source.CanonicalPath, "worktree", "remove", path)
	if err := store.setCheckoutStatus(ctx, next.ID, source.ID, "error", "interrupted"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ProvisionWorkspace(ctx, next.ID); err != nil {
		t.Fatalf("retry adopted branch: %v", err)
	}
	if err := store.DeleteWorkspace(ctx, next.ID, next.Title); err != nil {
		t.Fatalf("delete adopted workspace: %v", err)
	}
	if got := strings.TrimSpace(runGitProvisionCommand(t, source.CanonicalPath, "rev-parse", "zotigo/test-workspace")); got != head {
		t.Fatalf("delete changed branch head: %s", got)
	}
}

func TestProvisionRejectsBranchOfExistingWorkspace(t *testing.T) {
	for _, archived := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "archived"}[archived], func(t *testing.T) {
			store, previous, source := createGitWorkspaceFixture(t)
			ctx := context.Background()
			if archived {
				if _, err := store.ArchiveWorkspace(ctx, previous.ID); err != nil {
					t.Fatal(err)
				}
			}
			next, err := store.CreateWorkspacePlan(ctx, previous.ProjectID, "Collision", []WorkspaceSourceInput{{SourceID: source.ID, BaseRef: "main", BranchName: "zotigo/test-workspace"}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.ProvisionWorkspace(ctx, next.ID); !errors.Is(err, ErrConflict) {
				t.Fatalf("occupied branch error = %v", err)
			}
		})
	}
}

func TestAddWorkspaceSourceReusesLocalBranch(t *testing.T) {
	store, previous, source := createGitWorkspaceFixture(t)
	ctx := context.Background()
	runGitProvisionCommand(t, source.CanonicalPath, "branch", "existing", "main")
	next, err := store.CreateWorkspace(ctx, previous.ProjectID, "Existing branch")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ProvisionWorkspace(ctx, next.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddWorkspaceSource(ctx, next.ID, WorkspaceSourceInput{
		SourceID: source.ID, BaseRef: "main", BranchName: "existing",
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(next.RootPath, "code", workspaceSourceName(source))
	if got := strings.TrimSpace(runGitProvisionCommand(t, path, "branch", "--show-current")); got != "existing" {
		t.Fatalf("checked out branch = %s", got)
	}
}

func TestUnownedPlansDoNotReserveExistingBranch(t *testing.T) {
	store, previous, source := createGitWorkspaceFixture(t)
	ctx := context.Background()
	runGitProvisionCommand(t, source.CanonicalPath, "branch", "existing", "main")
	var plans []Workspace
	for _, title := range []string{"Failed plan", "Pending plan", "Successful plan"} {
		plan, err := store.CreateWorkspacePlan(ctx, previous.ProjectID, title, []WorkspaceSourceInput{{SourceID: source.ID, BaseRef: "main", BranchName: "existing"}})
		if err != nil {
			t.Fatal(err)
		}
		plans = append(plans, plan)
	}
	if err := store.setWorkspaceStatus(ctx, plans[0].ID, WorkspaceStatusError, "interrupted"); err != nil {
		t.Fatal(err)
	}
	if err := store.setCheckoutStatus(ctx, plans[0].ID, source.ID, "error", "interrupted"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ProvisionWorkspace(ctx, plans[2].ID); err != nil {
		t.Fatalf("unowned plans blocked reuse: %v", err)
	}
	for _, plan := range plans[:2] {
		if _, err := store.ProvisionWorkspace(ctx, plan.ID); !errors.Is(err, ErrConflict) {
			t.Fatalf("provision while branch occupied = %v", err)
		}
	}
}
