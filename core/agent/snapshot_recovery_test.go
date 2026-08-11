package agent_test

import (
	"testing"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/protocol"
)

func TestInterruptPendingSnapshotRecordsDeniedResultsAndReturnsIdle(t *testing.T) {
	snapshot := agent.Snapshot{
		State: agent.StatePaused,
		PendingActions: []*agent.PendingAction{{
			ToolCallID: "call-2",
			Name:       "shell",
			Order:      2,
		}},
		DeferredActions: []*agent.PendingAction{{
			ToolCallID: "call-1",
			Name:       "read_file",
			Order:      1,
		}},
	}

	recovered := agent.InterruptPendingSnapshot(snapshot, "worker restarted")
	if recovered.State != agent.StateIdle || len(recovered.PendingActions) != 0 || len(recovered.DeferredActions) != 0 {
		t.Fatalf("unexpected recovered snapshot: %#v", recovered)
	}
	if len(recovered.History) != 1 || recovered.History[0].Role != protocol.RoleTool || len(recovered.History[0].Content) != 2 {
		t.Fatalf("unexpected recovery history: %#v", recovered.History)
	}
	for index, wantID := range []string{"call-1", "call-2"} {
		result := recovered.History[0].Content[index].ToolResult
		if result == nil || result.ToolCallID != wantID || !result.IsError || result.Type != protocol.ToolResultTypeExecutionDenied {
			t.Fatalf("unexpected recovery result %d: %#v", index, result)
		}
	}
}
