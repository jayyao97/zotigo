package workspace

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestProvisionWorkspaceFolderBindings(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "Folders")
	if err != nil {
		t.Fatal(err)
	}

	type fixture struct {
		mode   FolderMode
		source Source
	}
	fixtures := make([]fixture, 0, 3)
	for _, mode := range []FolderMode{FolderModeDirect, FolderModeCopy, FolderModeReference} {
		path := t.TempDir()
		if err := os.WriteFile(filepath.Join(path, "content.txt"), []byte(string(mode)), 0o666); err != nil {
			t.Fatal(err)
		}
		inspection, err := InspectSource(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		source, err := store.AddSource(ctx, project.ID, SourceInput{
			Kind:          SourceKindFolder,
			CanonicalPath: inspection.CanonicalPath,
			FolderMode:    mode,
			SourceKey:     inspection.SourceKey,
		})
		if err != nil {
			t.Fatal(err)
		}
		fixtures = append(fixtures, fixture{mode: mode, source: source})
	}
	selections := make([]WorkspaceSourceInput, 0, len(fixtures))
	for _, fixture := range fixtures {
		selections = append(selections, WorkspaceSourceInput{SourceID: fixture.source.ID, Mode: fixture.mode})
	}
	workspace, err := store.CreateWorkspacePlan(ctx, project.ID, "Folder modes", selections)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = store.ProvisionWorkspace(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if workspace.Status != WorkspaceStatusReady {
		t.Fatalf("workspace status = %q", workspace.Status)
	}
	for _, fixture := range fixtures {
		parent := "code"
		if fixture.mode == FolderModeReference {
			parent = "notes"
		}
		target := filepath.Join(workspace.RootPath, parent, fixture.source.SourceKey)
		info, err := os.Lstat(target)
		if err != nil {
			t.Fatal(err)
		}
		if fixture.mode == FolderModeDirect {
			if info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("direct target mode = %v", info.Mode())
			}
			continue
		}
		contentInfo, err := os.Stat(filepath.Join(target, "content.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if fixture.mode == FolderModeReference && contentInfo.Mode().Perm()&0o222 != 0 {
			t.Fatalf("reference file mode = %o, want read-only", contentInfo.Mode().Perm())
		}
	}
	if _, err := store.ProvisionWorkspace(ctx, workspace.ID); err != nil {
		t.Fatalf("idempotent provision: %v", err)
	}
}

func TestProvisionRejectsAbsoluteSymlinkInCopiedBindings(t *testing.T) {
	for _, mode := range []FolderMode{FolderModeCopy, FolderModeReference} {
		t.Run(string(mode), func(t *testing.T) {
			store, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			ctx := context.Background()
			project, err := store.CreateProject(ctx, "Symlink")
			if err != nil {
				t.Fatal(err)
			}
			sourcePath := t.TempDir()
			filePath := filepath.Join(sourcePath, "content.txt")
			if err := os.WriteFile(filePath, []byte("source"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filePath, filepath.Join(sourcePath, "absolute-link")); err != nil {
				t.Fatal(err)
			}
			inspection, err := InspectSource(ctx, sourcePath)
			if err != nil {
				t.Fatal(err)
			}
			source, err := store.AddSource(ctx, project.ID, SourceInput{Kind: SourceKindFolder, CanonicalPath: inspection.CanonicalPath, FolderMode: mode, SourceKey: inspection.SourceKey})
			if err != nil {
				t.Fatal(err)
			}
			workspace, err := store.CreateWorkspacePlan(ctx, project.ID, "Reject symlink", []WorkspaceSourceInput{{SourceID: source.ID, Mode: mode}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.ProvisionWorkspace(ctx, workspace.ID); !errors.Is(err, ErrConflict) {
				t.Fatalf("provision error = %v, want conflict", err)
			}
		})
	}
}

func TestProvisionWorkspaceGitWorktree(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	project, err := store.CreateProject(ctx, "Git")
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
	workspace, err := store.CreateWorkspacePlan(ctx, project.ID, "Git worktree", []WorkspaceSourceInput{{
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
	worktree := filepath.Join(workspace.RootPath, "code", source.SourceKey)
	if data, err := os.ReadFile(filepath.Join(worktree, "README.md")); err != nil || string(data) != "source\n" {
		t.Fatalf("worktree content = %q, err=%v", data, err)
	}
	if branch := strings.TrimSpace(runGitProvisionCommand(t, worktree, "branch", "--show-current")); branch != "zotigo/test-workspace" {
		t.Fatalf("worktree branch = %q", branch)
	}
	if branch := strings.TrimSpace(runGitProvisionCommand(t, repository, "branch", "--show-current")); branch != "main" {
		t.Fatalf("source branch = %q", branch)
	}
}

func TestReadyWorkspaceRetryRecreatesMissingWorktree(t *testing.T) {
	store, workspace, source := createGitWorkspaceFixture(t)
	worktree := filepath.Join(workspace.RootPath, "code", source.SourceKey)
	if err := os.RemoveAll(worktree); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ProvisionWorkspace(context.Background(), workspace.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(worktree, "README.md")); err != nil {
		t.Fatalf("recreated worktree: %v", err)
	}
}

func runGitProvisionCommand(t *testing.T, directory string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = directory
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return string(output)
}
