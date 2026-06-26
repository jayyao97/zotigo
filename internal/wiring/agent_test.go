package wiring

import (
	"context"
	"testing"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
)

type stubProvider struct{}

func (p *stubProvider) Name() string { return "stub" }

func (p *stubProvider) StreamChat(context.Context, []protocol.Message, []tools.Tool, ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	ch := make(chan protocol.Event)
	close(ch)
	return ch, nil
}

func TestNewAgentClassifierResolveErrorFallsBackToManual(t *testing.T) {
	providers.Register("wiring-test-main-resolve-error", func(config.ProfileConfig) (providers.Provider, error) {
		return &stubProvider{}, nil
	})

	profile := config.ProfileConfig{
		Provider: "wiring-test-main-resolve-error",
		Safety: config.SafetyProfileConfig{
			Classifier: config.SafetyClassifierConfig{
				Enabled: config.BoolPtr(true),
				Profile: "missing-classifier",
			},
		},
	}
	cfg := &config.Config{
		Profiles: map[string]config.ProfileConfig{
			"main": profile,
		},
	}
	exec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatalf("failed to create executor: %v", err)
	}

	ag, err := NewAgent(AgentConfig{
		Config:              cfg,
		ProfileName:         "main",
		Profile:             profile,
		Executor:            exec,
		ApprovalPolicy:      agent.ApprovalPolicyAuto,
		ConfigureClassifier: true,
	})
	if err != nil {
		t.Fatalf("NewAgent failed: %v", err)
	}

	if got := ag.Describe().ApprovalPolicy; got != agent.ApprovalPolicyManual {
		t.Fatalf("expected manual approval fallback, got %s", got)
	}
}

func TestNewAgentClassifierProviderErrorKeepsPolicy(t *testing.T) {
	providers.Register("wiring-test-main-provider-error", func(config.ProfileConfig) (providers.Provider, error) {
		return &stubProvider{}, nil
	})

	profile := config.ProfileConfig{
		Provider: "wiring-test-main-provider-error",
		Safety: config.SafetyProfileConfig{
			Classifier: config.SafetyClassifierConfig{
				Enabled: config.BoolPtr(true),
				Profile: "classifier",
			},
		},
	}
	cfg := &config.Config{
		Profiles: map[string]config.ProfileConfig{
			"main":       profile,
			"classifier": {Provider: "wiring-test-missing-classifier-provider"},
		},
	}
	exec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatalf("failed to create executor: %v", err)
	}

	ag, err := NewAgent(AgentConfig{
		Config:              cfg,
		ProfileName:         "main",
		Profile:             profile,
		Executor:            exec,
		ApprovalPolicy:      agent.ApprovalPolicyAuto,
		ConfigureClassifier: true,
	})
	if err != nil {
		t.Fatalf("NewAgent failed: %v", err)
	}

	desc := ag.Describe()
	if desc.ApprovalPolicy != agent.ApprovalPolicyAuto {
		t.Fatalf("expected auto approval policy, got %s", desc.ApprovalPolicy)
	}
	if desc.ClassifierAvailable {
		t.Fatal("expected classifier to be unavailable")
	}
}
