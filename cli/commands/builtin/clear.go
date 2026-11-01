package builtin

import (
	"context"

	"github.com/jayyao97/zotigo/cli/commands"
)

// ClearCommand starts a fresh conversation. Sessions are durable in the
// session service, so the old conversation is kept and can be resumed.
type ClearCommand struct{}

func NewClearCommand() *ClearCommand {
	return &ClearCommand{}
}

func (c *ClearCommand) Name() string      { return "clear" }
func (c *ClearCommand) Aliases() []string { return []string{"reset", "new"} }
func (c *ClearCommand) Description() string {
	return "Start a new session in this directory (the current one stays resumable)"
}
func (c *ClearCommand) Usage() string { return "/clear" }

func (c *ClearCommand) Execute(ctx context.Context, env *commands.Environment, args []string) error {
	if env.NewSession == nil {
		env.Output("Starting a new session is not supported in this context.")
		return nil
	}
	return env.NewSession()
}
