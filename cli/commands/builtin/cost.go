package builtin

import (
	"context"
	"fmt"

	"github.com/jayyao97/zotigo/cli/commands"
	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/protocol"
)

// CostCommand shows token usage statistics.
type CostCommand struct{}

func NewCostCommand() *CostCommand {
	return &CostCommand{}
}

func (c *CostCommand) Name() string        { return "cost" }
func (c *CostCommand) Aliases() []string   { return []string{"usage"} }
func (c *CostCommand) Description() string { return "Show token usage statistics for this session" }
func (c *CostCommand) Usage() string       { return "/cost" }

func (c *CostCommand) Execute(ctx context.Context, env *commands.Environment, args []string) error {
	ag, ok := env.Agent.(*agent.Agent)
	if !ok || ag == nil {
		env.Output("Cost requires an active agent session.")
		return nil
	}

	snap := ag.Snapshot()

	// Share the same aggregation path as the TUI exit summary so
	// legacy Anthropic turns (TotalTokens=0 on disk) are normalized
	// in both places.
	total := protocol.SessionUsage(snap.History)
	turns := protocol.CountAssistantTurns(snap.History)

	if turns == 0 {
		env.Output("No assistant turns recorded yet.")
		return nil
	}

	env.Output("Session Token Usage:\n")
	env.Output("  Input tokens:          %s\n", formatNumber(total.InputTokens))
	env.Output("  Output tokens:         %s\n", formatNumber(total.OutputTokens))
	if total.CacheCreationInputTokens > 0 {
		env.Output("  Cache creation tokens: %s\n", formatNumber(total.CacheCreationInputTokens))
	}
	if total.CacheReadInputTokens > 0 {
		env.Output("  Cache read tokens:     %s\n", formatNumber(total.CacheReadInputTokens))
	}
	if total.TotalTokens > 0 {
		env.Output("  Total tokens:          %s\n", formatNumber(total.TotalTokens))
	}
	env.Output("  Turns:                 %d", turns)

	return nil
}

// formatNumber formats an integer with comma separators.
func formatNumber(n int) string {
	if n < 0 {
		return "-" + formatNumber(-n)
	}
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}

	var result []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	return string(result)
}
