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

func TestDiscoverSkills_FollowsDirectorySymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "linked-skill")
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatalf("create skill target: %v", err)
	}
	content := "---\nname: linked-skill\ndescription: Linked skill\n---\nInstructions.\n"
	if err := os.WriteFile(filepath.Join(target, SkillFileName), []byte(content), 0644); err != nil {
		t.Fatalf("write linked skill: %v", err)
	}
	link := filepath.Join(root, "linked-skill")
	relativeTarget, err := filepath.Rel(root, target)
	if err != nil {
		t.Fatalf("make relative skill target: %v", err)
	}
	if err := os.Symlink(relativeTarget, link); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}

	discovered, err := DiscoverSkills(root, SkillSourceAgents)
	if err != nil {
		t.Fatalf("discover linked skill: %v", err)
	}
	if len(discovered) != 1 || discovered[0].Name != "linked-skill" {
		t.Fatalf("unexpected discovered skills: %#v", discovered)
	}
	if want := filepath.Join(link, SkillFileName); discovered[0].Path != want {
		t.Fatalf("skill path = %q, want logical path %q", discovered[0].Path, want)
	}
}

func TestDiscoverSkills_SkipsBrokenDirectorySymlink(t *testing.T) {
	root := t.TempDir()
	validDir := filepath.Join(root, "valid-skill")
	if err := os.MkdirAll(validDir, 0755); err != nil {
		t.Fatalf("create valid skill: %v", err)
	}
	content := "---\nname: valid-skill\ndescription: Valid skill\n---\nInstructions.\n"
	if err := os.WriteFile(filepath.Join(validDir, SkillFileName), []byte(content), 0644); err != nil {
		t.Fatalf("write valid skill: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "missing"), filepath.Join(root, "broken")); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}

	discovered, err := DiscoverSkills(root, SkillSourceAgents)
	if err != nil {
		t.Fatalf("discover alongside broken symlink: %v", err)
	}
	if len(discovered) != 1 || discovered[0].Name != "valid-skill" {
		t.Fatalf("unexpected discovered skills: %#v", discovered)
	}
}

func TestDiscoverSkills_SkipsSymlinkResolutionCycles(t *testing.T) {
	root := t.TempDir()
	validDir := filepath.Join(root, "valid-skill")
	if err := os.MkdirAll(validDir, 0755); err != nil {
		t.Fatalf("create valid skill: %v", err)
	}
	content := "---\nname: valid-skill\ndescription: Valid skill\n---\nInstructions.\n"
	if err := os.WriteFile(filepath.Join(validDir, SkillFileName), []byte(content), 0644); err != nil {
		t.Fatalf("write valid skill: %v", err)
	}
	if err := os.Symlink("self-loop", filepath.Join(root, "self-loop")); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	if err := os.Symlink("loop-b", filepath.Join(root, "loop-a")); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	if err := os.Symlink("loop-a", filepath.Join(root, "loop-b")); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}

	discovered, err := DiscoverSkills(root, SkillSourceAgents)
	if err != nil {
		t.Fatalf("discover alongside symlink cycles: %v", err)
	}
	if len(discovered) != 1 || discovered[0].Name != "valid-skill" {
		t.Fatalf("unexpected discovered skills: %#v", discovered)
	}
}

func TestDiscoverSkills_SkipsUnreadableSymlinkTree(t *testing.T) {
	root := t.TempDir()
	validDir := filepath.Join(root, "valid-skill")
	if err := os.MkdirAll(validDir, 0755); err != nil {
		t.Fatalf("create valid skill: %v", err)
	}
	content := "---\nname: valid-skill\ndescription: Valid skill\n---\nInstructions.\n"
	if err := os.WriteFile(filepath.Join(validDir, SkillFileName), []byte(content), 0644); err != nil {
		t.Fatalf("write valid skill: %v", err)
	}
	unreadableTarget := filepath.Join(t.TempDir(), "unreadable")
	if err := os.MkdirAll(unreadableTarget, 0755); err != nil {
		t.Fatalf("create unreadable target: %v", err)
	}
	if err := os.Symlink(unreadableTarget, filepath.Join(root, "unreadable")); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	if err := os.Chmod(unreadableTarget, 0); err != nil {
		t.Skipf("cannot make symlink target unreadable: %v", err)
	}
	defer func() { _ = os.Chmod(unreadableTarget, 0755) }()
	if _, err := os.ReadDir(unreadableTarget); err == nil {
		t.Skip("current user can read mode-000 directories")
	}

	discovered, err := DiscoverSkills(root, SkillSourceAgents)
	if err != nil {
		t.Fatalf("discover alongside unreadable symlink tree: %v", err)
	}
	if len(discovered) != 1 || discovered[0].Name != "valid-skill" {
		t.Fatalf("unexpected discovered skills: %#v", discovered)
	}
}

func TestDiscoverSkills_RealDirectoryErrorWinsOverOptionalAlias(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "z-real")
	if err := os.MkdirAll(realDir, 0755); err != nil {
		t.Fatalf("create real directory: %v", err)
	}
	if err := os.Symlink(realDir, filepath.Join(root, "a-link")); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	if err := os.Chmod(realDir, 0); err != nil {
		t.Skipf("cannot make directory unreadable: %v", err)
	}
	defer func() { _ = os.Chmod(realDir, 0755) }()
	if _, err := os.ReadDir(realDir); err == nil {
		t.Skip("current user can read mode-000 directories")
	}

	if _, err := DiscoverSkills(root, SkillSourceAgents); err == nil {
		t.Fatal("expected unreadable real directory to remain an error")
	}
}

