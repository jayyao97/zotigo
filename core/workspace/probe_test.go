package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestInspectSourceClassifiesFolderAndGitSubdirectory(t *testing.T) {
	folder := t.TempDir()
	inspection, err := InspectSource(context.Background(), folder)
	if err != nil {
		t.Fatal(err)
	}
	canonicalFolder, err := filepath.EvalSymlinks(folder)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Kind != SourceKindFolder || inspection.CanonicalPath != canonicalFolder || inspection.SourceKey == "" {
		t.Fatalf("folder inspection = %+v", inspection)
	}

	repository := t.TempDir()
	runGitTestCommand(t, repository, "init", "-b", "main")
	subdirectory := filepath.Join(repository, "nested")
	if err := os.Mkdir(subdirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	inspection, err = InspectSource(context.Background(), subdirectory)
	if err != nil {
		t.Fatal(err)
	}
	canonicalRepository, err := filepath.EvalSymlinks(repository)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Kind != SourceKindGit || inspection.CanonicalPath != canonicalRepository {
		t.Fatalf("git inspection = %+v", inspection)
	}
	wantCommonDir := filepath.Join(canonicalRepository, ".git")
	if inspection.GitCommonDir != wantCommonDir || inspection.GitObjectFormat != "sha1" {
		t.Fatalf("git inspection = %+v, want common dir %q", inspection, wantCommonDir)
	}
}

func TestInspectSourceIgnoresInheritedGitDirectory(t *testing.T) {
	repository := t.TempDir()
	runGitTestCommand(t, repository, "init", "-b", "main")
	folder := t.TempDir()
	t.Setenv("GIT_DIR", filepath.Join(repository, ".git"))
	inspection, err := InspectSource(context.Background(), folder)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Kind != SourceKindFolder {
		t.Fatalf("inspection kind = %q, want folder", inspection.Kind)
	}
}

func runGitTestCommand(t *testing.T, directory string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = directory
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}
