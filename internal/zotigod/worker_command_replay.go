package zotigod

import (
	"context"
	"fmt"
	"net/http"

	_ "github.com/jayyao97/zotigo/core/providers/anthropic"
	_ "github.com/jayyao97/zotigo/core/providers/deepseek"
	_ "github.com/jayyao97/zotigo/core/providers/gemini"
	_ "github.com/jayyao97/zotigo/core/providers/openai"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
	"github.com/jayyao97/zotigo/internal/zotigod/workerbase"
)

func replayWorkerCommands(ctx context.Context, client *http.Client, daemonURL string, sessionID string, runtime *workerRuntime, cursor workerbase.CommandCursor) (workerbase.CommandCursor, error) {
	type pendingProfile struct {
		sequence   uint64
		completion <-chan error
	}
	pendingProfiles := make([]pendingProfile, 0)
	appliedCommands, err := loadAppliedWorkerCommandSequences(ctx, runtime)
	if err != nil {
		return cursor, err
	}
	flushProfiles := func() error {
		for _, pending := range pendingProfiles {
			select {
			case err := <-pending.completion:
				if err != nil {
					return err
				}
				cursor.Sequence = pending.sequence
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		pendingProfiles = pendingProfiles[:0]
		return nil
	}

	for {
		previousOffset := cursor.Offset
		commands, err := workerbase.FetchCommands(ctx, client, daemonURL, sessionID, cursor)
		if err != nil {
			return cursor, err
		}
		for _, command := range commands.Commands {
			if command.Sequence <= cursor.Sequence {
				continue
			}
			// Recovery can rewind past an unacknowledged command. Later commands
			// with durable execution evidence must not run again across that gap.
			if appliedCommands[command.Sequence] {
				if err := flushProfiles(); err != nil {
					return cursor, err
				}
				cursor.Sequence = command.Sequence
				continue
			}
			if command.Type != wire.CommandProfile {
				if err := flushProfiles(); err != nil {
					return cursor, err
				}
			}
			completion, err := runtime.handleCommand(ctx, command)
			if err != nil {
				return cursor, err
			}
			if completion != nil {
				pendingProfiles = append(pendingProfiles, pendingProfile{sequence: command.Sequence, completion: completion})
				continue
			}
			cursor.Sequence = command.Sequence
		}
		if err := flushProfiles(); err != nil {
			return cursor, err
		}
		cursor.Offset = commands.NextOffset
		if err := workerbase.SaveCommandCursor(sessionID, cursor); err != nil {
			return cursor, err
		}
		if commands.NextOffset == previousOffset || len(commands.Commands) < wire.MaxCommandsLimit {
			return cursor, nil
		}
	}
}

func loadAppliedWorkerCommandSequences(ctx context.Context, runtime *workerRuntime) (map[uint64]bool, error) {
	if runtime == nil || runtime.display == nil || runtime.display.Items == nil {
		return nil, nil
	}
	items, _, err := runtime.display.Items.LoadItems(ctx, runtime.sessionID)
	if err != nil {
		return nil, fmt.Errorf("load worker command completions: %w", err)
	}
	return workerbase.AppliedCommandSequences(items), nil
}
