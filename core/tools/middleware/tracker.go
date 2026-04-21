// Package middleware holds agent-level tool-call middleware that
// implement cross-cutting concerns (read tracking, metrics, audit, ...)
// outside of the individual tool implementations. Each middleware here
// plugs into agent.WithHook.
package middleware

import (
	"context"
	"fmt"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/tools"
)

// Per-tool metadata: which tools read files, which mutate, and which
// argument key carries the path. Deliberately a centralized table
// rather than a per-tool declaration — this lets the middleware stay
// the single source of truth for "what counts as a read-before-write
// contract". Adding a tool that should participate = one line here.
var (
	// Tools that should Mark the path after a successful call.
	readerTools = map[string]string{
		"read_file": "path",
	}
	// Tools that must have a prior successful read (and no external
	// change) for the given path argument.
	mutatorTools = map[string]string{
		"edit": "path",
	}
	// Tools that only need the read-first check when the target file
	// already exists (creating a new file is always allowed).
	mutatorIfExists = map[string]string{
		"write_file": "path",
	}
)

// ReadTracker is the tool-call hook that enforces "read before edit"
// and records snapshots for external-change detection. Construct one
// per Agent and register via agent.WithHook.
func ReadTracker(tracker *tools.ReadTracker) agent.Hook {
	if tracker == nil {
		return nil
	}
	return func(next agent.Next) agent.Next {
		return func(ctx context.Context, call *agent.ToolCall) (any, error) {
			// Pre-check for mutators.
			if key, ok := mutatorTools[call.Name]; ok {
				path := tools.StringArg(call.Arguments, key)
				if err := checkFreshness(ctx, tracker, call.Executor, path); err != nil {
					return nil, err
				}
			}
			if key, ok := mutatorIfExists[call.Name]; ok {
				path := tools.StringArg(call.Arguments, key)
				// Only enforce when the target already exists; writing
				// a fresh file has nothing to compare against.
				if info, statErr := call.Executor.Stat(ctx, path); statErr == nil && info != nil && !info.IsDir {
					if err := checkFreshness(ctx, tracker, call.Executor, path); err != nil {
						return nil, err
					}
				}
			}

			res, err := next(ctx, call)

			// Post-record for readers.
			if err == nil {
				if key, ok := readerTools[call.Name]; ok {
					path := tools.StringArg(call.Arguments, key)
					markRead(ctx, tracker, call.Executor, path)
				}
			}
			return res, err
		}
	}
}

func checkFreshness(ctx context.Context, tracker *tools.ReadTracker, exec executor.Executor, path string) error {
	if path == "" {
		return nil
	}
	snap, read := tracker.Snapshot(path)
	if !read {
		return fmt.Errorf("must call read_file on %s before mutating so the exact current contents are known", path)
	}
	info, statErr := exec.Stat(ctx, path)
	if statErr != nil || info == nil {
		return fmt.Errorf("file %s is no longer accessible since it was read; call read_file again to refresh", path)
	}
	if info.Size != snap.Size || !info.ModTime.Equal(snap.Mtime) {
		return fmt.Errorf("file %s changed on disk since it was read (size %d→%d, mtime %s→%s); call read_file again before mutating",
			path, snap.Size, info.Size, snap.Mtime.Format("15:04:05"), info.ModTime.Format("15:04:05"))
	}
	return nil
}

func markRead(ctx context.Context, tracker *tools.ReadTracker, exec executor.Executor, path string) {
	if path == "" {
		return
	}
	snap := tools.ReadSnapshot{}
	if info, err := exec.Stat(ctx, path); err == nil && info != nil {
		snap.Size = info.Size
		snap.Mtime = info.ModTime
	}
	tracker.Mark(path, snap)
}
