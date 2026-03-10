//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/testutil"
)

// TestE2E_ProviderSmoke tests each available provider with a simple chat request.
//
// Run: go test -tags=e2e -v -run TestE2E_ProviderSmoke ./tests/e2e/
func TestE2E_ProviderSmoke(t *testing.T) {
	e2eCfg, err := testutil.LoadE2EConfig()
	if err != nil {
		t.Fatalf("Failed to load e2e config: %v", err)
	}

	profiles := e2eCfg.AllProfiles()
	if len(profiles) == 0 {
		t.Skip("No provider API keys configured")
	}

	for name, profile := range profiles {
		t.Run(name, func(t *testing.T) {
			if profile.APIKey == "" {
				t.Skipf("No API key for profile %s", name)
			}

			p, err := providers.NewProvider(profile)
			if err != nil {
				t.Fatalf("Failed to create provider: %v", err)
			}
			if p.Name() == "" {
				t.Fatal("Provider name should not be empty")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			msgs := []protocol.Message{
				protocol.NewSystemMessage("You are a concise assistant."),
				protocol.NewUserMessage("Reply with one word: pong"),
			}

			events, err := p.StreamChat(ctx, msgs, nil)
			if err != nil {
				if shouldSkipProviderError(name, err) {
					t.Skipf("Skipping: %v", err)
				}
				t.Fatalf("StreamChat error: %v", err)
			}

			gotContent := false
			gotFinish := false
			for e := range events {
				if e.Type == protocol.EventTypeError {
					if shouldSkipProviderError(name, e.Error) {
						t.Skipf("Skipping: %v", e.Error)
					}
					t.Fatalf("Stream error: %v", e.Error)
				}
				if e.Type == protocol.EventTypeContentDelta && e.ContentPartDelta != nil && e.ContentPartDelta.Text != "" {
					gotContent = true
				}
				if e.Type == protocol.EventTypeFinish {
					gotFinish = true
				}
			}

			if !gotContent && !gotFinish {
				t.Fatal("Expected content or finish events from stream")
			}
		})
	}
}
