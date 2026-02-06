//go:build e2e

package agent_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/testutil"
	"github.com/jayyao97/zotigo/core/tools"

	// Register providers
	_ "github.com/jayyao97/zotigo/core/providers/openai"
)

// E2E tests for agent compression functionality.
// Run with: go test -tags=e2e ./core/agent/...
//
// These tests may:
// - Take longer to run
// - Require API keys (OPENAI_API_KEY, ANTHROPIC_API_KEY)
// - Make real API calls

func TestE2E_AgentLongConversation(t *testing.T) {
	providers.Register("e2e-verbose", func(cfg config.ProfileConfig) (providers.Provider, error) {
		return &E2EVerboseProvider{}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "e2e-verbose"}
	ag, err := agent.New(cfg, exec, "You are a helpful assistant.")
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Run many conversations
	numTurns := 20
	for i := 0; i < numTurns; i++ {
		events, err := ag.Run(ctx, "Tell me about topic "+string(rune('A'+i%26)))
		if err != nil {
			t.Fatalf("Run error at turn %d: %v", i, err)
		}
		for range events {
			// Drain events
		}

		// Log progress every 5 turns
		if (i+1)%5 == 0 {
			stats := ag.GetContextStats()
			t.Logf("Turn %d: messages=%d, tokens=%d",
				i+1, stats["message_count"], stats["estimated_tokens"])
		}
	}

	// Get final stats
	statsBefore := ag.GetContextStats()
	t.Logf("Before compression: messages=%d, tokens=%d",
		statsBefore["message_count"], statsBefore["estimated_tokens"])

	// Force compress
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
		return &E2EToolProvider{step: 0}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "e2e-tools"}
	ag, err := agent.New(cfg, exec, "You are a coding assistant.")
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}

	ag.RegisterTool(&MockReadFileTool{})
	ag.SetApprovalPolicy(agent.ApprovalPolicyAuto) // Auto-approve for E2E

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Run conversations that trigger tool calls
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
		t.Logf("Turn %d response: %s", i+1, truncateString(content, 50))
	}

	stats := ag.GetContextStats()
	t.Logf("Final stats: messages=%d, tokens=%d", stats["message_count"], stats["estimated_tokens"])

	// Force compress and verify tool chains preserved
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
		t.Fatalf("Failed to load E2E config: %v", err)
	}

	profileCfg := e2eCfg.GetProfileConfig()
	if profileCfg.APIKey == "" {
		t.Skip("No API key configured in config.json")
	}

	t.Logf("Using provider: %s, model: %s", profileCfg.Provider, profileCfg.Model)

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	ag, err := agent.New(profileCfg, exec, "You are a helpful assistant. Keep responses brief.")
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Run a few real conversations
	prompts := []string{
		"What is 2+2?",
		"Name a color.",
		"Say hello.",
	}

	for i, prompt := range prompts {
		t.Logf("Sending prompt %d: %s", i+1, prompt)

		events, err := ag.Run(ctx, prompt)
		if err != nil {
			t.Fatalf("Run error: %v", err)
		}

		var response string
		for e := range events {
			if e.Type == protocol.EventTypeContentDelta && e.ContentPartDelta != nil {
				response += e.ContentPartDelta.Text
			}
		}

		t.Logf("Response %d: %s", i+1, truncateString(response, 100))

		stats := ag.GetContextStats()
		t.Logf("Stats after turn %d: messages=%d, tokens=%d",
			i+1, stats["message_count"], stats["estimated_tokens"])
	}

	// Test force compress
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
		return &E2ELargeResponseProvider{}, nil
	})

	tmpDir := t.TempDir()
	exec, err := executor.NewLocalExecutor(tmpDir)
	if err != nil {
		t.Fatalf("Failed to create executor: %v", err)
	}

	cfg := config.ProfileConfig{Provider: "e2e-memory"}
	ag, err := agent.New(cfg, exec, "System")
	if err != nil {
		t.Fatalf("Failed to create agent: %v", err)
	}

	ctx := context.Background()

	// Generate many large responses
	for i := 0; i < 50; i++ {
		events, _ := ag.Run(ctx, "Generate large response "+string(rune('0'+i%10)))
		for range events {
		}

		if i%10 == 9 {
			stats := ag.GetContextStats()
			t.Logf("Iteration %d: messages=%d, tokens=%d",
				i+1, stats["message_count"], stats["estimated_tokens"])

			// Force compress periodically
			result, _ := ag.ForceCompress(ctx)
			if result.Compressed {
				t.Logf("  Compressed: %d -> %d tokens", result.OriginalTokens, result.CompressedTokens)
			}
		}
	}
}

