package deepseek

import (
	"context"
	"testing"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
)

type captureProvider struct {
	resolved providers.ResolvedOptions
}

func (p *captureProvider) Name() string { return "anthropic-chat" }

func (p *captureProvider) StreamChat(
	_ context.Context,
	_ []protocol.Message,
	_ []tools.Tool,
	opts ...providers.StreamChatOption,
) (<-chan protocol.Event, error) {
	p.resolved = providers.ResolveOptions(opts)
	events := make(chan protocol.Event)
	close(events)
	return events, nil
}

func TestProviderRemovesForcedToolChoice(t *testing.T) {
	tests := []struct {
		name   string
		choice providers.ToolChoice
	}{
		{name: "auto", choice: providers.ToolChoice{}},
		{name: "required", choice: providers.ToolChoice{Mode: providers.ToolChoiceRequired}},
		{name: "specific", choice: providers.ToolChoice{Mode: providers.ToolChoiceSpecific, Name: "record_decision"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inner := &captureProvider{}
			provider := &Provider{Provider: inner}
			_, err := provider.StreamChat(
				context.Background(),
				nil,
				nil,
				providers.WithToolChoice(test.choice),
				providers.WithReasoningEffort("low"),
			)
			if err != nil {
				t.Fatalf("StreamChat: %v", err)
			}
			if inner.resolved.ToolChoice.Mode != providers.ToolChoiceAuto {
				t.Fatalf("tool choice = %v, want auto", inner.resolved.ToolChoice.Mode)
			}
			if inner.resolved.ReasoningEffort != "low" {
				t.Fatalf("reasoning effort = %q, want low", inner.resolved.ReasoningEffort)
			}
		})
	}
}

func TestWithDefaults(t *testing.T) {
	got := withDefaults(config.ProfileConfig{})
	if got.BaseURL != defaultBaseURL {
		t.Fatalf("default BaseURL = %q, want %q", got.BaseURL, defaultBaseURL)
	}

	const customURL = "https://deepseek.example/anthropic"
	got = withDefaults(config.ProfileConfig{BaseURL: customURL})
	if got.BaseURL != customURL {
		t.Fatalf("custom BaseURL = %q, want %q", got.BaseURL, customURL)
	}
}

func TestFactoryRegistration(t *testing.T) {
	provider, err := providers.NewProvider(config.ProfileConfig{
		Provider: ProviderName,
		Model:    "deepseek-v4-flash",
		APIKey:   "test-key",
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if provider.Name() != ProviderName {
		t.Fatalf("provider name = %q, want %q", provider.Name(), ProviderName)
	}
}