func TestDiscoverSkills_RequiredSubtreeErrorWinsOverOptionalAlias(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "z-real")
	unreadableChild := filepath.Join(realDir, "child")
	if err := os.MkdirAll(unreadableChild, 0755); err != nil {
		t.Fatalf("create real subtree: %v", err)
	}
	if err := os.Symlink(realDir, filepath.Join(root, "a-link")); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	if err := os.Chmod(unreadableChild, 0); err != nil {
		t.Skipf("cannot make child directory unreadable: %v", err)
	}
	defer func() { _ = os.Chmod(unreadableChild, 0755) }()
	if _, err := os.ReadDir(unreadableChild); err == nil {
		t.Skip("current user can read mode-000 directories")
	}

	if _, err := DiscoverSkills(root, SkillSourceAgents); err == nil {
		t.Fatal("expected unreadable required subtree to remain an error")
	}
}

func TestDiscoverSkills_RequiredEntryInfoErrorIsReported(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "entry"), []byte("test"), 0644); err != nil {
		t.Fatalf("write directory entry: %v", err)
	}
	if err := os.Chmod(root, 0400); err != nil {
		t.Skipf("cannot remove directory search permission: %v", err)
	}
	defer func() { _ = os.Chmod(root, 0755) }()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Skipf("current filesystem cannot list non-searchable directory: %v", err)
	}
	if _, err := entries[0].Info(); err == nil {
		t.Skip("current user can inspect entries in a non-searchable directory")
	}

	if _, err := DiscoverSkills(root, SkillSourceAgents); err == nil {
		t.Fatal("expected required entry inspection error")
	}
}

func TestWalkSkillsDir_MissingRequiredRootReturnsError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if err := walkSkillsDir(missing, func(string) error { return nil }); err == nil {
		t.Fatal("expected missing required directory to return an error")
	}
}

func TestDiscoverSkills_DeduplicatesSymlinkCycle(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "cycle-skill")
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatalf("create cycle skill: %v", err)
	}
	content := "---\nname: cycle-skill\ndescription: Cycle skill\n---\nInstructions.\n"
	if err := os.WriteFile(filepath.Join(target, SkillFileName), []byte(content), 0644); err != nil {
		t.Fatalf("write cycle skill: %v", err)
	}
	if err := os.Symlink(root, filepath.Join(target, "back")); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}

	discovered, err := DiscoverSkills(root, SkillSourceAgents)
	if err != nil {
		t.Fatalf("discover symlink cycle: %v", err)
	}
	if len(discovered) != 1 || discovered[0].Name != "cycle-skill" {
		t.Fatalf("unexpected discovered skills: %#v", discovered)
	}
}

