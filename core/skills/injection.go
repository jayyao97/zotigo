package skills

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"text/template"
)

type ResolutionError struct {
	Kind string
	Name string
	Err  error
}

func (e *ResolutionError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("skill %q is %s: %v", e.Name, e.Kind, e.Err)
	}
	return fmt.Sprintf("skill %q is %s", e.Name, e.Kind)
}

func (e *ResolutionError) Unwrap() error { return e.Err }

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

const skillIndexTmpl = `## Skills

### Available skills
{{range .Skills}}
- **{{.Name}}**: {{.Description}}{{if .Path}} (file: ` + "`{{.Path}}`" + `){{end}}
{{- end}}

### How to use skills

1. **Discovery**: The skill index above lists all available skills with their names and descriptions.
2. **Trigger rules**: Skills are triggered when a user mentions ` + "`$skill-name`" + ` in their message, or when the task clearly matches a skill's description.
3. **Progressive disclosure**: When a skill is triggered, use ` + "`read_file`" + ` to read the skill's SKILL.md file for full instructions before executing. Do NOT guess the skill's behavior from the description alone.
4. **Context hygiene**: Only read a skill's instructions when you actually need them. Do not preload all skill files.
5. **Safety/fallback**: If a skill file cannot be read or is missing, inform the user and fall back to general knowledge.
`

var skillIndexTemplate = template.Must(template.New("skillIndex").Parse(skillIndexTmpl))

// BuildSkillIndex builds a lightweight Markdown index of all loaded skills.
// Instead of injecting full instructions into the system prompt, this provides
// a compact listing that the model can use to discover skills and read their
// SKILL.md files on demand (progressive disclosure).
func (m *SkillManager) BuildSkillIndex() string {
	allSkills := m.List()
	if len(allSkills) == 0 {
		return ""
	}

	var buf bytes.Buffer
	if err := skillIndexTemplate.Execute(&buf, struct {
		Skills []*SkillDefinition
	}{Skills: allSkills}); err != nil {
		return ""
	}
	return buf.String()
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

// ResolveExplicit resolves and deduplicates names for one turn.
func (m *SkillManager) ResolveExplicit(names []string) ([]*SkillDefinition, error) {
	if err := m.EnsureLoaded(); err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	resolved := make([]*SkillDefinition, 0, len(names))
	for _, requested := range names {
		requested = strings.TrimSpace(requested)
		if requested == "" {
			continue
		}
		skill, ok := m.Get(requested)
		if !ok {
			for _, diagnostic := range m.Diagnostics() {
				if diagnostic.Name == requested && (diagnostic.Code == "invalid_skill" || diagnostic.Code == "missing_skill_file") {
					return nil, &ResolutionError{Kind: "invalid", Name: requested}
				}
			}
			return nil, &ResolutionError{Kind: "not_found", Name: requested}
		}
		if !skill.IsEnabled() {
			return nil, &ResolutionError{Kind: "disabled", Name: skill.Name}
		}
		if seen[skill.Name] {
			continue
		}
		seen[skill.Name] = true
		resolved = append(resolved, skill)
	}
	return resolved, nil
}

// InjectExplicit deterministically includes complete selected SKILL.md files.
func InjectExplicit(text string, selected []*SkillDefinition) string {
	if len(selected) == 0 {
		return text
	}
	var builder strings.Builder
	builder.WriteString("<selected_skills>\n")
	for _, skill := range selected {
		fmt.Fprintf(&builder, "<skill name=%q>\n%s\n</skill>\n", skill.Name, strings.TrimSpace(skill.Content))
	}
	builder.WriteString("</selected_skills>\n\n")
	builder.WriteString(text)
	return builder.String()
}
