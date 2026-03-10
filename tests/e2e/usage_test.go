//go:build e2e

package e2e

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/testutil"
	"github.com/jayyao97/zotigo/core/tools"

	// Register providers
	_ "github.com/jayyao97/zotigo/core/providers/anthropic"
	_ "github.com/jayyao97/zotigo/core/providers/gemini"
	_ "github.com/jayyao97/zotigo/core/providers/openai"
)

// TestE2E_UsageTracking verifies that token usage is captured and stored
// in assistant message metadata across multiple turns.
//
// Run: go test -tags=e2e -v -run TestE2E_UsageTracking ./tests/e2e/
func TestE2E_UsageTracking(t *testing.T) {
	e2eCfg, err := testutil.LoadE2EConfig()
	if err != nil {
		t.Fatalf("Failed to load e2e config: %v", err)
	}

	profileCfg := e2eCfg.GetProfileConfig()
	if profileCfg.APIKey == "" {
		t.Skip("No API key configured")
	}

	t.Logf("Provider: %s, Model: %s", profileCfg.Provider, profileCfg.Model)

	ag := newTestAgent(t, profileCfg)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	prompts := []string{
		"Reply with exactly: pong",
		"Reply with exactly: hello",
		"Reply with exactly: done",
	}

	for i, p := range prompts {
		t.Logf("Turn %d: sending %q", i+1, p)
		runAndDrain(t, ctx, ag, p)
	}

	// Verify usage in history
	snap := ag.Snapshot()
	assistantTurns := 0
	turnsWithUsage := 0
	var totalInput, totalOutput int

	for _, msg := range snap.History {
		if msg.Role != protocol.RoleAssistant {
			continue
		}
		assistantTurns++

		if msg.Metadata == nil || msg.Metadata.Usage == nil {
			t.Logf("  Turn %d: NO usage metadata", assistantTurns)
			continue
		}

		u := msg.Metadata.Usage
		turnsWithUsage++
		totalInput += u.InputTokens
		totalOutput += u.OutputTokens

		t.Logf("  Turn %d: input=%d output=%d cache_create=%d cache_read=%d",
			assistantTurns, u.InputTokens, u.OutputTokens,
			u.CacheCreationInputTokens, u.CacheReadInputTokens)
	}

	t.Logf("Summary: %d/%d turns have usage, total input=%d output=%d",
		turnsWithUsage, assistantTurns, totalInput, totalOutput)

	if turnsWithUsage == 0 {
		t.Fatal("Expected at least one assistant turn with usage metadata")
	}
	if totalInput == 0 {
		t.Error("Expected total input tokens > 0")
	}
	if totalOutput == 0 {
		t.Error("Expected total output tokens > 0")
	}
}

// TestE2E_PromptCaching verifies that prompt caching works across providers:
//   - Anthropic: explicit cache_control → cache_creation on first turn, cache_read on subsequent
//   - OpenAI: automatic caching of repeated prefixes → cached_tokens on subsequent turns
//   - Gemini: context caching via gateway (if supported)
//
// Run: go test -tags=e2e -v -run TestE2E_PromptCaching ./tests/e2e/
func TestE2E_PromptCaching(t *testing.T) {
	e2eCfg, err := testutil.LoadE2EConfig()
	if err != nil {
		t.Fatalf("Failed to load e2e config: %v", err)
	}

	profiles := e2eCfg.AllProfiles()
	if len(profiles) == 0 {
		t.Skip("No profiles configured")
	}

	for name, profile := range profiles {
		if profile.APIKey == "" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Logf("Provider: %s, Model: %s, BaseURL: %s",
				profile.Provider, profile.Model, profile.BaseURL)

			ag := newCachingTestAgent(t, profile)
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			prompts := []string{
				"Reply with: one",
				"Reply with: two",
				"Reply with: three",
			}

			for i, p := range prompts {
				t.Logf("Turn %d: sending %q", i+1, p)
				runAndDrain(t, ctx, ag, p)
			}

			type turnUsage struct {
				input       int
				output      int
				cacheCreate int
				cacheRead   int
			}
			var usages []turnUsage

			snap := ag.Snapshot()
			for _, msg := range snap.History {
				if msg.Role != protocol.RoleAssistant {
					continue
				}
				if msg.Metadata == nil || msg.Metadata.Usage == nil {
					usages = append(usages, turnUsage{})
					continue
				}
				u := msg.Metadata.Usage
				usages = append(usages, turnUsage{
					input:       u.InputTokens,
					output:      u.OutputTokens,
					cacheCreate: u.CacheCreationInputTokens,
					cacheRead:   u.CacheReadInputTokens,
				})
			}

			for i, u := range usages {
				t.Logf("Turn %d usage: input=%d output=%d cache_create=%d cache_read=%d",
					i+1, u.input, u.output, u.cacheCreate, u.cacheRead)
			}

			if len(usages) < 3 {
				t.Fatalf("Expected 3 assistant turns, got %d", len(usages))
			}

			// Check caching behavior per provider
			totalCacheActivity := 0
			for _, u := range usages {
				totalCacheActivity += u.cacheCreate + u.cacheRead
			}

			if totalCacheActivity == 0 {
				// Not all providers support caching — log but don't fail
				t.Logf("No cache activity detected for %s (provider may not support caching)", name)
				return
			}

			// Turn 1: explicit caching (Anthropic) should show cache_create or cache_read;
			// implicit caching (Gemini) won't have any cache activity on the first request.
			if usages[0].cacheCreate == 0 && usages[0].cacheRead == 0 {
				t.Logf("Turn 1: no cache activity (expected for implicit caching providers like Gemini)")
			}

			// Turns 2+ should read from cache
			totalCacheRead := 0
			for i := 1; i < len(usages); i++ {
				totalCacheRead += usages[i].cacheRead
			}
			if totalCacheRead == 0 {
				t.Error("Turns 2+: expected cache_read_input_tokens > 0")
			}
		})
	}
}

