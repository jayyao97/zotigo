package builtin

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jayyao97/zotigo/cli/commands"
)

// HelpCommand shows available commands.
type HelpCommand struct {
	registry *commands.Registry
}

// NewHelpCommand creates a new help command.
func NewHelpCommand(registry *commands.Registry) *HelpCommand {
	return &HelpCommand{registry: registry}
}

func (c *HelpCommand) Name() string        { return "help" }
func (c *HelpCommand) Aliases() []string   { return []string{"h", "?"} }
func (c *HelpCommand) Description() string { return "Show available commands" }
func (c *HelpCommand) Usage() string       { return "/help [command]" }

func (c *HelpCommand) Execute(ctx context.Context, env *commands.Environment, args []string) error {
	if len(args) > 0 {
		// Show help for specific command
		return c.showCommandHelp(env, args[0])
	}

	// Show all commands
	return c.showAllCommands(env)
}

func (c *HelpCommand) showAllCommands(env *commands.Environment) error {
	cmds := c.registry.List()

	// Sort by name
	sort.Slice(cmds, func(i, j int) bool {
		return cmds[i].Name() < cmds[j].Name()
	})

	var sb strings.Builder
	sb.WriteString("Available commands:\n\n")

	for _, cmd := range cmds {
		aliases := cmd.Aliases()
		aliasStr := ""
		if len(aliases) > 0 {
			aliasStr = fmt.Sprintf(" (aliases: /%s)", strings.Join(aliases, ", /"))
		}
		sb.WriteString(fmt.Sprintf("  /%s%s\n", cmd.Name(), aliasStr))
		sb.WriteString(fmt.Sprintf("    %s\n\n", cmd.Description()))
	}

	sb.WriteString("Type /help <command> for more details.")
	env.Output("%s", sb.String())
	return nil
}

func (c *HelpCommand) showCommandHelp(env *commands.Environment, name string) error {
	cmd, found := c.registry.Get(name)
	if !found {
		return fmt.Errorf("unknown command: %s", name)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("/%s - %s\n\n", cmd.Name(), cmd.Description()))

	if usage := cmd.Usage(); usage != "" {
		sb.WriteString(fmt.Sprintf("Usage: %s\n", usage))
	}

	if aliases := cmd.Aliases(); len(aliases) > 0 {
		sb.WriteString(fmt.Sprintf("Aliases: /%s\n", strings.Join(aliases, ", /")))
	}

	env.Output("%s", sb.String())
	return nil
}