func TestDiscoverSkills_PrefersShallowPathToSharedSymlinkTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "shared-target")
	nestedSkill := filepath.Join(target, "nested-skill")
	if err := os.MkdirAll(nestedSkill, 0755); err != nil {
		t.Fatalf("create nested skill: %v", err)
	}
	content := "---\nname: nested-skill\ndescription: Nested skill\n---\nInstructions.\n"
	if err := os.WriteFile(filepath.Join(nestedSkill, SkillFileName), []byte(content), 0644); err != nil {
		t.Fatalf("write nested skill: %v", err)
	}
	deepParent := filepath.Join(root, "a", "x", "y")
	shallowParent := filepath.Join(root, "z")
	if err := os.MkdirAll(deepParent, 0755); err != nil {
		t.Fatalf("create deep parent: %v", err)
	}
	if err := os.MkdirAll(shallowParent, 0755); err != nil {
		t.Fatalf("create shallow parent: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(deepParent, "link")); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(shallowParent, "link")); err != nil {
		t.Skipf("directory symlinks unavailable: %v", err)
	}

	discovered, err := DiscoverSkills(root, SkillSourceAgents)
	if err != nil {
		t.Fatalf("discover shared symlink target: %v", err)
	}
	if len(discovered) != 1 || discovered[0].Name != "nested-skill" {
		t.Fatalf("unexpected discovered skills: %#v", discovered)
	}
	if want := filepath.Join(shallowParent, "link", "nested-skill", SkillFileName); discovered[0].Path != want {
		t.Fatalf("skill path = %q, want shallow logical path %q", discovered[0].Path, want)
	}
}

