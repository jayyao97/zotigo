//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/services"
	"github.com/jayyao97/zotigo/core/testutil"
)

// E2E tests for compressor functionality.
// Run with: go test -tags=e2e -v -run TestE2E_Compressor ./tests/e2e/

func TestE2E_RealWorldConversation(t *testing.T) {
	c := services.NewCompressor(services.CompressorConfig{
		ContextWindowSize:   4000,
		TriggerRatio:        0.7,
		PreserveRatio:       0.3,
		ToolOutputThreshold: 500,
	})

	msgs := buildRealisticConversation()

	t.Logf("Conversation: %d messages", len(msgs))
	tokensBefore := c.CountTokens(msgs)
	t.Logf("Tokens before: %d", tokensBefore)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	compressed, result, err := c.Compress(ctx, msgs)
	if err != nil {
		t.Fatalf("Compress failed: %v", err)
	}

	t.Logf("Compression result:")
	t.Logf("  Compressed: %v", result.Compressed)
	t.Logf("  Messages: %d -> %d", result.MessagesBefore, result.MessagesAfter)
	t.Logf("  Tokens: %d -> %d (%.1f%% reduction)",
		result.OriginalTokens, result.CompressedTokens,
		float64(result.OriginalTokens-result.CompressedTokens)/float64(result.OriginalTokens)*100)
	t.Logf("  Partition Index: %d", result.PartitionIndex)

	if result.Compressed {
		if result.Summary == "" {
			t.Error("Summary should not be empty")
		}
		if !strings.Contains(result.Summary, "<context_summary>") {
			t.Error("Summary should have XML structure")
		}

		lastMsg := compressed[len(compressed)-1]
		if lastMsg.Role != protocol.RoleAssistant {
			t.Logf("Last message role: %s", lastMsg.Role)
		}

		hasSystem := false
		for _, msg := range compressed {
			if msg.Role == protocol.RoleSystem {
				hasSystem = true
				break
			}
		}
		if !hasSystem {
			t.Error("System message should be preserved")
		}

		t.Logf("\n=== Generated Summary ===\n%s\n========================", result.Summary)
	}
}

func TestE2E_ToolChainPreservation(t *testing.T) {
	c := services.NewCompressor(services.CompressorConfig{
		ContextWindowSize: 2000,
		TriggerRatio:      0.5,
		PreserveRatio:     0.4,
	})

	msgs := buildConversationWithToolChains()

	ctx := context.Background()
	compressed, result, err := c.Compress(ctx, msgs)
	if err != nil {
		t.Fatalf("Compress failed: %v", err)
	}

	if !result.Compressed {
		t.Log("No compression needed for this test case")
		return
	}

	var pendingToolCalls []string
	for i, msg := range compressed {
		for _, part := range msg.Content {
			if part.Type == protocol.ContentTypeToolCall && part.ToolCall != nil {
				pendingToolCalls = append(pendingToolCalls, part.ToolCall.ID)
			}
			if part.Type == protocol.ContentTypeToolResult && part.ToolResult != nil {
				found := false
				for j, id := range pendingToolCalls {
					if id == part.ToolResult.ToolCallID {
						pendingToolCalls = append(pendingToolCalls[:j], pendingToolCalls[j+1:]...)
						found = true
						break
					}
				}
				if !found {
					t.Errorf("Tool result at message %d has no matching tool call: %s",
						i, part.ToolResult.ToolCallID)
				}
			}
		}
	}

	if len(pendingToolCalls) > 0 {
		t.Errorf("Orphaned tool calls without results: %v", pendingToolCalls)
	}

	t.Logf("Tool chain verification passed. Compressed %d -> %d messages",
		result.MessagesBefore, result.MessagesAfter)
}

func TestE2E_LargeToolOutputCompression(t *testing.T) {
	c := services.NewCompressor(services.CompressorConfig{
		ContextWindowSize:   10000,
		ToolOutputThreshold: 100,
	})

	largeOutput := generateLargeCodeOutput()
	msgs := []protocol.Message{
		{Role: protocol.RoleSystem, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeText, Text: "You are a coding assistant."},
		}},
		{Role: protocol.RoleUser, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeText, Text: "Read the main.go file"},
		}},
		{Role: protocol.RoleAssistant, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeToolCall, ToolCall: &protocol.ToolCall{
				ID: "call_read", Name: "read_file", Arguments: `{"path": "main.go"}`,
			}},
		}},
		{Role: protocol.RoleTool, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeToolResult, ToolResult: &protocol.ToolResult{
				ToolCallID: "call_read",
				Type:       protocol.ToolResultTypeText,
				Text:       largeOutput,
			}},
		}},
		{Role: protocol.RoleUser, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeText, Text: "Now explain the code"},
		}},
	}

	originalLen := len(msgs[3].Content[0].ToolResult.Text)
	truncated := c.TruncateToolResults(msgs, 100)
	truncatedLen := len(truncated[3].Content[0].ToolResult.Text)

	t.Logf("Tool output: %d -> %d chars (%.1f%% reduction)",
		originalLen, truncatedLen,
		float64(originalLen-truncatedLen)/float64(originalLen)*100)

	if truncatedLen >= originalLen {
		t.Error("Large tool output should be truncated")
	}
	if !strings.Contains(truncated[3].Content[0].ToolResult.Text, "truncated") {
		t.Error("Should indicate truncation")
	}
}

