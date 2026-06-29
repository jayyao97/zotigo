package wiring

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jayyao97/zotigo/core/agent/prompt"
	"github.com/jayyao97/zotigo/core/skills"
)

func TestSystemPromptBuilderIncludesSkillsInDynamicSystemMessage(t *testing.T) {
	workDir := t.TempDir()
	userSkillsDir := filepath.Join(t.TempDir(), "skills")
	agentsDir := filepath.Join(t.TempDir(), "agents-skills")
	if err := os.MkdirAll(filepath.Join(userSkillsDir, "demo-skill"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userSkillsDir, "demo-skill", "SKILL.md"), []byte(`---
name: demo-skill
description: Demo skill
---

Use this skill for tests.
`), 0644); err != nil {
		t.Fatal(err)
	}

	sm := skills.NewSkillManager(workDir, skills.WithUserDir(userSkillsDir), skills.WithAgentsDir(agentsDir))
	if err := sm.Load(); err != nil {
		t.Fatal(err)
	}

	builder := NewSystemPromptBuilder(PromptConfig{
		WorkDir:      workDir,
		SkillManager: sm,
	})
	messages := builder.BuildMessages(prompt.PromptContext{
		WorkDir:  workDir,
		Platform: "linux",
	})

	if len(messages) != 2 {
		t.Fatalf("expected static plus dynamic system messages, got %d", len(messages))
	}
	if strings.Contains(messages[0], "demo-skill") {
		t.Fatalf("skill index should not be merged into static system message: %s", messages[0])
	}
	if !strings.Contains(messages[1], "<environment>") {
		t.Fatalf("dynamic message should include environment section: %s", messages[1])
	}
	if !strings.Contains(messages[1], "<available_skills>") {
		t.Fatalf("dynamic message should include available skills section: %s", messages[1])
	}
	if !strings.Contains(messages[1], "demo-skill") {
		t.Fatalf("dynamic message should include skill index: %s", messages[1])
	}
}