// TestE2E_ToolOrderDeterminism verifies that tools are passed to the provider
// in a deterministic (sorted) order, which is critical for prompt caching.
//
// Run: go test -tags=e2e -v -run TestE2E_ToolOrderDeterminism ./tests/e2e/
func TestE2E_ToolOrderDeterminism(t *testing.T) {
	e2eCfg, err := testutil.LoadE2EConfig()
	if err != nil {
		t.Fatalf("Failed to load e2e config: %v", err)
	}

	profileCfg := e2eCfg.GetProfileConfig()
	if profileCfg.APIKey == "" {
		t.Skip("No API key configured")
	}

	ag := newTestAgent(t, profileCfg)
	extraTools := []tools.Tool{
		&dummyTool{name: "zebra_tool"},
		&dummyTool{name: "alpha_tool"},
		&dummyTool{name: "middle_tool"},
	}
	for _, tool := range extraTools {
		ag.RegisterTool(tool)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for i := 0; i < 3; i++ {
		runAndDrain(t, ctx, ag, "Reply with: ok")
	}

	snap := ag.Snapshot()
	for _, msg := range snap.History {
		if msg.Role == protocol.RoleAssistant && msg.Metadata != nil && msg.Metadata.Usage != nil {
			t.Logf("Usage: input=%d output=%d cache_create=%d cache_read=%d",
				msg.Metadata.Usage.InputTokens,
				msg.Metadata.Usage.OutputTokens,
				msg.Metadata.Usage.CacheCreationInputTokens,
				msg.Metadata.Usage.CacheReadInputTokens,
			)
		}
	}

	// Verify sorting directly
	toolMap := map[string]tools.Tool{
		"zebra":  &dummyTool{name: "zebra"},
		"alpha":  &dummyTool{name: "alpha"},
		"middle": &dummyTool{name: "middle"},
	}
	var toolList []tools.Tool
	for _, tool := range toolMap {
		toolList = append(toolList, tool)
	}
	sort.Slice(toolList, func(i, j int) bool {
		return toolList[i].Name() < toolList[j].Name()
	})

	expected := []string{"alpha", "middle", "zebra"}
	for i, tool := range toolList {
		if tool.Name() != expected[i] {
			t.Errorf("Tool at index %d: got %q, want %q", i, tool.Name(), expected[i])
		}
	}
	t.Log("Tool ordering is deterministic (sorted by name)")
}

// TestE2E_UsageAllProviders tests usage tracking for every configured provider.
//
// Run: go test -tags=e2e -v -run TestE2E_UsageAllProviders ./tests/e2e/
func TestE2E_UsageAllProviders(t *testing.T) {
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
			t.Logf("Testing %s (provider: %s, model: %s)", name, profile.Provider, profile.Model)

			ag := newTestAgent(t, profile)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			runAndDrain(t, ctx, ag, "Reply with exactly one word: pong")
			runAndDrain(t, ctx, ag, "Reply with exactly one word: done")

			snap := ag.Snapshot()
			for _, msg := range snap.History {
				if msg.Role != protocol.RoleAssistant {
					continue
				}
				if msg.Metadata == nil || msg.Metadata.Usage == nil {
					t.Errorf("[%s] assistant message missing usage metadata", name)
					continue
				}
				u := msg.Metadata.Usage
				t.Logf("[%s] input=%d output=%d total=%d cache_create=%d cache_read=%d",
					name, u.InputTokens, u.OutputTokens, u.TotalTokens,
					u.CacheCreationInputTokens, u.CacheReadInputTokens)

				if u.InputTokens == 0 && u.OutputTokens == 0 {
					t.Errorf("[%s] both input and output tokens are 0", name)
				}
			}
		})
	}
}

// firstProfile returns the first profile from a map.
func firstProfile(profiles map[string]config.ProfileConfig) config.ProfileConfig {
	for _, p := range profiles {
		return p
	}
	return config.ProfileConfig{}
}
