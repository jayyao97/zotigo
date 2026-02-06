package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/jayyao97/zotigo/cli/commands"
	"github.com/jayyao97/zotigo/core/skills"
)

// SkillCommand activates a skill
type SkillCommand struct{}

func NewSkillCommand() *SkillCommand {
	return &SkillCommand{}
}

func (c *SkillCommand) Name() string {
	return "skill"
}

func (c *SkillCommand) Aliases() []string {
	return []string{"sk"}
}

func (c *SkillCommand) Description() string {
	return "Activate or deactivate a skill"
}

func (c *SkillCommand) Usage() string {
	return `/skill <name>           Activate a skill
/skill --off <name>      Deactivate a skill
/skill --off-all         Deactivate all skills
/skill --info <name>     Show skill details`
}

func (c *SkillCommand) Execute(ctx context.Context, env *commands.Environment, args []string) error {
	sm, ok := env.SkillManager.(*skills.SkillManager)
	if !ok || sm == nil {
		return fmt.Errorf("skill manager not available")
	}

	if len(args) == 0 {
		env.Output("Usage: %s", c.Usage())
		return nil
	}

	// Parse flags
	switch args[0] {
	case "--off", "-d", "--deactivate":
		if len(args) < 2 {
			return fmt.Errorf("skill name required")
		}
		return sm.Deactivate(args[1])

	case "--off-all", "--clear":
		sm.DeactivateAll()
		env.Output("All skills deactivated")
		return nil

	case "--info", "-i":
		if len(args) < 2 {
			return fmt.Errorf("skill name required")
		}
		return c.showSkillInfo(env, sm, args[1])

	default:
		// Activate skill
		skillName := args[0]
		if err := sm.Activate(skillName); err != nil {
			return err
		}
		skill, _ := sm.Get(skillName)
		env.Output("Activated skill: %s", skill.Name)
		if skill.Description != "" {
			env.Output("  %s", skill.Description)
		}
		return nil
	}
}

func (c *SkillCommand) showSkillInfo(env *commands.Environment, sm *skills.SkillManager, name string) error {
	skill, ok := sm.Get(name)
	if !ok {
		return fmt.Errorf("skill not found: %s", name)
	}

	env.Output("Skill: %s", skill.Name)
	env.Output("Source: %s", skill.Source.String())
	if skill.Description != "" {
		env.Output("Description: %s", skill.Description)
	}
	if len(skill.Aliases) > 0 {
		env.Output("Aliases: %s", strings.Join(skill.Aliases, ", "))
	}
	if skill.Path != "" {
		env.Output("Path: %s", skill.Path)
	}
	env.Output("Activated: %v", sm.IsActivated(skill.Name))

	return nil
}

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
		status := " "
		if sm.IsActivated(skill.Name) {
			status = "*"
		}

		// Build aliases string
		aliasStr := ""
		if len(skill.Aliases) > 0 {
			aliasStr = fmt.Sprintf(" (%s)", strings.Join(skill.Aliases, ", "))
		}

		env.Output("  %s %-20s [%s]%s", status, skill.Name, skill.Source.String(), aliasStr)
		if skill.Description != "" {
			env.Output("      %s", skill.Description)
		}
	}

	env.Output("")
	env.Output("  * = activated")
	env.Output("  Use /skill <name> to activate a skill")

	return nil
}
