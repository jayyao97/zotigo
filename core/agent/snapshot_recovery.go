package agent

import "github.com/jayyao97/zotigo/core/protocol"

// InterruptPendingSnapshot makes a persisted paused turn safe to abandon after
// its owning runtime is lost. It records denied tool results so the next turn
// does not inherit unanswered tool calls.
func InterruptPendingSnapshot(snapshot Snapshot, reason string) Snapshot {
	if snapshot.State != StatePaused {
		return snapshot
	}
	actions := orderedActions(append(append([]*PendingAction(nil), snapshot.DeferredActions...), snapshot.PendingActions...))
	results := make([]protocol.ToolResult, 0, len(actions))
	for _, action := range actions {
		results = append(results, skippedToolResult(action, reason))
	}
	if len(results) > 0 {
		snapshot.History = append(snapshot.History, protocol.NewToolMessage(results))
	}
	snapshot.State = StateIdle
	snapshot.PendingActions = nil
	snapshot.DeferredActions = nil
	return snapshot
}
