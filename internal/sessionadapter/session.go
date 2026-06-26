package sessionadapter

import (
	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/session"
)

func LastUserPrompt(history []protocol.Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == protocol.RoleUser {
			return history[i].String()
		}
	}
	return ""
}

func ApplySnapshot(sess *session.Session, snap agent.Snapshot, lastPrompt string) {
	sess.AgentSnapshot = snap
	sess.Turns = ConvertAgentTurns(snap.Turns)
	if lastPrompt != "" {
		sess.LastPrompt = lastPrompt
	}
}

func ConvertAgentTurns(turns []agent.TurnAudit) []session.Turn {
	result := make([]session.Turn, len(turns))
	for i, turn := range turns {
		result[i] = session.Turn{
			ID:                turn.ID,
			CreatedAt:         turn.CreatedAt,
			UpdatedAt:         turn.UpdatedAt,
			UserPromptSummary: turn.UserPromptSummary,
			SnapshotStatus:    session.SnapshotStatus(turn.SnapshotStatus),
			SnapshotID:        turn.SnapshotID,
		}
		if len(turn.SafetyEvents) > 0 {
			result[i].SafetyEvents = make([]session.SafetyEvent, len(turn.SafetyEvents))
			for j, event := range turn.SafetyEvents {
				result[i].SafetyEvents[j] = session.SafetyEvent{
					Timestamp:          event.Timestamp,
					TurnID:             event.TurnID,
					ToolCallID:         event.ToolCallID,
					ToolName:           event.ToolName,
					ToolArgsSummary:    event.ToolArgsSummary,
					DecisionSource:     session.SafetyDecisionSource(event.DecisionSource),
					Decision:           session.SafetyDecision(event.Decision),
					Reason:             event.Reason,
					RiskLevel:          event.RiskLevel,
					SnapshotStatus:     session.SnapshotStatus(event.SnapshotStatus),
					SnapshotID:         event.SnapshotID,
					ClassifierProvider: event.ClassifierProvider,
					ClassifierModel:    event.ClassifierModel,
					ContextSummary: session.ContextSummary{
						UserPrompt:    event.ContextSummary.UserPrompt,
						RecentActions: event.ContextSummary.RecentActions,
						Trigger:       event.ContextSummary.Trigger,
					},
					RawContext: event.RawContext,
				}
			}
		}
	}
	return result
}
