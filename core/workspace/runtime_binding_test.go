package workspace

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestRuntimeWorkspaceBindingCreatingToBound(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "Project")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := store.CreateWorkspace(ctx, project.ID, "Workspace")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(workspace.RootPath, "code")

	creating, inserted, err := store.BeginRuntimeWorkspaceBinding(ctx, workspace.ID, "codex", "create-1", workspace.Title, root)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted || creating.State != RuntimeWorkspaceBindingCreating || creating.ExternalID != "" || creating.Revision != 1 {
		t.Fatalf("creating binding = %#v, inserted=%v", creating, inserted)
	}
	replayed, inserted, err := store.BeginRuntimeWorkspaceBinding(ctx, workspace.ID, "codex", "other-key", "Other", "/other")
	if err != nil {
		t.Fatal(err)
	}
	if inserted || replayed.CreateKey != creating.CreateKey || replayed.CreateRoot != creating.CreateRoot {
		t.Fatalf("concurrent begin replaced intent: %#v", replayed)
	}

	bound, err := store.CompleteRuntimeWorkspaceBinding(ctx, creating, "project-codex", "codex-test")
	if err != nil {
		t.Fatal(err)
	}
	if bound.State != RuntimeWorkspaceBindingBound || bound.ExternalID != "project-codex" || bound.Revision != 2 {
		t.Fatalf("bound binding = %#v", bound)
	}
	if _, err := store.CompleteRuntimeWorkspaceBinding(ctx, creating, "project-other", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale complete error = %v, want conflict", err)
	}
}

func TestReuseRuntimeWorkspaceDoesNotOverwriteWinner(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "Project")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := store.CreateWorkspace(ctx, project.ID, "Workspace")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReuseRuntimeWorkspace(ctx, workspace.ID, "codex", "winner", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReuseRuntimeWorkspace(ctx, workspace.ID, "codex", "loser", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("reuse loser error = %v, want conflict", err)
	}
}

func TestRuntimeWorkspaceBindingRebuildAndReplaceAreRevisionFenced(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	project, _ := store.CreateProject(ctx, "Project")
	workspace, _ := store.CreateWorkspace(ctx, project.ID, "Workspace")
	bound, err := store.ReuseRuntimeWorkspace(ctx, workspace.ID, "codex", "deleted", "")
	if err != nil {
		t.Fatal(err)
	}
	replaced, err := store.ReplaceRuntimeWorkspaceBinding(ctx, bound, "found", "")
	if err != nil {
		t.Fatal(err)
	}
	if replaced.ExternalID != "found" || replaced.Revision != bound.Revision+1 {
		t.Fatalf("replaced = %#v", replaced)
	}
	if _, err := store.RebuildRuntimeWorkspaceBinding(ctx, bound, "stale", workspace.Title, filepath.Join(workspace.RootPath, "code")); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale rebuild error = %v, want conflict", err)
	}
	rebuilding, err := store.RebuildRuntimeWorkspaceBinding(ctx, replaced, "create-2", workspace.Title, filepath.Join(workspace.RootPath, "code"))
	if err != nil {
		t.Fatal(err)
	}
	if rebuilding.State != RuntimeWorkspaceBindingCreating || rebuilding.Revision != replaced.Revision+1 {
		t.Fatalf("rebuilding = %#v", rebuilding)
	}
	rotated, err := store.RotateRuntimeWorkspaceCreateKey(ctx, rebuilding, "create-3")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.CreateKey != "create-3" || rotated.Revision != rebuilding.Revision+1 {
		t.Fatalf("rotated = %#v", rotated)
	}
	if _, err := store.RotateRuntimeWorkspaceCreateKey(ctx, rebuilding, "stale"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale rotation error = %v, want conflict", err)
	}
}
