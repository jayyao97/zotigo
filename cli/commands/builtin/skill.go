package builtin

import (
	"context"
	"fmt"

	"github.com/jayyao97/zotigo/cli/commands"
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
	if env.Skills == nil {
		return fmt.Errorf("skills are not available")
	}
	reload := len(args) > 0 && (args[0] == "--reload" || args[0] == "-r")
	allSkills, err := env.Skills(ctx, reload)
	if err != nil {
		return fmt.Errorf("failed to list skills: %w", err)
	}
	if reload {
		env.Output("Skills reloaded")
	}
	if len(allSkills) == 0 {
		env.Output("No skills available")
		return nil
	}

	env.Output("Available skills (%d):", len(allSkills))
	env.Output("")
	for _, skill := range allSkills {
		env.Output("  %-20s [%s]", skill.Name, skill.Scope)
		if skill.Description != "" {
			env.Output("      %s", skill.Description)
		}
	}
	return nil
}
