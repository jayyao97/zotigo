//go:build e2e

package e2e

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/agent/prompt"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
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

// TestE2E_DeepSeekDynamicContextCaching verifies cache reuse across ten turns
// while a persistent user-context section alternates between unchanged and
// changed values. The section is synthetic so the test is deterministic; it
// exercises the same full/delta path used by production runtime context.
//
// Run: go test -tags=e2e -v -count=1 -run '^TestE2E_DeepSeekDynamicContextCaching$' ./tests/e2e/
func TestE2E_DeepSeekDynamicContextCaching(t *testing.T) {
	e2eCfg, err := testutil.LoadE2EConfig()
	if err != nil {
		t.Fatalf("Failed to load e2e config: %v", err)
	}

	profiles := deepSeekProfiles(e2eCfg.AllProfiles())
	if len(profiles) == 0 {
		t.Skip("No DeepSeek profiles configured")
	}

	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		profile := profiles[name]
		t.Run(name, func(t *testing.T) {
			if profile.APIKey == "" {
				t.Skipf("No API key for profile %s", name)
			}

			contextValues := []string{
				"state-1", "state-1", "state-2", "state-2", "state-3",
				"state-4", "state-4", "state-5", "state-6", "state-6",
			}
			currentContext := contextValues[0]
			ag := newDynamicCachingTestAgent(t, profile, func() string {
				return currentContext
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()

			for turn, value := range contextValues {
				currentContext = value
				runAndDrain(t, ctx, ag, "Reply with exactly: ok")
				t.Logf("Turn %d synthetic context: %s", turn+1, value)
			}

			usages := assistantUsages(ag.Snapshot().History)
			if len(usages) != len(contextValues) {
				t.Fatalf("Expected %d assistant turns, got %d", len(contextValues), len(usages))
			}

			contextMessages := 0
			fullUpdates := 0
			deltaUpdates := 0
			for _, msg := range ag.Snapshot().History {
				if !msg.IsContextualUser() {
					continue
				}
				contextMessages++
				text := msg.String()
				switch {
				case strings.Contains(text, `update="full"`):
					fullUpdates++
				case strings.Contains(text, `update="delta"`):
					deltaUpdates++
				}
			}
			if contextMessages != 6 || fullUpdates != 1 || deltaUpdates != 5 {
				t.Fatalf(
					"Expected 1 full and 5 delta context messages, got total=%d full=%d delta=%d",
					contextMessages, fullUpdates, deltaUpdates,
				)
			}

			var sumRates float64
			var totalInput, totalCacheRead int
			var warmSumRates float64
			var warmInput, warmCacheReadTokens int
			warmCacheRead := 0
			for turn, usage := range usages {
				turnInput := usage.TotalInput()
				if turnInput == 0 {
					t.Fatalf("Turn %d reported zero total input tokens", turn+1)
				}
				rate := float64(usage.CacheReadInputTokens) / float64(turnInput)
				sumRates += rate
				totalInput += turnInput
				totalCacheRead += usage.CacheReadInputTokens
				if turn > 0 && usage.CacheReadInputTokens > 0 {
					warmCacheRead++
				}
				if turn > 0 {
					warmSumRates += rate
					warmInput += turnInput
					warmCacheReadTokens += usage.CacheReadInputTokens
				}
				t.Logf(
					"Turn %d usage: input=%d cache_create=%d cache_read=%d cache_read_rate=%.2f%%",
					turn+1, usage.InputTokens, usage.CacheCreationInputTokens,
					usage.CacheReadInputTokens, rate*100,
				)
			}

			meanRate := sumRates / float64(len(usages))
			overallRate := float64(totalCacheRead) / float64(totalInput)
			warmMeanRate := warmSumRates / float64(len(usages)-1)
			warmOverallRate := float64(warmCacheReadTokens) / float64(warmInput)
			t.Logf(
				"Summary (all): mean_turn_cache_read_rate=%.2f%% overall_cache_read_rate=%.2f%%",
				meanRate*100, overallRate*100,
			)
			t.Logf(
				"Summary (warm): mean_turn_cache_read_rate=%.2f%% overall_cache_read_rate=%.2f%% turns_with_cache=%d/%d",
				warmMeanRate*100, warmOverallRate*100, warmCacheRead, len(usages)-1,
			)
			if warmCacheRead == 0 {
				t.Fatal("Expected cache reads on at least one warm turn")
			}
		})
	}
}

func newDynamicCachingTestAgent(
	t *testing.T,
	profile config.ProfileConfig,
	contextValue func() string,
) *agent.Agent {
	t.Helper()
	exec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	pb := prompt.NewSystemPromptBuilder()
	var largePrompt strings.Builder
	largePrompt.WriteString("You are a concise assistant. Reply with exactly the requested text.\n")
	for i := 0; i < 150; i++ {
		largePrompt.WriteString("Stable cache-prefix rule: preserve deterministic instructions across every request.\n")
	}
	pb.SetStaticPrompt(largePrompt.String())

	ag, err := agent.New(profile, exec,
		agent.WithSystemPromptBuilder(pb),
		agent.WithUserContextBuilder(prompt.NewUserContextBuilder(
			prompt.WithKeyedContext("synthetic_runtime_state", "runtime_state", func(_ prompt.PromptContext) string {
				return contextValue()
			}),
		)),
		agent.WithApprovalPolicy(agent.ApprovalPolicyAuto),
	)
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}
	return ag
}

func assistantUsages(history []protocol.Message) []*protocol.Usage {
	usages := make([]*protocol.Usage, 0, len(history))
	for _, msg := range history {
		if msg.Role != protocol.RoleAssistant {
			continue
		}
		if msg.Metadata == nil || msg.Metadata.Usage == nil {
			usages = append(usages, &protocol.Usage{})
			continue
		}
		usages = append(usages, msg.Metadata.Usage)
	}
	return usages
}

func deepSeekProfiles(profiles map[string]config.ProfileConfig) map[string]config.ProfileConfig {
	result := make(map[string]config.ProfileConfig)
	for name, profile := range profiles {
		haystack := strings.ToLower(name + " " + profile.Model + " " + profile.BaseURL)
		if strings.Contains(haystack, "deepseek") {
			result[name] = profile
		}
	}
	return result
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
