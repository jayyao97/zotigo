package builtin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jayyao97/zotigo/core/tools"
)

func classifyArgs(t *testing.T, args map[string]string) string {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Skill roots are passed to tools as extra SafeDirs. Read tools may open
// plain files there without approval, but not sensitive ones, and not
// paths that climb back out.
func TestReadToolsClassify_SkillRootSafeDir(t *testing.T) {
	base := t.TempDir()
	workDir := filepath.Join(base, "proj")
	skillRoot := filepath.Join(base, "skills")
	for _, dir := range []string{workDir, filepath.Join(skillRoot, "demo", ".git"), filepath.Join(base, "other")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{
		filepath.Join(skillRoot, "demo", "SKILL.md"),
		filepath.Join(skillRoot, "demo", ".env"),
		filepath.Join(skillRoot, "demo", "id_ed25519"),
		filepath.Join(skillRoot, "demo", "credentials.json"),
		filepath.Join(skillRoot, "demo", ".git", "config"),
		filepath.Join(base, "other", "file"),
	} {
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	safeDirs := []string{workDir, skillRoot}

	readers := []struct {
		name    string
		pathKey string
		tool    tools.Tool
	}{
		{"read_file", "path", &ReadFileTool{}},
		{"grep", "path", &GrepTool{}},
		{"glob", "path", &GlobTool{}},
		{"view_image", "path", &ViewImageTool{}},
	}
	paths := []struct {
		name     string
		path     string
		wantSafe bool
	}{
		{"regular file under the skill root", filepath.Join(skillRoot, "demo", "SKILL.md"), true},
		{"dotenv under the skill root", filepath.Join(skillRoot, "demo", ".env"), false},
		{"private key under the skill root", filepath.Join(skillRoot, "demo", "id_ed25519"), false},
		{"credentials file under the skill root", filepath.Join(skillRoot, "demo", "credentials.json"), false},
		{"VCS internals under the skill root", filepath.Join(skillRoot, "demo", ".git", "config"), false},
		{"dot-dot out of the skill root", skillRoot + "/../other/file", false},
	}
	for _, reader := range readers {
		for _, p := range paths {
			t.Run(reader.name+"/"+p.name, func(t *testing.T) {
				args := classifyArgs(t, map[string]string{reader.pathKey: p.path})
				got := reader.tool.Classify(tools.SafetyCall{Arguments: args, WorkDir: workDir, SafeDirs: safeDirs})
				if (got.Level == tools.LevelSafe) != p.wantSafe {
					t.Fatalf("Classify(%s) = %s (%s), want safe=%v", p.path, got.Level, got.Reason, p.wantSafe)
				}
				// Without the skill root registered nothing here is safe.
				got = reader.tool.Classify(tools.SafetyCall{Arguments: args, WorkDir: workDir, SafeDirs: []string{workDir}})
				if got.Level == tools.LevelSafe {
					t.Fatalf("Classify(%s) is safe without the skill root registered", p.path)
				}
			})
		}
	}
}

// Safe dirs must not widen write or shell classification: a mutation under
// a skill root is classified exactly as it would be without the safe dir,
// and never auto-safe / in-workdir Low.
func TestMutatingToolsClassify_SkillRootSafeDirDoesNotWiden(t *testing.T) {
	base := t.TempDir()
	workDir := filepath.Join(base, "proj")
	skillRoot := filepath.Join(base, "skills")
	for _, dir := range []string{workDir, filepath.Join(skillRoot, "demo")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(skillRoot, "demo", "SKILL.md")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	shell, err := NewShellTool(WithPolicy(DefaultShellPolicy()))
	if err != nil {
		t.Fatal(err)
	}
	bareShell, err := NewShellTool()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		tool tools.Tool
		args map[string]string
	}{
		{"write_file under the skill root", &WriteFileTool{}, map[string]string{"path": target, "content": "y"}},
		{"write_file creating a new file under the skill root", &WriteFileTool{}, map[string]string{"path": filepath.Join(skillRoot, "new", "SKILL.md"), "content": "y"}},
		{"edit under the skill root", &EditTool{}, map[string]string{"path": target, "old_string": "x", "new_string": "y"}},
		{"shell touch under the skill root", shell, map[string]string{"command": "touch " + target}},
		{"shell rm under the skill root", shell, map[string]string{"command": "rm " + target}},
		{"shell redirect under the skill root", shell, map[string]string{"command": "echo y > " + target}},
		{"shell sed -i under the skill root", shell, map[string]string{"command": "sed -i s/x/y/ " + target}},
		{"shell with workdir set to the skill root", shell, map[string]string{"command": "touch SKILL.md", "workdir": filepath.Join(skillRoot, "demo")}},
		{"shell without a policy touching the skill root", bareShell, map[string]string{"command": "touch " + target}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := classifyArgs(t, tt.args)
			with := tt.tool.Classify(tools.SafetyCall{Arguments: args, WorkDir: workDir, SafeDirs: []string{workDir, skillRoot}})
			without := tt.tool.Classify(tools.SafetyCall{Arguments: args, WorkDir: workDir, SafeDirs: []string{workDir}})
			if with != without {
				t.Fatalf("safe dir changed the decision: with=%+v without=%+v", with, without)
			}
			if with.Level < tools.LevelMedium {
				t.Fatalf("mutation under a safe dir must stay at Medium or above, got %s (%s)", with.Level, with.Reason)
			}
			if !with.RequiresSnapshot {
				t.Fatalf("mutation under a safe dir must request a snapshot, got %+v", with)
			}
		})
	}
}
