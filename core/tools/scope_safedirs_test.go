package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func pathArg(t *testing.T, p string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"path": p})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A skill root is registered as an extra safe dir outside the working
// directory. These cases pin what that grants to read-style checks.
func TestIsInSafeScope_ExtraSafeDir(t *testing.T) {
	base := t.TempDir()
	workDir := filepath.Join(base, "proj")
	skillRoot := filepath.Join(base, "skills")
	other := filepath.Join(base, "other")
	sibling := skillRoot + "-private"
	for _, dir := range []string{workDir, filepath.Join(skillRoot, "demo"), other, sibling} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{
		filepath.Join(skillRoot, "demo", "SKILL.md"),
		filepath.Join(other, "file"),
		filepath.Join(sibling, "file"),
	} {
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	missingRoot := filepath.Join(base, "not-created-yet")

	tests := []struct {
		name     string
		path     string
		safeDirs []string
		want     bool
	}{
		{"regular file under the skill root", filepath.Join(skillRoot, "demo", "SKILL.md"), []string{workDir, skillRoot}, true},
		{"the skill root itself", skillRoot, []string{workDir, skillRoot}, true},
		{"skill root given with a trailing separator", filepath.Join(skillRoot, "demo", "SKILL.md"), []string{workDir, skillRoot + string(filepath.Separator)}, true},
		{"same file when the skill root is not registered", filepath.Join(skillRoot, "demo", "SKILL.md"), []string{workDir}, false},
		{"dot-dot leaves the skill root", skillRoot + "/../other/file", []string{workDir, skillRoot}, false},
		{"dot-dot from a nested skill dir leaves the root", skillRoot + "/demo/../../other/file", []string{workDir, skillRoot}, false},
		{"dot-dot that stays inside the skill root", skillRoot + "/demo/../demo/SKILL.md", []string{workDir, skillRoot}, true},
		{"sibling directory sharing the root's name prefix", filepath.Join(sibling, "file"), []string{workDir, skillRoot}, false},
		{"relative path resolves against the working directory, not the skill root", "demo/SKILL.md", []string{workDir, skillRoot}, true},
		{"relative path climbing from the working directory into the skill root", "../skills/demo/SKILL.md", []string{workDir, skillRoot}, true},
		{"relative path climbing from the working directory elsewhere", "../other/file", []string{workDir, skillRoot}, false},
		{"path under a safe dir that does not exist on disk", filepath.Join(missingRoot, "demo", "SKILL.md"), []string{workDir, missingRoot}, true},
		{"dot-dot out of a safe dir that does not exist on disk", missingRoot + "/../other/file", []string{workDir, missingRoot}, false},
		{"a missing safe dir grants nothing outside itself", filepath.Join(other, "file"), []string{workDir, missingRoot}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			call := SafetyCall{Arguments: pathArg(t, tt.path), WorkDir: workDir, SafeDirs: tt.safeDirs}
			if got := IsInSafeScope(call, []string{"path"}); got != tt.want {
				t.Fatalf("IsInSafeScope(%q, safeDirs=%v) = %v, want %v", tt.path, tt.safeDirs, got, tt.want)
			}
		})
	}
}

// Extra safe dirs are for reads only: the mutator template never consults
// them, so a write under a skill root is classified like any other write
// outside the working directory.
func TestScopeTemplates_ExtraSafeDir(t *testing.T) {
	base := t.TempDir()
	workDir := filepath.Join(base, "proj")
	skillRoot := filepath.Join(base, "skills")
	safeDirs := []string{workDir, skillRoot}

	tests := []struct {
		name      string
		path      string
		wantRead  SafetyLevel
		wantWrite SafetyLevel
	}{
		{"regular file under the skill root", filepath.Join(skillRoot, "demo", "SKILL.md"), LevelSafe, LevelMedium},
		{"dotenv under the skill root", filepath.Join(skillRoot, "demo", ".env"), LevelMedium, LevelMedium},
		{"private key under the skill root", filepath.Join(skillRoot, "demo", "id_rsa"), LevelMedium, LevelMedium},
		{"VCS internals under the skill root", filepath.Join(skillRoot, "demo", ".git", "config"), LevelMedium, LevelMedium},
		{"dot-dot out of the skill root", skillRoot + "/../other/file", LevelMedium, LevelMedium},
		{"file in the working directory", filepath.Join(workDir, "main.go"), LevelSafe, LevelLow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			call := SafetyCall{Arguments: pathArg(t, tt.path), WorkDir: workDir, SafeDirs: safeDirs}
			if got := ReadOnlyScope("path")(call); got.Level != tt.wantRead {
				t.Errorf("ReadOnlyScope level = %s, want %s", got.Level, tt.wantRead)
			}
			got := MutatorScope("path")(call)
			if got.Level != tt.wantWrite {
				t.Errorf("MutatorScope level = %s, want %s", got.Level, tt.wantWrite)
			}
			if !got.RequiresSnapshot {
				t.Error("MutatorScope must request a snapshot")
			}
		})
	}
}
