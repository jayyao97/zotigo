//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/agent/prompt"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/testutil"
)

// E2E tests for agent compression functionality.
// Run with: go test -tags=e2e -v -run TestE2E_Agent ./tests/e2e/

func TestE2E_AgentLongConversation(t *testing.T) {
	providers.Register("e2e-verbose", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &e2eVerboseProvider{}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "e2e-verbose"}
	pb := prompt.NewSystemPromptBuilder()
	pb.SetStaticPrompt("You are a helpful assistant.")
	ag, err := agent.New(cfg, exec, agent.WithSystemPromptBuilder(pb))
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	numTurns := 20
	for i := 0; i < numTurns; i++ {
		events, err := ag.Run(ctx, "Tell me about topic "+string(rune('A'+i%26)))
		if err != nil {
			t.Fatalf("Run error at turn %d: %v", i, err)
		}
		for range events {
		}

		if (i+1)%5 == 0 {
			stats := ag.GetContextStats()
			t.Logf("Turn %d: messages=%d, tokens=%d",
				i+1, stats["message_count"], stats["estimated_tokens"])
		}
	}

	statsBefore := ag.GetContextStats()
	t.Logf("Before compression: messages=%d, tokens=%d",
		statsBefore["message_count"], statsBefore["estimated_tokens"])

	result, err := ag.ForceCompress(ctx)
	if err != nil {
		t.Fatalf("ForceCompress error: %v", err)
	}

	statsAfter := ag.GetContextStats()
	t.Logf("After compression: messages=%d, tokens=%d",
		statsAfter["message_count"], statsAfter["estimated_tokens"])

	t.Logf("Compression result: compressed=%v, tokens %d -> %d",
		result.Compressed, result.OriginalTokens, result.CompressedTokens)

	if result.Compressed {
		reduction := float64(result.OriginalTokens-result.CompressedTokens) / float64(result.OriginalTokens) * 100
		t.Logf("Token reduction: %.1f%%", reduction)

		if reduction < 20 {
			t.Logf("Warning: Low compression ratio (%.1f%%), may need tuning", reduction)
		}
	}
}

func TestE2E_AgentWithToolCalls(t *testing.T) {
	providers.Register("e2e-tools", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &e2eToolProvider{step: 0}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "e2e-tools"}
	pb := prompt.NewSystemPromptBuilder()
	pb.SetStaticPrompt("You are a coding assistant.")
	ag, err := agent.New(cfg, exec, agent.WithSystemPromptBuilder(pb))
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}

	ag.RegisterTool(&mockReadFileTool{})
	ag.SetApprovalPolicy(agent.ApprovalPolicyAuto)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for i := 0; i < 5; i++ {
		events, err := ag.Run(ctx, "Read file "+string(rune('A'+i))+".go")
		if err != nil {
			t.Fatalf("Run error: %v", err)
		}

		var content string
		for e := range events {
			if e.Type == protocol.EventTypeContentDelta && e.ContentPartDelta != nil {
				content += e.ContentPartDelta.Text
			}
		}
		t.Logf("Turn %d response: %s", i+1, truncate(content, 50))
	}

	stats := ag.GetContextStats()
	t.Logf("Final stats: messages=%d, tokens=%d", stats["message_count"], stats["estimated_tokens"])

	result, err := ag.ForceCompress(ctx)
	if err != nil {
		t.Fatalf("ForceCompress error: %v", err)
	}

	t.Logf("Compression: %v, messages %d -> %d",
		result.Compressed, result.MessagesBefore, result.MessagesAfter)
}

func TestE2E_AgentCompressionWithRealProvider(t *testing.T) {
	e2eCfg, err := testutil.LoadE2EConfig()
	if err != nil {
		t.Fatalf("Failed to load e2e config: %v", err)
	}

	profileCfg := e2eCfg.GetProfileConfig()
	if profileCfg.APIKey == "" {
		t.Skip("No API key configured")
	}

	t.Logf("Using provider: %s, model: %s", profileCfg.Provider, profileCfg.Model)

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	pb := prompt.NewSystemPromptBuilder()
	pb.SetStaticPrompt("You are a helpful assistant. Keep responses brief.")
	ag, err := agent.New(profileCfg, exec, agent.WithSystemPromptBuilder(pb))
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	prompts := []string{
		"What is 2+2?",
		"Name a color.",
		"Say hello.",
	}

	for i, p := range prompts {
		t.Logf("Sending prompt %d: %s", i+1, p)

		events, err := ag.Run(ctx, p)
		if err != nil {
			t.Fatalf("Run error: %v", err)
		}

		var response string
		for e := range events {
			if e.Type == protocol.EventTypeContentDelta && e.ContentPartDelta != nil {
				response += e.ContentPartDelta.Text
			}
		}

		t.Logf("Response %d: %s", i+1, truncate(response, 100))

		stats := ag.GetContextStats()
		t.Logf("Stats after turn %d: messages=%d, tokens=%d",
			i+1, stats["message_count"], stats["estimated_tokens"])
	}

	result, err := ag.ForceCompress(ctx)
	if err != nil {
		t.Fatalf("ForceCompress error: %v", err)
	}

	t.Logf("Final compression result: compressed=%v", result.Compressed)
}

func TestE2E_MemoryUnderPressure(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping memory pressure test in short mode")
	}

	providers.Register("e2e-memory", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &e2eLargeResponseProvider{}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "e2e-memory"}
	pb := prompt.NewSystemPromptBuilder()
	pb.SetStaticPrompt("System")
	ag, err := agent.New(cfg, exec, agent.WithSystemPromptBuilder(pb))
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}

	ctx := context.Background()

	for i := 0; i < 50; i++ {
		events, _ := ag.Run(ctx, "Generate large response "+string(rune('0'+i%10)))
		for range events {
		}

		if i%10 == 9 {
			stats := ag.GetContextStats()
			t.Logf("Iteration %d: messages=%d, tokens=%d",
				i+1, stats["message_count"], stats["estimated_tokens"])

			result, _ := ag.ForceCompress(ctx)
			if result.Compressed {
				t.Logf("  Compressed: %d -> %d tokens", result.OriginalTokens, result.CompressedTokens)
			}
		}
	}
}
