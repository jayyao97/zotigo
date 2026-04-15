package tui

import (
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/session"
)

func TestConvertAgentTurns(t *testing.T) {
	now := time.Now()
	turns := []agent.TurnAudit{
		{
			ID:                "turn_1",
			CreatedAt:         now,
			UpdatedAt:         now,
			UserPromptSummary: "write a file",
			SnapshotStatus:    agent.SnapshotStatusCreated,
			SnapshotID:        "snap-123",
			SafetyEvents: []agent.AuditEvent{
				{
					Timestamp:       now,
					TurnID:          "turn_1",
					ToolCallID:      "call_1",
					ToolName:        "write_file",
					DecisionSource:  agent.SafetyDecisionSourceHardRule,
					Decision:        agent.SafetyClassifierDecisionAskUser,
					Reason:          "mutating tool requires approval",
					SnapshotStatus:  agent.SnapshotStatusCreated,
					ContextSummary:  agent.AuditContextSummary{UserPrompt: "write a file", Trigger: "protected write"},
					ToolArgsSummary: `{"path":"note.txt"}`,
				},
			},
		},
	}

	got := convertAgentTurns(turns)
	if len(got) != 1 {
		t.Fatalf("Expected 1 turn, got %d", len(got))
	}
	if got[0].ID != "turn_1" {
		t.Fatalf("Expected turn_1, got %s", got[0].ID)
	}
	if got[0].SnapshotStatus != session.SnapshotStatusCreated {
		t.Fatalf("Expected created snapshot status, got %s", got[0].SnapshotStatus)
	}
	if len(got[0].SafetyEvents) != 1 {
		t.Fatalf("Expected 1 safety event, got %d", len(got[0].SafetyEvents))
	}
	if got[0].SafetyEvents[0].Decision != session.SafetyDecisionAskUser {
		t.Fatalf("Expected ask_user, got %s", got[0].SafetyEvents[0].Decision)
	}
	if got[0].SafetyEvents[0].ContextSummary.Trigger != "protected write" {
		t.Fatalf("Unexpected trigger summary: %s", got[0].SafetyEvents[0].ContextSummary.Trigger)
	}
}
