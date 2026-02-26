package skills

import (
	"fmt"
	"regexp"
	"strings"
)

// skillMentionRegex matches $skill-name patterns
// Supports: $skill-name, $skill_name, $skillName
var skillMentionRegex = regexp.MustCompile(`\$([a-zA-Z][a-zA-Z0-9_-]*)`)

// DetectSkillMentions detects $SkillName mentions in text
func DetectSkillMentions(text string) []string {
	matches := skillMentionRegex.FindAllStringSubmatch(text, -1)
	if matches == nil {
		return nil
	}

	// Deduplicate
	seen := make(map[string]bool)
	var mentions []string
	for _, match := range matches {
		name := match[1]
		if !seen[name] {
			seen[name] = true
			mentions = append(mentions, name)
		}
	}

	return mentions
}

// BuildSkillInjection builds the XML injection content for a skill
func BuildSkillInjection(skill *SkillDefinition) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("<skill name=%q source=%q>\n", skill.Name, skill.Source.String()))

	if skill.Description != "" {
		sb.WriteString(fmt.Sprintf("  <description>%s</description>\n", skill.Description))
	}

	sb.WriteString("  <instructions>\n")
	// Indent each line of instructions
	for _, line := range strings.Split(skill.Instructions, "\n") {
		sb.WriteString("    ")
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	sb.WriteString("  </instructions>\n")

	sb.WriteString("</skill>")

	return sb.String()
}

// BuildAllInjections builds injection content for all loaded skills
func (m *SkillManager) BuildAllInjections() string {
	allSkills := m.List()
	if len(allSkills) == 0 {
		return ""
	}

	var parts []string
	for _, skill := range allSkills {
		parts = append(parts, BuildSkillInjection(skill))
	}

	return strings.Join(parts, "\n\n")
}

// RemoveMentions removes $skill-name mentions from text
// This is useful when you want to clean up the user message
func RemoveMentions(text string) string {
	return skillMentionRegex.ReplaceAllString(text, "")
}

// ReplaceMentions replaces $skill-name with the skill description
func (m *SkillManager) ReplaceMentions(text string) string {
	return skillMentionRegex.ReplaceAllStringFunc(text, func(match string) string {
		name := match[1:] // Remove $ prefix
		skill, ok := m.Get(name)
		if !ok {
			return match // Keep original if skill not found
		}
		return fmt.Sprintf("[skill: %s]", skill.Name)
	})
}
