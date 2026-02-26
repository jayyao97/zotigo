package builtin

import (
	"github.com/jayyao97/zotigo/cli/commands"
)

// RegisterAll registers all builtin commands with the registry.
func RegisterAll(registry *commands.Registry) {
	// Help needs registry reference for listing commands
	registry.Register(NewHelpCommand(registry))

	// Basic commands
	registry.Register(NewClearCommand())
	registry.Register(NewModelCommand())
	registry.Register(NewCostCommand())

	// Context management commands
	registry.Register(NewCompressCommand())
	registry.Register(NewStatsCommand())

	// Snapshot commands (requires snap-commit)
	registry.Register(NewSnapshotCommand())
	registry.Register(NewRewindCommand())
	registry.Register(NewSnapshotsCommand())

	// Skills commands
	registry.Register(NewSkillsCommand())
}
