package builtin

import (
	"context"

	"github.com/jayyao97/zotigo/cli/commands"
)

// CostCommand shows token usage statistics.
type CostCommand struct{}

func NewCostCommand() *CostCommand {
	return &CostCommand{}
}

func (c *CostCommand) Name() string        { return "cost" }
func (c *CostCommand) Aliases() []string   { return []string{"usage", "tokens"} }
func (c *CostCommand) Description() string { return "Show token usage statistics for this session" }
func (c *CostCommand) Usage() string       { return "/cost" }

func (c *CostCommand) Execute(ctx context.Context, env *commands.Environment, args []string) error {
	// TODO: Implement token tracking in agent
	env.Output("Token usage tracking is not yet implemented.")
	return nil
}
