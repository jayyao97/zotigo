package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseSkillContent(t *testing.T) {
	content := `---
name: test-skill
description: A test skill
aliases:
  - ts
  - test
---

# Test Skill Instructions

This is a test skill.
`

	skill, err := ParseSkillContent(content, "test/SKILL.md")
	if err != nil {
		t.Fatalf("ParseSkillContent failed: %v", err)
	}

	if skill.Name != "test-skill" {
		t.Errorf("Expected name 'test-skill', got '%s'", skill.Name)
	}
	if skill.Description != "A test skill" {
		t.Errorf("Expected description 'A test skill', got '%s'", skill.Description)
	}
	if len(skill.Aliases) != 2 {
		t.Errorf("Expected 2 aliases, got %d", len(skill.Aliases))
	}
	if !skill.HasAlias("ts") {
		t.Error("Expected alias 'ts'")
	}
	if skill.Instructions == "" {
		t.Error("Instructions should not be empty")
	}
}

func TestParseSkillContent_Invalid(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"no front matter", "# Just markdown"},
		{"missing name", "---\ndescription: test\n---\n# Content"},
		{"invalid yaml", "---\nname: [invalid\n---\n# Content"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSkillContent(tc.content, "test.md")
			if err == nil {
				t.Error("Expected error for invalid content")
			}
		})
	}
}

func TestSkillManager_Load(t *testing.T) {
	// Create temp directory for test skills
	tmpDir := t.TempDir()
	userDir := filepath.Join(tmpDir, "user-skills")
	projectDir := filepath.Join(tmpDir, "project")
	projectSkillsDir := filepath.Join(projectDir, ".zotigo", "skills")

	// Create directories
	os.MkdirAll(filepath.Join(userDir, "user-skill"), 0755)
	os.MkdirAll(filepath.Join(projectSkillsDir, "project-skill"), 0755)

	// Write user skill
	userSkill := `---
name: user-skill
description: A user skill
---
User skill instructions.
`
	os.WriteFile(filepath.Join(userDir, "user-skill", "SKILL.md"), []byte(userSkill), 0644)

	// Write project skill
	projectSkill := `---
name: project-skill
description: A project skill
---
Project skill instructions.
`
	os.WriteFile(filepath.Join(projectSkillsDir, "project-skill", "SKILL.md"), []byte(projectSkill), 0644)

	// Create manager with custom paths
	sm := &SkillManager{
		skills:     make(map[string]*SkillDefinition),
		aliases:    make(map[string]string),
		projectDir: projectDir,
		userDir:    userDir,
	}

	if err := sm.Load(); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Should have builtin + user + project skills
	if sm.Count() < 3 {
		t.Errorf("Expected at least 3 skills (builtin + user + project), got %d", sm.Count())
	}

	// Check user skill
	userS, ok := sm.Get("user-skill")
	if !ok {
		t.Error("User skill not found")
	} else if userS.Source != SkillSourceUser {
		t.Errorf("Expected source User, got %v", userS.Source)
	}

	// Check project skill
	projectS, ok := sm.Get("project-skill")
	if !ok {
		t.Error("Project skill not found")
	} else if projectS.Source != SkillSourceProject {
		t.Errorf("Expected source Project, got %v", projectS.Source)
	}
}

func TestSkillManager_Priority(t *testing.T) {
	tmpDir := t.TempDir()
	userDir := filepath.Join(tmpDir, "user-skills")
	projectDir := filepath.Join(tmpDir, "project")
	projectSkillsDir := filepath.Join(projectDir, ".zotigo", "skills")

	// Create same-named skill in both locations
	os.MkdirAll(filepath.Join(userDir, "my-skill"), 0755)
	os.MkdirAll(filepath.Join(projectSkillsDir, "my-skill"), 0755)

	userSkill := `---
name: my-skill
description: User version
---
User instructions.
`
	os.WriteFile(filepath.Join(userDir, "my-skill", "SKILL.md"), []byte(userSkill), 0644)

	projectSkill := `---
name: my-skill
description: Project version
---
Project instructions.
`
	os.WriteFile(filepath.Join(projectSkillsDir, "my-skill", "SKILL.md"), []byte(projectSkill), 0644)

	sm := &SkillManager{
		skills:     make(map[string]*SkillDefinition),
		aliases:    make(map[string]string),
		projectDir: projectDir,
		userDir:    userDir,
	}

	if err := sm.Load(); err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	// Project skill should win
	skill, ok := sm.Get("my-skill")
	if !ok {
		t.Fatal("Skill not found")
	}
	if skill.Source != SkillSourceProject {
		t.Errorf("Expected Project source (highest priority), got %v", skill.Source)
	}
	if skill.Description != "Project version" {
		t.Errorf("Expected 'Project version', got '%s'", skill.Description)
	}
}

func TestSkillManager_Aliases(t *testing.T) {
	sm := NewSkillManager("")
	sm.Load()

	// Get by alias
	skill, ok := sm.Get("create-skill") // Alias for skill-creator
	if !ok {
		t.Fatal("Should find skill by alias")
	}
	if skill.Name != "skill-creator" {
		t.Errorf("Expected skill-creator, got %s", skill.Name)
	}
}

func TestDetectSkillMentions(t *testing.T) {
	tests := []struct {
		text     string
		expected []string
	}{
		{"Use $code-review to check this", []string{"code-review"}},
		{"Try $skill1 and $skill2", []string{"skill1", "skill2"}},
		{"No mentions here", nil},
		{"$test $test duplicate", []string{"test"}}, // Deduped
		{"$my_skill with underscore", []string{"my_skill"}},
		{"$mySkill with camelCase", []string{"mySkill"}},
	}

	for _, tc := range tests {
		t.Run(tc.text, func(t *testing.T) {
			mentions := DetectSkillMentions(tc.text)
			if len(mentions) != len(tc.expected) {
				t.Errorf("Expected %d mentions, got %d", len(tc.expected), len(mentions))
				return
			}
			for i, exp := range tc.expected {
				if mentions[i] != exp {
					t.Errorf("Expected mention '%s', got '%s'", exp, mentions[i])
				}
			}
		})
	}
}

func TestBuildSkillInjection(t *testing.T) {
	skill := &SkillDefinition{
		Name:         "test-skill",
		Description:  "A test skill",
		Instructions: "Do something\nDo another thing",
		Source:       SkillSourceUser,
	}

	injection := BuildSkillInjection(skill)

	if injection == "" {
		t.Error("Injection should not be empty")
	}
	if !contains(injection, `<skill name="test-skill"`) {
		t.Error("Should contain skill XML tag")
	}
	if !contains(injection, `source="user"`) {
		t.Error("Should contain source attribute")
	}
	if !contains(injection, "<instructions>") {
		t.Error("Should contain instructions tag")
	}
	if !contains(injection, "Do something") {
		t.Error("Should contain instruction content")
	}
}

func TestBuiltinSkills(t *testing.T) {
	if len(BuiltinSkills) == 0 {
		t.Error("Should have at least one builtin skill")
	}

	// Check skill-creator exists
	var found bool
	for _, skill := range BuiltinSkills {
		if skill.Name == "skill-creator" {
			found = true
			if skill.Source != SkillSourceBuiltin {
				t.Error("Builtin skill should have Builtin source")
			}
			if skill.Instructions == "" {
				t.Error("skill-creator should have instructions")
			}
			break
		}
	}
	if !found {
		t.Error("skill-creator builtin skill not found")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
