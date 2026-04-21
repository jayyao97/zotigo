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

func TestRenderAgentBanner(t *testing.T) {
	tests := []struct {
		name    string
		desc    agent.Description
		wantIn  []string
		wantOut []string
	}{
		{
			name: "full config with classifier",
			desc: agent.Description{
				Provider:            "openai-response",
				Model:               "gpt-5-codex",
				ThinkingLevel:       "low",
				ApprovalPolicy:      agent.ApprovalPolicyAuto,
				ClassifierEnabled:   true,
				ClassifierAvailable: true,
				ClassifierProvider:  "openai",
				ClassifierModel:     "gpt-4o-mini",
				ReviewThreshold:     "medium",
			},
			wantIn: []string{"openai-response", "gpt-5-codex", "low", "gpt-4o-mini", "threshold=medium"},
		},
		{
			name: "classifier disabled",
			desc: agent.Description{
				Provider:          "openai-chat",
				Model:             "gpt-4o",
				ApprovalPolicy:    agent.ApprovalPolicyManual,
				ClassifierEnabled: false,
			},
			wantIn:  []string{"openai-chat", "gpt-4o", "off"},
			wantOut: []string{"threshold="},
		},
		{
			name: "classifier enabled but unavailable",
			desc: agent.Description{
				Provider:            "openai-chat",
				Model:               "gpt-4o",
				ApprovalPolicy:      agent.ApprovalPolicyAuto,
				ClassifierEnabled:   true,
				ClassifierAvailable: false,
			},
			wantIn: []string{"enabled but unavailable"},
		},
		{
			name: "no thinking level suppresses the row",
			desc: agent.Description{
				Provider: "openai-chat",
				Model:    "gpt-4o-mini",
			},
			wantOut: []string{"Thinking:"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := renderAgentBanner(tc.desc)
			for _, s := range tc.wantIn {
				if !containsSubstr(got, s) {
					t.Errorf("expected %q in banner, got:\n%s", s, got)
				}
			}
			for _, s := range tc.wantOut {
				if containsSubstr(got, s) {
					t.Errorf("did not expect %q in banner, got:\n%s", s, got)
				}
			}
		})
	}
}

func containsSubstr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
