package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestProvisionWorkspaceScaffoldIsIdempotent(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "Scaffold")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := store.CreateWorkspace(ctx, project.ID, "Empty")
	if err != nil {
		t.Fatal(err)
	}

	ready, err := store.ProvisionWorkspaceScaffold(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ready.Status != WorkspaceStatusReady {
		t.Fatalf("status = %q, want ready", ready.Status)
	}
	for _, name := range []string{ownerMarkerName, "code", "artifacts", "notes"} {
		if _, err := os.Stat(filepath.Join(ready.RootPath, name)); err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
	}
	if _, err := store.ProvisionWorkspaceScaffold(ctx, workspace.ID); err != nil {
		t.Fatalf("idempotent provision: %v", err)
	}
}

func TestProvisionWorkspaceScaffoldRejectsUnknownTarget(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "Conflict")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := store.CreateWorkspace(ctx, project.ID, "Occupied")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workspace.RootPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace.RootPath, "user.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := store.ProvisionWorkspaceScaffold(ctx, workspace.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("provision error = %v, want conflict", err)
	}
	if data, err := os.ReadFile(filepath.Join(workspace.RootPath, "user.txt")); err != nil || string(data) != "keep" {
		t.Fatalf("unknown target was changed: data=%q err=%v", data, err)
	}
	got, err := store.GetWorkspace(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != WorkspaceStatusError || got.Error == "" {
		t.Fatalf("workspace = %+v", got)
	}
}
