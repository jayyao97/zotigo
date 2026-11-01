package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jayyao97/zotigo/internal/testutil/catalogtest"
)

func TestWorkspaceContainingPath(t *testing.T) {
	store, err := Open(catalogtest.NewRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "Lookup")
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateWorkspace(ctx, project.ID, "Main")
	if err != nil {
		t.Fatal(err)
	}
	ready, err := store.ProvisionWorkspaceScaffold(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	backend := filepath.Join(ready.RootPath, "code", "backend")
	if err := os.MkdirAll(backend, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "backend-link")
	if err := os.Symlink(backend, link); err != nil {
		t.Fatal(err)
	}

	for name, path := range map[string]string{
		"root":          ready.RootPath,
		"subdirectory":  backend,
		"unclean path":  ready.RootPath + "/code/../code/backend/",
		"symlinked dir": link,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := store.WorkspaceContainingPath(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			if got.ID != ready.ID {
				t.Fatalf("workspace = %s, want %s", got.ID, ready.ID)
			}
		})
	}

	sibling := ready.RootPath + "-sibling"
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"outside":        t.TempDir(),
		"prefix sibling": sibling,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.WorkspaceContainingPath(ctx, path); !errors.Is(err, ErrNotFound) {
				t.Fatalf("err = %v, want ErrNotFound", err)
			}
		})
	}
	if _, err := store.WorkspaceContainingPath(ctx, "relative/path"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("relative path err = %v, want ErrInvalid", err)
	}

	// Workspace roots are unique and disjoint by construction; an overlapping
	// root can only come from manual catalog edits and must not be resolved
	// by picking one owner.
	if _, err := store.db.ExecContext(ctx, `UPDATE workspaces SET root_path = ? WHERE id = ?`, filepath.Join(ready.RootPath, "code"), ready.ID); err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateWorkspace(ctx, project.ID, "Overlap")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE workspaces SET root_path = ? WHERE id = ?`, ready.RootPath, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.WorkspaceContainingPath(ctx, backend); !errors.Is(err, ErrConflict) {
		t.Fatalf("ambiguous err = %v, want ErrConflict", err)
	}
}
