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

// ProcessMentions processes a text and activates any mentioned skills
// Returns the list of newly activated skill names
func (m *SkillManager) ProcessMentions(text string) []string {
	mentions := DetectSkillMentions(text)
	if len(mentions) == 0 {
		return nil
	}

	var activated []string
	for _, mention := range mentions {
		skill, ok := m.Get(mention)
		if !ok {
			continue
		}

		// Only add if not already activated
		if !m.IsActivated(skill.Name) {
			if err := m.Activate(skill.Name); err == nil {
				activated = append(activated, skill.Name)
			}
		}
	}

	return activated
}

// BuildAllInjections builds injection content for all activated skills
func (m *SkillManager) BuildAllInjections() string {
	activated := m.GetActivated()
	if len(activated) == 0 {
		return ""
	}

	var parts []string
	for _, skill := range activated {
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
