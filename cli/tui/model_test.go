package tui

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/jayyao97/zotigo/core/agent"
)

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
			name: "bypass policy suppresses classifier status",
			desc: agent.Description{
				Provider:            "anthropic-chat",
				Model:               "deepseek-v4-flash",
				ApprovalPolicy:      agent.ApprovalPolicyBypass,
				ClassifierEnabled:   true,
				ClassifierAvailable: true,
				ClassifierProvider:  "anthropic",
				ClassifierModel:     "deepseek-v4-flash",
			},
			wantIn:  []string{"bypass_permissions", "Classifier:", "bypassed"},
			wantOut: []string{"threshold="},
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

func TestWriteInputFooterWarnsWhenPermissionsBypassed(t *testing.T) {
	ta := textarea.New()
	ta.SetWidth(80)
	model := &Model{input: ta, bypassPermissions: true}
	var output strings.Builder

	model.writeInputFooter(&output)

	if !strings.Contains(output.String(), "BYPASS PERMISSIONS") {
		t.Fatalf("expected bypass warning, got %q", output.String())
	}
	if strings.Contains(output.String(), "Auto-approve") {
		t.Fatalf("bypass warning must not be labeled auto-approve: %q", output.String())
	}
}

func TestNewModelResumesPendingActionsWithoutApprovalInBypassMode(t *testing.T) {
	ag := newDisplayLogTestAgent(t)
	ag.SetApprovalPolicy(agent.ApprovalPolicyBypass)
	ag.Restore(agent.Snapshot{
		State: agent.StatePaused,
		PendingActions: []*agent.PendingAction{
			{ToolCallID: "call-1", Name: "read_file"},
		},
	})

	model := NewModel(ag, nil, "", nil)
	if model.approving {
		t.Fatal("bypassed resume must not reopen the approval UI")
	}
	if !model.resumeBypass || !model.thinking {
		t.Fatalf("bypassed resume state = resume:%v thinking:%v, want both true", model.resumeBypass, model.thinking)
	}
}

func TestBypassResumeStartsBeforeShiftTabCanChangePolicy(t *testing.T) {
	ag := newDisplayLogTestAgent(t)
	ag.SetApprovalPolicy(agent.ApprovalPolicyBypass)
	ag.Restore(agent.Snapshot{
		State: agent.StatePaused,
		PendingActions: []*agent.PendingAction{
			{ToolCallID: "call-1", Name: "missing_tool"},
		},
	})
	model := NewModel(ag, nil, "", nil)

	initMsg := model.Init()()
	batch, ok := initMsg.(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatalf("Init message = %T %#v, want two-command batch", initMsg, initMsg)
	}
	updated, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	model = updated.(*Model)
	if got := model.agent.Describe().ApprovalPolicy; got != agent.ApprovalPolicyAuto {
		t.Fatalf("policy after Shift+Tab = %q, want auto", got)
	}

	msg := batch[1]()
	stream, ok := msg.(streamReadyMsg)
	if !ok {
		t.Fatalf("resume command returned %T (%v), want streamReadyMsg", msg, msg)
	}
	for range stream {
	}
	if model.resumeBypass {
		t.Fatal("resumeBypass must be cleared after startup")
	}
}

func TestPasteMsgInsertsMultilineTextOnce(t *testing.T) {
	ta := textarea.New()
	ta.Focus()
	ta.Prompt = ""
	ta.SetWidth(80)
	ta.SetHeight(1)

	m := &Model{input: ta}
	pasted := "first line\nsecond line\nthird line"

	updated, _ := m.Update(tea.PasteMsg{Content: pasted})
	got := updated.(*Model).input.Value()

	if got != pasted {
		t.Fatalf("paste should insert content once, got %q", got)
	}
}

func TestShouldUseViewportRendererDisablesJetBrainsTerminal(t *testing.T) {
	t.Setenv("TERMINAL_EMULATOR", "JetBrains-JediTerm")
	t.Setenv("TERM_PROGRAM", "")

	if shouldUseViewportRenderer() {
		t.Fatal("expected JetBrains terminal to use inline renderer")
	}
}

func TestShouldUseViewportRendererAllowsOtherTerminals(t *testing.T) {
	t.Setenv("TERMINAL_EMULATOR", "")
	t.Setenv("TERM_PROGRAM", "iTerm.app")

	if !shouldUseViewportRenderer() {
		t.Fatal("expected non-JetBrains terminal to use viewport renderer")
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
