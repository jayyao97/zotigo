package builtin

import (
	"context"

	"github.com/jayyao97/zotigo/cli/commands"
)

// CompressCommand asks the session service to compact the conversation.
// The result arrives in the session stream as a compaction notice.
type CompressCommand struct{}

func NewCompressCommand() *CompressCommand {
	return &CompressCommand{}
}

func (c *CompressCommand) Name() string        { return "compress" }
func (c *CompressCommand) Aliases() []string   { return []string{"summarize", "compact"} }
func (c *CompressCommand) Description() string { return "Manually compress conversation history" }
func (c *CompressCommand) Usage() string       { return "/compress" }

func (c *CompressCommand) Execute(ctx context.Context, env *commands.Environment, args []string) error {
	if env.Compact == nil {
		env.Output("Compress requires an active session.")
		return nil
	}
	return env.Compact()
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
	if env.Usage == nil {
		env.Output("Stats requires an active session.")
		return nil
	}
	report := env.Usage()
	env.Output("Context Statistics:\n")
	if report.ContextTokens == 0 {
		env.Output("  Context usage:    unavailable until the session reports it\n")
	} else if report.ContextWindow > 0 {
		env.Output("  Context tokens:   %s / %s (%.0f%%)\n", formatNumber(report.ContextTokens), formatNumber(report.ContextWindow),
			float64(report.ContextTokens)/float64(report.ContextWindow)*100)
	} else {
		env.Output("  Context tokens:   %s\n", formatNumber(report.ContextTokens))
	}
	env.Output("  Completed turns:  %d\n", report.Turns)
	return nil
}
