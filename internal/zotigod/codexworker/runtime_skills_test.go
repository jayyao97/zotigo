package codexworker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexRuntimeBuildsTextAndStructuredSkillInputs(t *testing.T) {
	workspace := t.TempDir()
	path := writeDaemonTestSkill(t, filepath.Join(workspace, ".agents", "skills"), "review-taste", "review", "review carefully")
	runtime := &Runtime{cfg: Config{WorkingDirectory: workspace}}
	inputs, err := runtime.codexInputs("review this", nil, []string{"review-taste"})
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 2 || inputs[0]["type"] != "text" || inputs[0]["text"] != "$review-taste review this" {
		t.Fatalf("Codex inputs = %#v", inputs)
	}
	if inputs[1]["type"] != "skill" || inputs[1]["name"] != "review-taste" || inputs[1]["path"] != path {
		t.Fatalf("Codex skill input = %#v", inputs[1])
	}
	inputs, err = runtime.codexInputs("$review-taste review this", nil, []string{"review-taste"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(inputs[0]["text"].(string), "$review-taste") != 1 {
		t.Fatalf("Codex mention duplicated: %#v", inputs[0])
	}
}

func TestCodexRuntimeMaterializesBuiltinSkill(t *testing.T) {
	runtime := &Runtime{cfg: Config{WorkingDirectory: t.TempDir()}}
	inputs, err := runtime.codexInputs("create a skill", nil, []string{"skill-creator"})
	if err != nil {
		t.Fatal(err)
	}
	path, _ := inputs[1]["path"].(string)
	if !filepath.IsAbs(path) || filepath.Base(path) != "SKILL.md" {
		t.Fatalf("builtin skill path = %q", path)
	}
	if data, readErr := os.ReadFile(path); readErr != nil || !strings.Contains(string(data), "name: skill-creator") {
		t.Fatalf("materialized builtin = %q, err=%v", data, readErr)
	}
	root := runtime.skillTempDir
	if err := runtime.cleanupSkillFiles(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("builtin skill temp dir was not removed: %v", err)
	}
}
