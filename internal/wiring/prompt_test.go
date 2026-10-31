package wiring

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jayyao97/zotigo/core/agent/prompt"
	"github.com/jayyao97/zotigo/core/skills"
)

func TestSessionIdentityPrompt(t *testing.T) {
	for _, tc := range []struct{ session, workspace string }{
		{"sess_parent", "workspace_one"},
		{"sess_fork", "workspace_one"},
		{"sess_directory", ""},
		{"", ""},
	} {
		t.Run(tc.session, func(t *testing.T) {
			builder := NewSystemPromptBuilder(PromptConfig{SessionID: tc.session, WorkspaceID: tc.workspace})
			text := strings.Join(builder.BuildMessages(prompt.PromptContext{}), "\n")
			if tc.session == "" {
				if strings.Contains(text, "<zotigo_context>") {
					t.Fatal("unexpected identity")
				}
				return
			}
			if !strings.Contains(text, "<zotigo_context>") || !strings.Contains(text, "session_id: "+tc.session) {
				t.Fatal("missing current identity")
			}
			if tc.workspace == "" {
				if strings.Contains(text, "workspace_id:") {
					t.Fatal("invented workspace")
				}
			} else if !strings.Contains(text, "workspace_id: "+tc.workspace) {
				t.Fatal("missing workspace")
			}
			if tc.session == "sess_fork" && strings.Contains(text, "sess_parent") {
				t.Fatal("inherited parent identity")
			}
		})
	}
}

func TestPromptBuildersSplitSkillsAndUserContext(t *testing.T) {
	workDir := t.TempDir()
	userSkillsDir := filepath.Join(t.TempDir(), "skills")
	if err := os.WriteFile(filepath.Join(workDir, "AGENTS.md"), []byte("Follow project rules."), 0644); err != nil {
		t.Fatal(err)
	}
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

	sm := skills.NewSkillManager(workDir, skills.WithUserDir(userSkillsDir))
	if err := sm.Load(); err != nil {
		t.Fatal(err)
	}

	systemBuilder := NewSystemPromptBuilder(PromptConfig{
		WorkDir:           workDir,
		SkillManager:      sm,
		AgentInstructions: "Keep replies concise.",
	})
	systemMessages := systemBuilder.BuildMessages(prompt.PromptContext{
		WorkDir:  workDir,
		Platform: "linux",
	})

	if len(systemMessages) != 2 {
		t.Fatalf("expected static plus dynamic system messages, got %d", len(systemMessages))
	}
	if strings.Contains(systemMessages[0], "demo-skill") {
		t.Fatalf("skill index should not be merged into static system message: %s", systemMessages[0])
	}
	if strings.Contains(systemMessages[1], "<environment>") {
		t.Fatalf("environment should not be in system prompt: %s", systemMessages[1])
	}
	if !strings.Contains(systemMessages[1], "<available_skills>") {
		t.Fatalf("dynamic system message should include available skills section: %s", systemMessages[1])
	}
	if !strings.Contains(systemMessages[1], "demo-skill") {
		t.Fatalf("dynamic system message should include skill index: %s", systemMessages[1])
	}
	if !strings.Contains(systemMessages[1], "<agent_instructions>\nKeep replies concise.\n</agent_instructions>") {
		t.Fatalf("dynamic system message should append agent instructions: %s", systemMessages[1])
	}

	userContextBuilder := NewUserContextBuilder(PromptConfig{
		WorkDir:                    workDir,
		Transport:                  "test transport",
		IncludeProjectInstructions: true,
	})
	context := userContextBuilder.BuildMetaUserContext(prompt.PromptContext{
		WorkDir:  workDir,
		Platform: "linux",
	})
	for _, want := range []string{
		"<user_context>",
		"<environment>",
		"working_directory: " + workDir,
		"platform: linux",
		"transport: test transport",
		"current_date:",
		"timezone:",
		`<project_instructions source="AGENTS.md">`,
		"Follow project rules.",
	} {
		if !strings.Contains(context, want) {
			t.Fatalf("user context missing %q:\n%s", want, context)
		}
	}
}
