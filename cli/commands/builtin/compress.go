package builtin

import (
	"context"
	"fmt"

	"github.com/jayyao97/zotigo/cli/commands"
	"github.com/jayyao97/zotigo/core/agent"
)

// CompressCommand manually triggers context compression.
type CompressCommand struct{}

func NewCompressCommand() *CompressCommand {
	return &CompressCommand{}
}

func (c *CompressCommand) Name() string        { return "compress" }
func (c *CompressCommand) Aliases() []string   { return []string{"summarize", "compact"} }
func (c *CompressCommand) Description() string { return "Manually compress conversation history" }
func (c *CompressCommand) Usage() string       { return "/compress" }

func (c *CompressCommand) Execute(ctx context.Context, env *commands.Environment, args []string) error {
	// Try to get agent from environment
	ag, ok := env.Agent.(*agent.Agent)
	if !ok || ag == nil {
		env.Output("Compress requires an active agent session.")
		return nil
	}

	// Get stats before compression
	statsBefore := ag.GetContextStats()
	tokensBefore := statsBefore["estimated_tokens"]
	messagesBefore := statsBefore["message_count"]

	// Force compression
	result, err := ag.ForceCompress(ctx)
	if err != nil {
		return fmt.Errorf("compression failed: %w", err)
	}

	if !result.Compressed {
		env.Output("No compression needed.\n")
		env.Output("Current context: %d messages, ~%d tokens", messagesBefore, tokensBefore)
		return nil
	}

	// Report results
	env.Output("Context compressed successfully!\n")
	env.Output("Before: %d messages, ~%d tokens\n", result.MessagesBefore, result.OriginalTokens)
	env.Output("After:  %d messages, ~%d tokens\n", result.MessagesAfter, result.CompressedTokens)
	env.Output("Saved:  ~%d tokens (%.1f%% reduction)",
		result.OriginalTokens-result.CompressedTokens,
		float64(result.OriginalTokens-result.CompressedTokens)/float64(result.OriginalTokens)*100)

	return nil
}

// StatsCommand shows context statistics.
type StatsCommand struct{}

func NewStatsCommand() *StatsCommand {
	return &StatsCommand{}
}

func (c *StatsCommand) Name() string        { return "stats" }
func (c *StatsCommand) Aliases() []string   { return []string{"context", "tokens"} }
func (c *StatsCommand) Description() string { return "Show context statistics" }
func (c *StatsCommand) Usage() string       { return "/stats" }

func (c *StatsCommand) Execute(ctx context.Context, env *commands.Environment, args []string) error {
	ag, ok := env.Agent.(*agent.Agent)
	if !ok || ag == nil {
		env.Output("Stats requires an active agent session.")
		return nil
	}

	stats := ag.GetContextStats()

	env.Output("Context Statistics:\n")
	env.Output("  Messages:         %d\n", stats["message_count"])
	env.Output("  Estimated tokens: %d\n", stats["estimated_tokens"])

	if loopCount, ok := stats["loop_total_calls"]; ok {
		env.Output("  Tool calls:       %d\n", loopCount)
	}
	if uniqueCalls, ok := stats["loop_unique_calls"]; ok {
		env.Output("  Unique calls:     %d\n", uniqueCalls)
	}

	return nil
}
