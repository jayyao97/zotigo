package skills

import (
	_ "embed"
)

//go:embed builtin/skill-creator/SKILL.md
var skillCreatorContent string

//go:embed builtin/lsp-usage/SKILL.md
var lspUsageContent string

// BuiltinSkills contains all built-in skills
var BuiltinSkills []*SkillDefinition

func init() {
	// Parse the embedded skill-creator content
	skillCreator, err := ParseSkillContent(skillCreatorContent, "builtin/skill-creator/SKILL.md")
	if err != nil {
		// This should never happen with valid embedded content
		panic("failed to parse builtin skill-creator: " + err.Error())
	}
	skillCreator.Source = SkillSourceBuiltin

	// Parse the embedded lsp-usage content
	lspUsage, err := ParseSkillContent(lspUsageContent, "builtin/lsp-usage/SKILL.md")
	if err != nil {
		panic("failed to parse builtin lsp-usage: " + err.Error())
	}
	lspUsage.Source = SkillSourceBuiltin

	BuiltinSkills = []*SkillDefinition{skillCreator, lspUsage}
}
