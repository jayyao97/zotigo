package builtin

import (
	"context"
	"fmt"
	"strings"

	"github.com/jayyao97/zotigo/cli/commands"
)

// SnapshotCommand creates a code snapshot using snap-commit.
type SnapshotCommand struct{}

func NewSnapshotCommand() *SnapshotCommand {
	return &SnapshotCommand{}
}

func (c *SnapshotCommand) Name() string        { return "snapshot" }
func (c *SnapshotCommand) Aliases() []string   { return []string{"snap", "save"} }
func (c *SnapshotCommand) Description() string { return "Create a code snapshot for easy rollback" }
func (c *SnapshotCommand) Usage() string       { return "/snapshot [message]" }

func (c *SnapshotCommand) Execute(ctx context.Context, env *commands.Environment, args []string) error {
	// Check if snap-commit is available
	if env.Exec == nil {
		env.Output("Snapshot requires shell execution capability.")
		return nil
	}

	// Build the command
	cmd := "snap-commit store"
	if len(args) > 0 {
		message := strings.Join(args, " ")
		cmd = fmt.Sprintf("snap-commit store -m %q", message)
	}

	// Execute snap-commit
	result, err := env.Exec(ctx, cmd)
	if err != nil {
		// Check if snap-commit is not installed
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "command not found") {
			env.Output("snap-commit is not installed.\n\nInstall with: go install github.com/jayyao97/snap-commit@latest")
			return nil
		}
		return fmt.Errorf("snapshot failed: %w", err)
	}

	env.Output("Snapshot created: %s", strings.TrimSpace(result))
	return nil
}

// RewindCommand restores code to a previous snapshot.
type RewindCommand struct{}

func NewRewindCommand() *RewindCommand {
	return &RewindCommand{}
}

func (c *RewindCommand) Name() string        { return "rewind" }
func (c *RewindCommand) Aliases() []string   { return []string{"restore", "undo"} }
func (c *RewindCommand) Description() string { return "Restore code to a previous snapshot" }
func (c *RewindCommand) Usage() string       { return "/rewind [snapshot_id]" }

func (c *RewindCommand) Execute(ctx context.Context, env *commands.Environment, args []string) error {
	if env.Exec == nil {
		env.Output("Rewind requires shell execution capability.")
		return nil
	}

	// Build the command
	cmd := "snap-commit restore -d" // -d for dry-run safety by default
	if len(args) > 0 {
		// If snapshot ID provided, use it
		cmd = fmt.Sprintf("snap-commit restore %s", args[0])
	}

	result, err := env.Exec(ctx, cmd)
	if err != nil {
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "command not found") {
			env.Output("snap-commit is not installed.\n\nInstall with: go install github.com/jayyao97/snap-commit@latest")
			return nil
		}
		return fmt.Errorf("rewind failed: %w", err)
	}

	env.Output("%s", strings.TrimSpace(result))
	return nil
}

// SnapshotsCommand lists all available snapshots.
type SnapshotsCommand struct{}

func NewSnapshotsCommand() *SnapshotsCommand {
	return &SnapshotsCommand{}
}

func (c *SnapshotsCommand) Name() string        { return "snapshots" }
func (c *SnapshotsCommand) Aliases() []string   { return []string{"snaps", "history"} }
func (c *SnapshotsCommand) Description() string { return "List all available snapshots" }
func (c *SnapshotsCommand) Usage() string       { return "/snapshots" }

func (c *SnapshotsCommand) Execute(ctx context.Context, env *commands.Environment, args []string) error {
	if env.Exec == nil {
		env.Output("Snapshots requires shell execution capability.")
		return nil
	}

	result, err := env.Exec(ctx, "snap-commit list")
	if err != nil {
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "command not found") {
			env.Output("snap-commit is not installed.\n\nInstall with: go install github.com/jayyao97/snap-commit@latest")
			return nil
		}
		return fmt.Errorf("list snapshots failed: %w", err)
	}

	if strings.TrimSpace(result) == "" {
		env.Output("No snapshots found. Create one with /snapshot")
		return nil
	}

	env.Output("Available snapshots:\n%s", result)
	return nil
}
