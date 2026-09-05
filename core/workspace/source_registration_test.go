package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSourceDeregistrationPreservesExistingWorkspace(t *testing.T) {
	for _, archived := range []bool{false, true} {
		name := "ready"
		if archived {
			name = "archived"
		}
		t.Run(name, func(t *testing.T) {
			store, workspace, source := createGitWorkspaceFixture(t)
			ctx := context.Background()
			if archived {
				if _, err := store.ArchiveWorkspace(ctx, workspace.ID); err != nil {
					t.Fatal(err)
				}
			}
			before, err := store.ListWorkspaceSources(ctx, workspace.ID)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := store.DeleteSource(ctx, source.ProjectID, source.ID); err != nil {
					t.Fatal(err)
				}
			}
			if sources, err := store.ListSources(ctx, source.ProjectID); err != nil || len(sources) != 0 {
				t.Fatalf("registered sources = %+v, %v", sources, err)
			}
			after, err := store.ListWorkspaceSources(ctx, workspace.ID)
			if err != nil || len(after) != 1 || after[0].Source.ID != source.ID || after[0].TargetPath != before[0].TargetPath || after[0].Status != before[0].Status {
				t.Fatalf("workspace binding changed: %+v, %v", after, err)
			}
			if _, err := store.CreateWorkspacePlan(ctx, source.ProjectID, "Removed source", []WorkspaceSourceInput{{SourceID: source.ID}}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("new workspace accepted removed source: %v", err)
			}
			if archived {
				if _, err := store.UnarchiveWorkspace(ctx, workspace.ID); err != nil {
					t.Fatalf("restore with removed source: %v", err)
				}
			}
			if _, err := os.Stat(filepath.Join(after[0].TargetPath, "README.md")); err != nil {
				t.Fatalf("existing files unavailable: %v", err)
			}
			if err := store.DeleteWorkspace(ctx, workspace.ID, workspace.Title); err != nil {
				t.Fatalf("delete with removed source: %v", err)
			}
			if _, err := os.Stat(filepath.Join(source.CanonicalPath, "README.md")); err != nil {
				t.Fatalf("original repository unavailable: %v", err)
			}
		})
	}
}

func TestSourceReRegistrationRetainsIdentity(t *testing.T) {
	store, workspace, source := createGitWorkspaceFixture(t)
	ctx := context.Background()
	input := SourceInput{Kind: source.Kind, CanonicalPath: source.CanonicalPath,
		GitCommonDir: source.GitCommonDir, GitObjectFormat: source.GitObjectFormat, SourceKey: source.SourceKey}
	if err := store.DeleteSource(ctx, source.ProjectID, source.ID); err != nil {
		t.Fatal(err)
	}
	changed := input
	changed.GitCommonDir = filepath.Join(t.TempDir(), ".git")
	if _, err := store.AddSource(ctx, source.ProjectID, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("replaced Git identity accepted: %v", err)
	}
	restored, err := store.AddSource(ctx, source.ProjectID, input)
	if err != nil || restored.ID != source.ID || !restored.CreatedAt.Equal(source.CreatedAt) {
		t.Fatalf("restored source = %+v, %v", restored, err)
	}
	if _, err := store.AddSource(ctx, source.ProjectID, input); !errors.Is(err, ErrConflict) {
		t.Fatalf("active duplicate accepted: %v", err)
	}
	if sources, err := store.ListSources(ctx, source.ProjectID); err != nil || len(sources) != 1 {
		t.Fatalf("registered sources = %+v, %v", sources, err)
	}
	if bindings, err := store.ListWorkspaceSources(ctx, workspace.ID); err != nil || len(bindings) != 1 || bindings[0].Source.ID != restored.ID {
		t.Fatalf("existing binding identity changed: %+v, %v", bindings, err)
	}
}

func TestSourceRegistrationMigration(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, "Migration")
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.AddSource(ctx, project.ID, SourceInput{
		Kind: SourceKindFolder, CanonicalPath: t.TempDir(), FolderMode: FolderModeCopy, SourceKey: "migration-folder",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`ALTER TABLE sources DROP COLUMN registered`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE schema_meta SET version = 6`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		store, err = Open(root)
		if err != nil {
			t.Fatal(err)
		}
		sources, err := store.ListSources(ctx, project.ID)
		if err != nil || len(sources) != 1 || sources[0].ID != source.ID {
			t.Fatalf("migrated sources = %+v, %v", sources, err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDeregisteredFolderPreservesBindingsAndRejectsNewBindings(t *testing.T) {
	for _, mode := range []FolderMode{FolderModeDirect, FolderModeReference, FolderModeCopy} {
		t.Run(string(mode), func(t *testing.T) {
			ctx := context.Background()
			store, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			project, err := store.CreateProject(ctx, "Folder")
			if err != nil {
				t.Fatal(err)
			}
			folder := t.TempDir()
			if err := os.WriteFile(filepath.Join(folder, "keep.txt"), []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			inspection, err := InspectSource(ctx, folder)
			if err != nil {
				t.Fatal(err)
			}
			input := SourceInput{Kind: SourceKindFolder, CanonicalPath: inspection.CanonicalPath, FolderMode: mode, SourceKey: inspection.SourceKey}
			source, err := store.AddSource(ctx, project.ID, input)
			if err != nil {
				t.Fatal(err)
			}
			workspace, err := store.CreateWorkspacePlan(ctx, project.ID, "Existing", []WorkspaceSourceInput{{SourceID: source.ID, Mode: mode}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.ProvisionWorkspace(ctx, workspace.ID); err != nil {
				t.Fatal(err)
			}
			if err := store.DeleteSource(ctx, project.ID, source.ID); err != nil {
				t.Fatal(err)
			}
			bindings, err := store.ListWorkspaceSources(ctx, workspace.ID)
			if err != nil || len(bindings) != 1 {
				t.Fatalf("bindings = %+v, %v", bindings, err)
			}
			if data, err := os.ReadFile(filepath.Join(bindings[0].TargetPath, "keep.txt")); err != nil || string(data) != "keep" {
				t.Fatalf("bound files changed: %q, %v", data, err)
			}
			empty, err := store.CreateWorkspace(ctx, project.ID, "Empty")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.ProvisionWorkspace(ctx, empty.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := store.AddWorkspaceSource(ctx, empty.ID, WorkspaceSourceInput{SourceID: source.ID, Mode: mode}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("existing workspace accepted deregistered source: %v", err)
			}
			if restored, err := store.AddSource(ctx, project.ID, input); err != nil || restored.ID != source.ID {
				t.Fatalf("folder re-registration = %+v, %v", restored, err)
			}
		})
	}
}
