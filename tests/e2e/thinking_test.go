//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/testutil"
)

// TestE2E_ThinkingMode tests that thinking/reasoning mode works for each provider
// that has a thinking profile configured.
//
// Run: go test -tags=e2e -v -run TestE2E_ThinkingMode ./tests/e2e/
func TestE2E_ThinkingMode(t *testing.T) {
	e2eCfg, err := testutil.LoadE2EConfig()
	if err != nil {
		t.Fatalf("Failed to load e2e config: %v", err)
	}

	// Test profiles with thinking enabled
	thinkingProfiles := map[string]struct {
		profileName     string
		provider        string
		expectReasoning bool // whether this provider surfaces reasoning content
	}{
		"anthropic": {
			profileName:     "claude-sonnet-thinking",
			provider:        "anthropic",
			expectReasoning: true, // Anthropic returns thinking blocks
		},
		"gemini": {
			profileName:     "gemini-pro-thinking",
			provider:        "gemini",
			expectReasoning: true, // Gemini returns thought parts
		},
		"openai": {
			profileName:     "gpt-5.4-reasoning",
			provider:        "openai",
			expectReasoning: false, // OpenAI reasoning is internal, not surfaced
		},
	}

	for name, tc := range thinkingProfiles {
		t.Run(name, func(t *testing.T) {
			profile, ok := e2eCfg.GetProfile(tc.profileName)
			if !ok {
				t.Skipf("Profile %s not configured", tc.profileName)
			}
			if profile.APIKey == "" {
				t.Skipf("No API key for profile %s", tc.profileName)
			}

			t.Logf("Provider: %s, Model: %s, ThinkingLevel: %s",
				profile.Provider, profile.Model, profile.ThinkingLevel)

			p, err := providers.NewProvider(profile)
			if err != nil {
				t.Fatalf("Failed to create provider: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()

			// Use a question that benefits from reasoning
			msgs := []protocol.Message{
				protocol.NewSystemMessage("You are a helpful assistant. Think step by step."),
				protocol.NewUserMessage("What is 17 * 23? Show your reasoning briefly."),
			}

			events, err := p.StreamChat(ctx, msgs, nil)
			if err != nil {
				if shouldSkipProviderError(tc.profileName, err) {
					t.Skipf("Skipping: %v", err)
				}
				t.Fatalf("StreamChat error: %v", err)
			}

			var gotReasoning bool
			var gotContent bool
			var gotFinish bool
			var reasoningText string
			var contentText string

			for e := range events {
				if e.Type == protocol.EventTypeError {
					if shouldSkipProviderError(tc.profileName, e.Error) {
						t.Skipf("Skipping: %v", e.Error)
					}
					t.Fatalf("Stream error: %v", e.Error)
				}

				if e.Type == protocol.EventTypeContentDelta && e.ContentPartDelta != nil {
					if e.ContentPartDelta.Type == protocol.ContentTypeReasoning {
						gotReasoning = true
						reasoningText += e.ContentPartDelta.Text
					} else {
						gotContent = true
						contentText += e.ContentPartDelta.Text
					}
				}

				if e.Type == protocol.EventTypeFinish {
					gotFinish = true
					if e.Usage != nil {
						t.Logf("Usage: input=%d, output=%d, total=%d",
							e.Usage.InputTokens, e.Usage.OutputTokens, e.Usage.TotalTokens)
					}
				}
			}

			if !gotFinish {
				t.Fatal("Expected finish event")
			}
			if !gotContent {
				t.Fatal("Expected content output")
			}

			t.Logf("Content: %s", truncate(contentText, 200))

			// Check for the correct answer
			if !strings.Contains(contentText, "391") && !strings.Contains(reasoningText, "391") {
				t.Logf("Warning: Expected 391 in response (17*23), got content=%s", truncate(contentText, 100))
			}

			if tc.expectReasoning {
				if gotReasoning {
					t.Logf("Reasoning content received (%d chars): %s",
						len(reasoningText), truncate(reasoningText, 200))
				} else {
					t.Logf("Warning: Expected reasoning content from %s but got none (model may not support it)", name)
				}
			} else {
				t.Logf("Reasoning not expected from %s (internal reasoning only)", name)
			}
		})
	}
}

// TestE2E_ThinkingMultiTurn tests that thinking blocks are correctly
// preserved across multi-turn conversations.
//
// Run: go test -tags=e2e -v -run TestE2E_ThinkingMultiTurn ./tests/e2e/
func TestE2E_ThinkingMultiTurn(t *testing.T) {
	e2eCfg, err := testutil.LoadE2EConfig()
	if err != nil {
		t.Fatalf("Failed to load e2e config: %v", err)
	}

	profile, ok := e2eCfg.GetProfile("claude-sonnet-thinking")
	if !ok {
		t.Skip("claude-sonnet-thinking profile not configured")
	}
	if profile.APIKey == "" {
		t.Skip("No API key configured")
	}

	p, err := providers.NewProvider(profile)
	if err != nil {
		t.Fatalf("Failed to create provider: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Turn 1: Ask a question that triggers thinking
	msgs := []protocol.Message{
		protocol.NewSystemMessage("You are a concise math tutor."),
		protocol.NewUserMessage("What is the square root of 144?"),
	}

	events, err := p.StreamChat(ctx, msgs, nil)
	if err != nil {
		if shouldSkipProviderError("claude-sonnet-thinking", err) {
			t.Skipf("Skipping: %v", err)
		}
		t.Fatalf("Turn 1 StreamChat error: %v", err)
	}

	// Collect turn 1 response
	var turn1Reasoning string
	var turn1ReasoningSig string
	var turn1Content string

	for e := range events {
		if e.Type == protocol.EventTypeError {
			if shouldSkipProviderError("claude-sonnet-thinking", e.Error) {
				t.Skipf("Skipping: %v", e.Error)
			}
			t.Fatalf("Turn 1 error: %v", e.Error)
		}
		if e.Type == protocol.EventTypeContentDelta && e.ContentPartDelta != nil {
			if e.ContentPartDelta.Type == protocol.ContentTypeReasoning {
				turn1Reasoning += e.ContentPartDelta.Text
			} else {
				turn1Content += e.ContentPartDelta.Text
			}
		}
		if e.Type == protocol.EventTypeContentEnd && e.ContentPart != nil &&
			e.ContentPart.Type == protocol.ContentTypeReasoning {
			turn1ReasoningSig = e.ContentPart.Signature
			if e.ContentPart.Text != "" {
				turn1Reasoning = e.ContentPart.Text
			}
		}
	}

	t.Logf("Turn 1 content: %s", truncate(turn1Content, 200))
	if turn1Reasoning != "" {
		t.Logf("Turn 1 reasoning: %s", truncate(turn1Reasoning, 200))
		t.Logf("Turn 1 signature present: %v", turn1ReasoningSig != "")
	}

	// Build turn 2 with history including reasoning block
	asstMsg := protocol.NewAssistantMessage(turn1Content)
	if turn1Reasoning != "" {
		reasoningPart := protocol.ContentPart{
			Type:      protocol.ContentTypeReasoning,
			Text:      turn1Reasoning,
			Signature: turn1ReasoningSig,
		}
		asstMsg.Content = append([]protocol.ContentPart{reasoningPart}, asstMsg.Content...)
	}

	msgs = append(msgs, asstMsg)
	msgs = append(msgs, protocol.NewUserMessage("And what is 144 squared?"))

	events2, err := p.StreamChat(ctx, msgs, nil)
	if err != nil {
		t.Fatalf("Turn 2 StreamChat error: %v", err)
	}

	var turn2Content string
	var gotTurn2Finish bool

	for e := range events2 {
		if e.Type == protocol.EventTypeError {
			t.Fatalf("Turn 2 error: %v", e.Error)
		}
		if e.Type == protocol.EventTypeContentDelta && e.ContentPartDelta != nil {
			if e.ContentPartDelta.Type != protocol.ContentTypeReasoning {
				turn2Content += e.ContentPartDelta.Text
			}
		}
		if e.Type == protocol.EventTypeFinish {
			gotTurn2Finish = true
		}
	}

	if !gotTurn2Finish {
		t.Fatal("Expected finish event for turn 2")
	}

	t.Logf("Turn 2 content: %s", truncate(turn2Content, 200))

	if turn2Content == "" {
		t.Error("Turn 2 should have content")
	}

	// Check for correct answer (144^2 = 20736)
	if strings.Contains(turn2Content, "20736") || strings.Contains(turn2Content, "20,736") {
		t.Log("Turn 2 contains correct answer (20736)")
	} else {
		t.Logf("Warning: Expected 20736 in turn 2 response")
	}
}

// TestE2E_ThinkingVsNonThinking verifies that the same provider
// behaves differently with and without thinking enabled.
//
// Run: go test -tags=e2e -v -run TestE2E_ThinkingVsNonThinking ./tests/e2e/
func TestE2E_ThinkingVsNonThinking(t *testing.T) {
	e2eCfg, err := testutil.LoadE2EConfig()
	if err != nil {
		t.Fatalf("Failed to load e2e config: %v", err)
	}

	// Compare Anthropic with and without thinking
	pairs := []struct {
		name         string
		withThinking string
		noThinking   string
	}{
		{"anthropic", "claude-sonnet-thinking", "claude-sonnet"},
		{"gemini", "gemini-pro-thinking", "gemini-pro"},
	}

	for _, pair := range pairs {
		t.Run(pair.name, func(t *testing.T) {
			thinkingProfile, ok := e2eCfg.GetProfile(pair.withThinking)
			if !ok {
				t.Skipf("Profile %s not configured", pair.withThinking)
			}
			noThinkingProfile, ok := e2eCfg.GetProfile(pair.noThinking)
			if !ok {
				t.Skipf("Profile %s not configured", pair.noThinking)
			}

			if thinkingProfile.APIKey == "" || noThinkingProfile.APIKey == "" {
				t.Skip("Missing API keys")
			}

			// Run without thinking
			pNoThink, err := providers.NewProvider(noThinkingProfile)
			if err != nil {
				t.Fatalf("Failed to create non-thinking provider: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()

			msgs := []protocol.Message{
				protocol.NewSystemMessage("You are a concise assistant."),
				protocol.NewUserMessage("What is 15 + 27?"),
			}

			eventsNoThink, err := pNoThink.StreamChat(ctx, msgs, nil)
			if err != nil {
				if shouldSkipProviderError(pair.noThinking, err) {
					t.Skipf("Skipping: %v", err)
				}
				t.Fatalf("Non-thinking StreamChat error: %v", err)
			}

			var noThinkReasoning bool
			for e := range eventsNoThink {
				if e.Type == protocol.EventTypeError {
					if shouldSkipProviderError(pair.noThinking, e.Error) {
						t.Skipf("Skipping: %v", e.Error)
					}
					t.Fatalf("Non-thinking error: %v", e.Error)
				}
				if e.Type == protocol.EventTypeContentDelta && e.ContentPartDelta != nil &&
					e.ContentPartDelta.Type == protocol.ContentTypeReasoning {
					noThinkReasoning = true
				}
			}

			// Run with thinking
			pThink, err := providers.NewProvider(thinkingProfile)
			if err != nil {
				t.Fatalf("Failed to create thinking provider: %v", err)
			}

			eventsThink, err := pThink.StreamChat(ctx, msgs, nil)
			if err != nil {
				if shouldSkipProviderError(pair.withThinking, err) {
					t.Skipf("Skipping: %v", err)
				}
				t.Fatalf("Thinking StreamChat error: %v", err)
			}

			var thinkReasoning bool
			for e := range eventsThink {
				if e.Type == protocol.EventTypeError {
					if shouldSkipProviderError(pair.withThinking, e.Error) {
						t.Skipf("Skipping: %v", e.Error)
					}
					t.Fatalf("Thinking error: %v", e.Error)
				}
				if e.Type == protocol.EventTypeContentDelta && e.ContentPartDelta != nil &&
					e.ContentPartDelta.Type == protocol.ContentTypeReasoning {
					thinkReasoning = true
				}
			}

			t.Logf("Without thinking: reasoning=%v", noThinkReasoning)
			t.Logf("With thinking: reasoning=%v", thinkReasoning)

			if noThinkReasoning {
				t.Log("Warning: Non-thinking mode produced reasoning events (unexpected)")
			}
			if thinkReasoning {
				t.Log("Thinking mode correctly produced reasoning events")
			} else {
				t.Logf("Warning: Thinking mode did not produce reasoning events for %s (model may not support it)", pair.name)
			}
		})
	}
}
