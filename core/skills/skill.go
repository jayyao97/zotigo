package skills

// SkillSource represents where a skill was loaded from
type SkillSource int

const (
	// SkillSourceBuiltin - built into the binary
	SkillSourceBuiltin SkillSource = iota
	// SkillSourceAgents - loaded from ~/.agents/skills/ (shared across agent CLIs)
	SkillSourceAgents
	// SkillSourceUser - loaded from ~/.zotigo/skills/
	SkillSourceUser
	// SkillSourceProject - loaded from .zotigo/skills/
	SkillSourceProject
)

func (s SkillSource) String() string {
	switch s {
	case SkillSourceBuiltin:
		return "builtin"
	case SkillSourceAgents:
		return "agents"
	case SkillSourceUser:
		return "user"
	case SkillSourceProject:
		return "project"
	default:
		return "unknown"
	}
}

// SkillDefinition represents a skill with its metadata and instructions
type SkillDefinition struct {
	// Name is the unique identifier for the skill
	Name string `yaml:"name"`

	// Description explains what the skill does
	Description string `yaml:"description"`

	// Aliases are alternative names for the skill
	Aliases []string `yaml:"aliases,omitempty"`

	// Instructions is the markdown content after the YAML front matter
	Instructions string `yaml:"-"`

	// Source indicates where this skill was loaded from
	Source SkillSource `yaml:"-"`

	// Path is the file path (for debugging and reloading)
	Path string `yaml:"-"`
}

// HasAlias checks if the skill has a specific alias
func (s *SkillDefinition) HasAlias(alias string) bool {
	for _, a := range s.Aliases {
		if a == alias {
			return true
		}
	}
	return false
}

// MatchesNameOrAlias checks if the skill matches the given name or alias
func (s *SkillDefinition) MatchesNameOrAlias(nameOrAlias string) bool {
	if s.Name == nameOrAlias {
		return true
	}
	return s.HasAlias(nameOrAlias)
}
