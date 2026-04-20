package providers_test

import (
	"context"
	"testing"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
)

// MockProvider is a simple implementation for testing the registry.
type MockProvider struct {
	name string
}

func (m *MockProvider) StreamChat(ctx context.Context, messages []protocol.Message, tools []tools.Tool, _ ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	ch := make(chan protocol.Event)
	close(ch)
	return ch, nil
}

func (m *MockProvider) Name() string {
	return m.name
}

func TestProviderRegistry(t *testing.T) {
	const providerName = "mock_test"

	// Register the mock provider
	providers.Register(providerName, func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &MockProvider{name: providerName}, nil
	})

	// Try to retrieve it using a profile
	profile := config.ProfileConfig{
		Provider: providerName,
		Model:    "test-model",
		APIKey:   "dummy",
	}

	p, err := providers.NewProvider(profile)
	if err != nil {
		t.Fatalf("Failed to create provider: %v", err)
	}

	if p.Name() != providerName {
		t.Errorf("Expected provider name %s, got %s", providerName, p.Name())
	}

	// Try to retrieve a non-existent provider
	badProfile := config.ProfileConfig{Provider: "non_existent"}
	_, err = providers.NewProvider(badProfile)
	if err == nil {
		t.Error("Expected error for non-existent provider, got nil")
	}
}
