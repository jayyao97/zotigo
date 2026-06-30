package tui

import (
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