func TestDiscoverSkills_PreservesDepthBoundary(t *testing.T) {
	root := t.TempDir()
	boundaryDir := filepath.Join(root, "one", "two", "three", "four")
	beyondDir := filepath.Join(boundaryDir, "five")
	if err := os.MkdirAll(beyondDir, 0755); err != nil {
		t.Fatalf("create nested skills: %v", err)
	}
	boundaryContent := "---\nname: boundary-skill\ndescription: Boundary skill\n---\nInstructions.\n"
	if err := os.WriteFile(filepath.Join(boundaryDir, SkillFileName), []byte(boundaryContent), 0644); err != nil {
		t.Fatalf("write boundary skill: %v", err)
	}
	beyondContent := "---\nname: beyond-skill\ndescription: Beyond skill\n---\nInstructions.\n"
	if err := os.WriteFile(filepath.Join(beyondDir, SkillFileName), []byte(beyondContent), 0644); err != nil {
		t.Fatalf("write beyond-boundary skill: %v", err)
	}

	discovered, err := DiscoverSkills(root, SkillSourceAgents)
	if err != nil {
		t.Fatalf("discover depth boundary: %v", err)
	}
	if len(discovered) != 1 || discovered[0].Name != "boundary-skill" {
		t.Fatalf("unexpected discovered skills: %#v", discovered)
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

	// Get by name
	skill, ok := sm.Get("skill-creator")
	if !ok {
		t.Fatal("Should find skill-creator")
	}
	if skill.Name != "skill-creator" {
		t.Errorf("Expected skill-creator, got %s", skill.Name)
	}

	// Get by alias (only if the loaded skill has aliases)
	// Note: ~/.agents/skills/skill-creator may not have aliases, so this is best-effort
	skill, ok = sm.Get("create-skill")
	if ok && skill.Name != "skill-creator" {
		t.Errorf("Expected skill-creator by alias, got %s", skill.Name)
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

func TestBuildSkillIndex(t *testing.T) {
	sm := &SkillManager{
		skills:  make(map[string]*SkillDefinition),
		aliases: make(map[string]string),
	}
	sm.addSkill(&SkillDefinition{
		Name:         "test-skill",
		Description:  "A test skill",
		Instructions: "Do something\nDo another thing",
		Source:       SkillSourceUser,
		Path:         "/home/user/.zotigo/skills/test-skill/SKILL.md",
	})
	sm.addSkill(&SkillDefinition{
		Name:         "another-skill",
		Description:  "Another skill",
		Instructions: "Instructions here",
		Source:       SkillSourceProject,
		Path:         "/project/.zotigo/skills/another-skill/SKILL.md",
	})

	index := sm.BuildSkillIndex()

	if index == "" {
		t.Fatal("Index should not be empty")
	}
	// Should contain skill names and descriptions
	if !contains(index, "test-skill") {
		t.Error("Should contain skill name 'test-skill'")
	}
	if !contains(index, "A test skill") {
		t.Error("Should contain skill description")
	}
	if !contains(index, "another-skill") {
		t.Error("Should contain skill name 'another-skill'")
	}
	// Should contain file paths
	if !contains(index, "/home/user/.zotigo/skills/test-skill/SKILL.md") {
		t.Error("Should contain skill file path")
	}
	// Should contain How to use skills section
	if !contains(index, "How to use skills") {
		t.Error("Should contain 'How to use skills' section")
	}
	// Should NOT contain XML tags
	if contains(index, "<instructions>") {
		t.Error("Should not contain XML instructions tag")
	}
	if contains(index, "<skill") {
		t.Error("Should not contain XML skill tag")
	}
}

func TestBuildSkillIndex_Empty(t *testing.T) {
	sm := &SkillManager{
		skills:  make(map[string]*SkillDefinition),
		aliases: make(map[string]string),
	}

	index := sm.BuildSkillIndex()
	if index != "" {
		t.Error("Empty manager should return empty index")
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

func TestGetAgentsUserSkillsDir_Exists(t *testing.T) {
	// Use a temp dir injected via WithAgentsDir — mirrors ReloadAgentsDir
	// but is simpler since we don't need the reload scenario.
	// Avoids writing to the user's real ~/.agents/skills/.
	tmpDir := t.TempDir()
	agentsDir := filepath.Join(tmpDir, "agents-skills")

	os.MkdirAll(filepath.Join(agentsDir, "test-agents-skill"), 0755)
	content := "---\nname: test-agents-skill\ndescription: Test from ~/.agents\n---\nTest content.\n"
	os.WriteFile(filepath.Join(agentsDir, "test-agents-skill", "SKILL.md"), []byte(content), 0644)

	// Create manager pointing at the temp agents dir
	sm := NewSkillManager("", WithAgentsDir(agentsDir))
	sm.Load()

	// Should find the skill we just wrote
	skill, ok := sm.Get("test-agents-skill")
	if !ok {
		t.Fatal("Expected to find skill from ~/.agents/skills/")
	}
	if skill.Source != SkillSourceAgents {
		t.Errorf("Expected source Agents, got %v", skill.Source)
	}
}

func TestSkillManager_ReloadAgentsDir(t *testing.T) {
	tmpDir := t.TempDir()
	agentsDir := filepath.Join(tmpDir, "agents-skills")
	projectDir := filepath.Join(tmpDir, "project")

	// Create manager with empty agentsDir — simulates ~/.agents/skills/ not existing
	sm := NewSkillManager(projectDir, WithAgentsDir(""))
	if err := sm.Load(); err != nil {
		t.Fatalf("Initial Load failed: %v", err)
	}
	initial := sm.Count()

	// Now create the directory and a skill file — simulates user
	// creating ~/.agents/skills/ after startup
	os.MkdirAll(filepath.Join(agentsDir, "reload-skill"), 0755)
	content := "---\nname: reload-skill\ndescription: Added after reload\n---\nReloaded!\n"
	os.WriteFile(filepath.Join(agentsDir, "reload-skill", "SKILL.md"), []byte(content), 0644)

	// Point at the new dir and reload
	sm.SetAgentsDir(agentsDir)
	if err := sm.Reload(); err != nil {
		t.Fatalf("Reload failed: %v", err)
	}

	// Should now see the new skill
	if sm.Count() <= initial {
		t.Errorf("Expected skill count %d+, got %d", initial+1, sm.Count())
	}
	if _, ok := sm.Get("reload-skill"); !ok {
		t.Error("Expected to find reload-skill after reload")
	}
}
