package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestArchiveAndUnarchiveWorkspacePreserveBranchAndFiles(t *testing.T) {
	store, workspace, source := createGitWorkspaceFixture(t)
	ctx := context.Background()
	worktree := filepath.Join(workspace.RootPath, "code", source.SourceKey)
	artifact := filepath.Join(workspace.RootPath, "artifacts", "result.txt")
	if err := os.WriteFile(artifact, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirtyPath := filepath.Join(worktree, "dirty.txt")
	if err := os.WriteFile(dirtyPath, []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	impact, err := store.PreviewArchive(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(impact.DirtyWorktreePaths) != 1 {
		t.Fatalf("dirty paths = %v", impact.DirtyWorktreePaths)
	}
	if _, err := store.ArchiveWorkspace(ctx, workspace.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("archive dirty error = %v, want conflict", err)
	}
	if err := os.Remove(dirtyPath); err != nil {
		t.Fatal(err)
	}
	archived, err := store.ArchiveWorkspace(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if archived.Status != WorkspaceStatusArchived || archived.ArchivedAt == nil {
		t.Fatalf("archived workspace = %+v", archived)
	}
	if _, err := os.Stat(worktree); !os.IsNotExist(err) {
		t.Fatalf("worktree still exists: %v", err)
	}
	if data, err := os.ReadFile(artifact); err != nil || string(data) != "keep" {
		t.Fatalf("artifact data = %q, err=%v", data, err)
	}
	if head := strings.TrimSpace(runGitProvisionCommand(t, source.CanonicalPath, "rev-parse", "--verify", "refs/heads/zotigo/test-workspace")); head == "" {
		t.Fatal("archived branch is missing")
	}

	ready, err := store.UnarchiveWorkspace(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ready.Status != WorkspaceStatusReady || ready.ArchivedAt != nil {
		t.Fatalf("unarchived workspace = %+v", ready)
	}
	if _, err := os.Stat(filepath.Join(worktree, "README.md")); err != nil {
		t.Fatalf("recreated worktree: %v", err)
	}
}

func TestDeleteWorkspaceRemovesOwnedRootAndLocalBranchOnly(t *testing.T) {
	store, workspace, source := createGitWorkspaceFixture(t)
	ctx := context.Background()
	worktree := filepath.Join(workspace.RootPath, "code", source.SourceKey)
	if err := os.WriteFile(filepath.Join(worktree, "discard.txt"), []byte("discard"), 0o644); err != nil {
		t.Fatal(err)
	}
	impact, err := store.PreviewDelete(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(impact.DirtyWorktreePaths) != 1 || !impact.PreservesSources || !impact.PreservesSessions || !impact.PreservesRemoteRefs {
		t.Fatalf("delete impact = %+v", impact)
	}
	if err := store.DeleteWorkspace(ctx, workspace.ID, "wrong"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("confirmation error = %v, want invalid", err)
	}
	if err := store.DeleteWorkspace(ctx, workspace.ID, workspace.Title); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(workspace.RootPath); !os.IsNotExist(err) {
		t.Fatalf("workspace root still exists: %v", err)
	}
	if _, err := store.GetWorkspace(ctx, workspace.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted workspace lookup = %v, want not found", err)
	}
	if _, err := store.GetSource(ctx, workspace.ProjectID, source.ID); err != nil {
		t.Fatalf("source was deleted: %v", err)
	}
	if _, err := runGitMutation(ctx, source.CanonicalPath, "rev-parse", "--verify", "refs/heads/zotigo/test-workspace"); err == nil {
		t.Fatal("workspace branch still exists")
	}
	if _, err := os.Stat(source.CanonicalPath); err != nil {
		t.Fatalf("source path was deleted: %v", err)
	}
}

func TestDeleteWorkspaceRejectsSymlinkAncestor(t *testing.T) {
	store, workspace, _ := createGitWorkspaceFixture(t)
	projectDir := filepath.Join(store.RootDir(), "projects", workspace.ProjectID)
	relocated := filepath.Join(store.RootDir(), "relocated-project")
	if err := os.Rename(projectDir, relocated); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(relocated, projectDir); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteWorkspace(context.Background(), workspace.ID, workspace.Title); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete error = %v, want conflict", err)
	}
	if _, err := os.Stat(filepath.Join(workspace.RootPath, ownerMarkerName)); err != nil {
		t.Fatalf("workspace root was removed: %v", err)
	}
}

func TestDeleteWorkspaceRequiresZotigoBranchOwnership(t *testing.T) {
	store, workspace, source := createGitWorkspaceFixture(t)
	ownershipRef := checkoutOwnershipRef(workspace.ID, source.SourceKey)
	runGitProvisionCommand(t, source.CanonicalPath, "update-ref", "-d", ownershipRef)
	if err := store.DeleteWorkspace(context.Background(), workspace.ID, workspace.Title); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete error = %v, want ownership conflict", err)
	}
	if _, err := runGitMutation(context.Background(), source.CanonicalPath, "rev-parse", "--verify", "refs/heads/zotigo/test-workspace"); err != nil {
		t.Fatalf("unowned branch was removed: %v", err)
	}
	if _, err := os.Stat(workspace.RootPath); err != nil {
		t.Fatalf("workspace root was removed: %v", err)
	}
}

func TestArchivedWorkspaceRejectsRecreatedBranchGeneration(t *testing.T) {
	for _, operation := range []string{"unarchive", "delete"} {
		t.Run(operation, func(t *testing.T) {
			store, workspace, source := createGitWorkspaceFixture(t)
			ctx := context.Background()
			if _, err := store.ArchiveWorkspace(ctx, workspace.ID); err != nil {
				t.Fatal(err)
			}
			branchRef := "refs/heads/zotigo/test-workspace"
			oldHead := strings.TrimSpace(runGitProvisionCommand(t, source.CanonicalPath, "rev-parse", "HEAD"))
			if err := os.WriteFile(filepath.Join(source.CanonicalPath, "replacement.txt"), []byte("replacement"), 0o644); err != nil {
				t.Fatal(err)
			}
			runGitProvisionCommand(t, source.CanonicalPath, "add", "replacement.txt")
			runGitProvisionCommand(t, source.CanonicalPath, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "replacement")
			newHead := strings.TrimSpace(runGitProvisionCommand(t, source.CanonicalPath, "rev-parse", "HEAD"))
			if newHead == oldHead {
				t.Fatal("replacement commit did not change")
			}
			runGitProvisionCommand(t, source.CanonicalPath, "update-ref", "-d", branchRef, oldHead)
			runGitProvisionCommand(t, source.CanonicalPath, "update-ref", branchRef, newHead, strings.Repeat("0", len(newHead)))

			var err error
			if operation == "unarchive" {
				_, err = store.UnarchiveWorkspace(ctx, workspace.ID)
			} else {
				err = store.DeleteWorkspace(ctx, workspace.ID, workspace.Title)
			}
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("%s error = %v, want generation conflict", operation, err)
			}
			if head := strings.TrimSpace(runGitProvisionCommand(t, source.CanonicalPath, "rev-parse", branchRef)); head != newHead {
				t.Fatalf("replacement branch head = %q, want %q", head, newHead)
			}
		})
	}
}

func createGitWorkspaceFixture(t *testing.T) (*Store, Workspace, Source) {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "Lifecycle")
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
	workspace, err := store.CreateWorkspacePlan(ctx, project.ID, "Lifecycle workspace", []WorkspaceSourceInput{{
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
	return store, workspace, source
}
