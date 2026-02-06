package builtin

import (
	"context"

	"github.com/jayyao97/zotigo/cli/commands"
)

// ClearCommand clears the conversation history.
type ClearCommand struct{}

func NewClearCommand() *ClearCommand {
	return &ClearCommand{}
}

func (c *ClearCommand) Name() string        { return "clear" }
func (c *ClearCommand) Aliases() []string   { return []string{"reset"} }
func (c *ClearCommand) Description() string { return "Clear the conversation history and start fresh" }
func (c *ClearCommand) Usage() string       { return "/clear" }

func (c *ClearCommand) Execute(ctx context.Context, env *commands.Environment, args []string) error {
	if env.ClearHistory != nil {
		env.ClearHistory()
	}
	env.Output("Conversation history cleared.")
	return nil
}