// ============ E2E Mock Providers ============

type E2EVerboseProvider struct{}

func (p *E2EVerboseProvider) Name() string { return "e2e-verbose" }

func (p *E2EVerboseProvider) StreamChat(ctx context.Context, messages []protocol.Message, t []tools.Tool) (<-chan protocol.Event, error) {
	ch := make(chan protocol.Event, 10)
	go func() {
		defer close(ch)

		// Generate a reasonably long response
		response := "Thank you for your question. Let me provide a detailed explanation. " +
			"This topic has several important aspects to consider. " +
			"First, we should understand the fundamental concepts. " +
			"Second, we need to examine the practical applications. " +
			"Third, let's look at some real-world examples. " +
			"In conclusion, this is a fascinating subject with many nuances. " +
			"I hope this explanation was helpful and comprehensive!"

		ch <- protocol.NewTextDeltaEvent(response)
		ch <- protocol.NewFinishEvent(protocol.FinishReasonStop)
	}()
	return ch, nil
}

type E2EToolProvider struct {
	step int
}

func (p *E2EToolProvider) Name() string { return "e2e-tools" }

func (p *E2EToolProvider) StreamChat(ctx context.Context, messages []protocol.Message, t []tools.Tool) (<-chan protocol.Event, error) {
	ch := make(chan protocol.Event, 10)
	go func() {
		defer close(ch)

		// Check if we need to respond to a tool result
		hasToolResult := false
		for _, msg := range messages {
			if msg.Role == protocol.RoleTool {
				hasToolResult = true
				break
			}
		}

		if !hasToolResult && len(t) > 0 {
			// Make a tool call
			ch <- protocol.Event{
				Type:  protocol.EventTypeToolCallDelta,
				Index: 0,
				ToolCallDelta: &protocol.ToolCallDelta{
					ID:   "call_" + string(rune('A'+p.step)),
					Name: "read_file",
				},
			}
			ch <- protocol.Event{
				Type:  protocol.EventTypeToolCallEnd,
				Index: 0,
				ToolCall: &protocol.ToolCall{
					ID:        "call_" + string(rune('A'+p.step)),
					Name:      "read_file",
					Arguments: `{"path": "test.go"}`,
				},
			}
			ch <- protocol.NewFinishEvent(protocol.FinishReasonToolCalls)
			p.step++
		} else {
			// Respond with analysis
			ch <- protocol.NewTextDeltaEvent("I've read the file and analyzed its contents. The code looks well-structured.")
			ch <- protocol.NewFinishEvent(protocol.FinishReasonStop)
		}
	}()
	return ch, nil
}

type E2ELargeResponseProvider struct{}

func (p *E2ELargeResponseProvider) Name() string { return "e2e-memory" }

func (p *E2ELargeResponseProvider) StreamChat(ctx context.Context, messages []protocol.Message, t []tools.Tool) (<-chan protocol.Event, error) {
	ch := make(chan protocol.Event, 10)
	go func() {
		defer close(ch)
		// Generate a large response
		largeResponse := strings.Repeat("This is a test response with substantial content. ", 50)
		ch <- protocol.NewTextDeltaEvent(largeResponse)
		ch <- protocol.NewFinishEvent(protocol.FinishReasonStop)
	}()
	return ch, nil
}

// ============ Mock Tools ============

type MockReadFileTool struct{}

func (t *MockReadFileTool) Name() string        { return "read_file" }
func (t *MockReadFileTool) Description() string { return "Reads a file" }
func (t *MockReadFileTool) Schema() any         { return nil }

func (t *MockReadFileTool) Execute(ctx context.Context, exec executor.Executor, args string) (any, error) {
	return "// Mock file content\npackage main\n\nfunc main() {\n\tfmt.Println(\"Hello\")\n}", nil
}

// ============ Helper Functions ============

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