func TestE2E_TokenCountingAccuracy(t *testing.T) {
	testCases := []struct {
		text           string
		expectedTokens int
		tolerance      int
	}{
		{"Hello, world!", 4, 1},
		{"The quick brown fox jumps over the lazy dog.", 10, 2},
		{"func main() {\n\tfmt.Println(\"Hello, World!\")\n}", 15, 3},
		{strings.Repeat("token ", 100), 100, 10},
		{"你好世界，这是一个测试。", 10, 5},
	}

	tk, err := services.DefaultTokenizer()
	if err != nil {
		t.Fatalf("Failed to create tokenizer: %v", err)
	}

	for _, tc := range testCases {
		actual := tk.Count(tc.text)
		diff := tc.expectedTokens - actual
		if diff < 0 {
			diff = -diff
		}

		t.Logf("Text: %q\n  Expected: ~%d, Actual: %d, Diff: %d",
			truncate(tc.text, 40), tc.expectedTokens, actual, diff)

		if diff > tc.tolerance {
			t.Errorf("Token count for %q is off by %d (expected ~%d, got %d)",
				truncate(tc.text, 20), diff, tc.expectedTokens, actual)
		}
	}
}

func TestE2E_CompressionPerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping performance test in short mode")
	}

	c := services.NewCompressor(services.CompressorConfig{
		ContextWindowSize: 1000,
		TriggerRatio:      0.3,
		PreserveRatio:     0.3,
	})

	msgs := make([]protocol.Message, 0, 100)
	msgs = append(msgs, protocol.Message{
		Role:    protocol.RoleSystem,
		Content: []protocol.ContentPart{{Type: protocol.ContentTypeText, Text: "System prompt"}},
	})

	for i := 0; i < 50; i++ {
		msgs = append(msgs,
			protocol.Message{
				Role: protocol.RoleUser,
				Content: []protocol.ContentPart{
					{Type: protocol.ContentTypeText, Text: strings.Repeat("user message ", 20)},
				},
			},
			protocol.Message{
				Role: protocol.RoleAssistant,
				Content: []protocol.ContentPart{
					{Type: protocol.ContentTypeText, Text: strings.Repeat("assistant response ", 20)},
				},
			},
		)
	}

	ctx := context.Background()

	start := time.Now()
	_, result, err := c.Compress(ctx, msgs)
	duration := time.Since(start)

	if err != nil {
		t.Fatalf("Compress failed: %v", err)
	}

	t.Logf("Performance results:")
	t.Logf("  Messages: %d", len(msgs))
	t.Logf("  Original tokens: %d", result.OriginalTokens)
	t.Logf("  Compression time: %v", duration)
	t.Logf("  Compressed: %v", result.Compressed)

	if result.Compressed {
		t.Logf("  Compressed tokens: %d (%.1f%% reduction)",
			result.CompressedTokens,
			float64(result.OriginalTokens-result.CompressedTokens)/float64(result.OriginalTokens)*100)
	}

	if duration > 5*time.Second {
		t.Errorf("Compression took too long: %v", duration)
	}
}

func TestE2E_WithRealSummarizer(t *testing.T) {
	cfg, err := testutil.LoadE2EConfig()
	if err != nil {
		t.Fatalf("Failed to load e2e config: %v", err)
	}

	profileCfg := cfg.GetProfileConfig()
	if profileCfg.APIKey == "" {
		t.Skip("No API key configured")
	}

	t.Logf("Using provider: %s, model: %s", profileCfg.Provider, profileCfg.Model)

	provider, err := providers.NewProvider(profileCfg)
	if err != nil {
		t.Fatalf("Failed to create provider: %v", err)
	}

	summarizer := services.NewProviderSummarizer(provider)

	c := services.NewCompressor(services.CompressorConfig{
		ContextWindowSize:   500,
		TriggerRatio:        0.5,
		PreserveRatio:       0.3,
		ToolOutputThreshold: 100,
	})
	c.SetSummarizer(summarizer)

	msgs := buildRealisticConversation()
	t.Logf("Test conversation: %d messages", len(msgs))

	tokensBefore := c.CountTokens(msgs)
	t.Logf("Tokens before: %d", tokensBefore)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	compressed, result, err := c.Compress(ctx, msgs)
	if err != nil {
		t.Fatalf("Compress failed: %v", err)
	}

	t.Logf("Compression result:")
	t.Logf("  Compressed: %v", result.Compressed)
	t.Logf("  Messages: %d -> %d", result.MessagesBefore, result.MessagesAfter)
	t.Logf("  Tokens: %d -> %d", result.OriginalTokens, result.CompressedTokens)

	if result.Compressed {
		t.Logf("\n=== LLM Generated Summary ===\n%s\n==============================", result.Summary)

		if len(result.Summary) < 100 {
			t.Error("Summary seems too short")
		}

		if result.CompressedTokens >= result.OriginalTokens {
			t.Error("Compression should reduce token count")
		}

		if len(compressed) < 3 {
			t.Error("Should have at least system, summary, and some preserved messages")
		}
	}
}
