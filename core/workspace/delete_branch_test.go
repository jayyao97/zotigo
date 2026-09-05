package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDeleteWorkspaceIgnoresCurrentBranch(t *testing.T) {
	for _, mode := range []string{"switched", "detached", "original-missing"} {
		t.Run(mode, func(t *testing.T) {
			store, workspace, source := createGitWorkspaceFixture(t)
			ctx := context.Background()
			path := filepath.Join(workspace.RootPath, "code", workspaceSourceName(source))
			if mode == "detached" {
				runGitProvisionCommand(t, path, "checkout", "--detach")
			} else {
				runGitProvisionCommand(t, path, "checkout", "-b", "user-branch")
			}
			if mode == "original-missing" {
				runGitProvisionCommand(t, source.CanonicalPath, "branch", "-D", "zotigo/test-workspace")
			}
			before := runGitProvisionCommand(t, source.CanonicalPath, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads", "refs/remotes")
			impact, err := store.PreviewDelete(ctx, workspace.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !impact.PreservesLocalBranches || len(impact.LocalBranches) != 0 {
				t.Fatalf("impact = %+v", impact)
			}
			if err := store.DeleteWorkspace(ctx, workspace.ID, workspace.Title); err != nil {
				t.Fatal(err)
			}
			after := runGitProvisionCommand(t, source.CanonicalPath, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads", "refs/remotes")
			if before != after {
				t.Fatalf("branch refs changed: before %s after %s", before, after)
			}
		})
	}
}

func TestDeleteWorkspaceRetryAfterCheckoutRemoval(t *testing.T) {
	for _, removeOwnership := range []bool{false, true} {
		store, workspace, source := createGitWorkspaceFixture(t)
		ctx := context.Background()
		checkouts, _, err := store.workspaceBindings(ctx, workspace.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.setWorkspaceStatus(ctx, workspace.ID, WorkspaceStatusDeleting, ""); err != nil {
			t.Fatal(err)
		}
		if err := removeCheckout(ctx, source, checkouts[0], true); err != nil {
			t.Fatal(err)
		}
		if removeOwnership {
			runGitProvisionCommand(t, source.CanonicalPath, "update-ref", "-d", checkoutOwnershipRef(workspace.ID, source.SourceKey))
		}
		if _, err := store.PreviewDelete(ctx, workspace.ID); err != nil {
			t.Fatal(err)
		}
		if err := store.DeleteWorkspace(ctx, workspace.ID, workspace.Title); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDeleteWorkspacePreflightProtectsCheckoutOnInvalidRoot(t *testing.T) {
	store, workspace, source := createGitWorkspaceFixture(t)
	path := filepath.Join(workspace.RootPath, "code", workspaceSourceName(source))
	if err := os.Remove(filepath.Join(workspace.RootPath, ownerMarkerName)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PreviewDelete(context.Background(), workspace.ID); err == nil {
		t.Fatal("preview accepted missing owner")
	}
	if err := store.DeleteWorkspace(context.Background(), workspace.ID, workspace.Title); err == nil {
		t.Fatal("delete accepted missing owner")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("checkout removed before preflight: %v", err)
	}
}

func TestDeleteWorkspaceRejectsSymlinkCheckout(t *testing.T) {
	store, workspace, source := createGitWorkspaceFixture(t)
	path := filepath.Join(workspace.RootPath, "code", workspaceSourceName(source))
	relocated := filepath.Join(t.TempDir(), "checkout")
	if err := os.Rename(path, relocated); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(relocated, path); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteWorkspace(context.Background(), workspace.ID, workspace.Title); !errors.Is(err, ErrConflict) {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(relocated, "README.md")); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteWorkspacePreflightsAllCheckouts(t *testing.T) {
	store, workspace, source := createGitWorkspaceFixture(t)
	ctx := context.Background()
	repository := filepath.Join(t.TempDir(), "second")
	runGitProvisionCommand(t, filepath.Dir(repository), "clone", source.CanonicalPath, repository)
	inspection, err := InspectSource(ctx, repository)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.AddSource(ctx, workspace.ProjectID, SourceInput{Kind: SourceKindGit, CanonicalPath: inspection.CanonicalPath, GitCommonDir: inspection.GitCommonDir, GitObjectFormat: inspection.GitObjectFormat, SourceKey: inspection.SourceKey})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddWorkspaceSource(ctx, workspace.ID, WorkspaceSourceInput{SourceID: second.ID}); err != nil {
		t.Fatal(err)
	}
	checkouts, _, err := store.workspaceBindings(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := checkouts[len(checkouts)-1]
	lastSource, err := store.GetSource(ctx, workspace.ProjectID, last.SourceID)
	if err != nil {
		t.Fatal(err)
	}
	runGitProvisionCommand(t, lastSource.CanonicalPath, "update-ref", "-d", checkoutOwnershipRef(workspace.ID, lastSource.SourceKey))
	if _, err := store.PreviewDelete(ctx, workspace.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("preview error = %v", err)
	}
	if err := store.DeleteWorkspace(ctx, workspace.ID, workspace.Title); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete error = %v", err)
	}
	for _, checkout := range checkouts {
		if _, err := os.Stat(filepath.Join(checkout.WorktreePath, "README.md")); err != nil {
			t.Fatalf("preflight removed checkout: %v", err)
		}
	}
	current, err := store.GetWorkspace(ctx, workspace.ID)
	if err != nil || current.Status != WorkspaceStatusReady {
		t.Fatalf("workspace = %+v, err=%v", current, err)
	}
}

func TestDeleteWorkspaceRetryAfterRootMovedToTrash(t *testing.T) {
	store, workspace, source := createGitWorkspaceFixture(t)
	ctx := context.Background()
	checkouts, _, err := store.workspaceBindings(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.setWorkspaceStatus(ctx, workspace.ID, WorkspaceStatusDeleting, ""); err != nil {
		t.Fatal(err)
	}
	if err := removeCheckout(ctx, source, checkouts[0], true); err != nil {
		t.Fatal(err)
	}
	runGitProvisionCommand(t, source.CanonicalPath, "update-ref", "-d", checkoutOwnershipRef(workspace.ID, source.SourceKey))
	trash := filepath.Join(filepath.Dir(workspace.RootPath), ".trash-"+workspace.ID)
	if err := os.Rename(workspace.RootPath, trash); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PreviewDelete(ctx, workspace.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteWorkspace(ctx, workspace.ID, workspace.Title); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(trash); !os.IsNotExist(err) {
		t.Fatalf("trash remains: %v", err)
	}
}
