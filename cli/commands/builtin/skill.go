package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/jayyao97/zotigo/cli/commands"
	"github.com/jayyao97/zotigo/core/skills"
)

// SkillsCommand lists all available skills
type SkillsCommand struct{}

func NewSkillsCommand() *SkillsCommand {
	return &SkillsCommand{}
}

func (c *SkillsCommand) Name() string {
	return "skills"
}

func (c *SkillsCommand) Aliases() []string {
	return []string{"sks"}
}

func (c *SkillsCommand) Description() string {
	return "List available skills"
}

func (c *SkillsCommand) Usage() string {
	return `/skills              List all skills
/skills --reload     Reload skills from disk`
}

func (c *SkillsCommand) Execute(ctx context.Context, env *commands.Environment, args []string) error {
	sm, ok := env.SkillManager.(*skills.SkillManager)
	if !ok || sm == nil {
		return fmt.Errorf("skill manager not available")
	}

	// Handle reload flag
	if len(args) > 0 && (args[0] == "--reload" || args[0] == "-r") {
		if err := sm.Reload(); err != nil {
			return fmt.Errorf("failed to reload skills: %w", err)
		}
		env.Output("Skills reloaded")
	}

	// List skills
	allSkills := sm.List()
	if len(allSkills) == 0 {
		env.Output("No skills available")
		return nil
	}

	env.Output("Available skills (%d):", len(allSkills))
	env.Output("")

	for _, skill := range allSkills {
		// Build aliases string
		aliasStr := ""
		if len(skill.Aliases) > 0 {
			aliasStr = fmt.Sprintf(" (%s)", strings.Join(skill.Aliases, ", "))
		}

		env.Output("  %-20s [%s]%s", skill.Name, skill.Source.String(), aliasStr)
		if skill.Description != "" {
			env.Output("      %s", skill.Description)
		}
	}

	return nil
}
