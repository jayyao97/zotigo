//go:build e2e

package providers_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/testutil"

	_ "github.com/jayyao97/zotigo/core/providers/anthropic"
	_ "github.com/jayyao97/zotigo/core/providers/gemini"
	_ "github.com/jayyao97/zotigo/core/providers/openai"
)

func TestE2E_ProviderSmoke(t *testing.T) {
	e2eCfg, err := testutil.LoadE2EConfig()
	if err != nil {
		t.Fatalf("Failed to load E2E config: %v", err)
	}

	profiles := availableProfiles(e2eCfg)
	if len(profiles) == 0 {
		t.Skip("No provider API keys configured in e2e.config.json (or legacy config.json)")
	}

	for providerName, profile := range profiles {
		t.Run(providerName, func(t *testing.T) {
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
				if shouldSkipProviderError(providerName, err) {
					t.Skipf("Skipping provider smoke test: %v", err)
				}
				t.Fatalf("StreamChat returned error: %v", err)
			}

			gotContent := false
			gotFinish := false
			for e := range events {
				if e.Type == protocol.EventTypeError {
					if shouldSkipProviderError(providerName, e.Error) {
						t.Skipf("Skipping provider smoke test: %v", e.Error)
					}
					t.Fatalf("Provider stream returned error event: %v", e.Error)
				}
				if e.Type == protocol.EventTypeContentDelta && e.ContentPartDelta != nil && e.ContentPartDelta.Text != "" {
					gotContent = true
				}
				if e.Type == protocol.EventTypeFinish {
					gotFinish = true
				}
			}

			if !gotContent && !gotFinish {
				t.Fatalf("Expected content or finish events from provider stream")
			}
		})
	}
}

func shouldSkipProviderError(providerName string, err error) bool {
	if err == nil {
		return false
	}
	if providerName != "openai" {
		return false
	}
	msg := strings.ToLower(fmt.Sprintf("%v", err))
	return strings.Contains(msg, "only supported in v1/responses")
}

func availableProfiles(cfg *testutil.E2EConfig) map[string]config.ProfileConfig {
	profiles := make(map[string]config.ProfileConfig)

	if cfg.OpenAI != nil && cfg.OpenAI.APIKey != "" {
		profiles["openai"] = config.ProfileConfig{
			Provider: "openai",
			Model:    cfg.OpenAI.Model,
			APIKey:   cfg.OpenAI.APIKey,
			BaseURL:  cfg.OpenAI.BaseURL,
		}
	}

	if cfg.Anthropic != nil && cfg.Anthropic.APIKey != "" {
		provider := "anthropic"
		if isOpenRouterBaseURL(cfg.Anthropic.BaseURL) {
			provider = "openai"
		}
		profiles["anthropic"] = config.ProfileConfig{
			Provider: provider,
			Model:    cfg.Anthropic.Model,
			APIKey:   cfg.Anthropic.APIKey,
			BaseURL:  cfg.Anthropic.BaseURL,
		}
	}

	if cfg.Gemini != nil && cfg.Gemini.APIKey != "" {
		provider := "gemini"
		if isOpenRouterBaseURL(cfg.Gemini.BaseURL) {
			provider = "openai"
		}
		profiles["gemini"] = config.ProfileConfig{
			Provider: provider,
			Model:    cfg.Gemini.Model,
			APIKey:   cfg.Gemini.APIKey,
			BaseURL:  cfg.Gemini.BaseURL,
		}
	}

	return profiles
}

func isOpenRouterBaseURL(baseURL string) bool {
	return strings.Contains(strings.ToLower(baseURL), "openrouter.ai")
}
