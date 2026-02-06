//go:build e2e

package services

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/testutil"

	// Register providers
	_ "github.com/jayyao97/zotigo/core/providers/anthropic"
	_ "github.com/jayyao97/zotigo/core/providers/gemini"
	_ "github.com/jayyao97/zotigo/core/providers/openai"
)

// E2E tests for compressor functionality.
// Run with: go test -tags=e2e ./core/services/...
//
// These tests may:
// - Take longer to run
// - Require API keys (OPENAI_API_KEY, ANTHROPIC_API_KEY, GEMINI_API_KEY)
// - Make real API calls

func TestE2E_RealWorldConversation(t *testing.T) {
	c := NewCompressor(CompressorConfig{
		ContextWindowSize:   4000, // Small window to trigger compression
		TriggerRatio:        0.7,
		PreserveRatio:       0.3,
		ToolOutputThreshold: 500,
	})

	// Simulate a realistic coding conversation
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
		// Verify summary quality
		if result.Summary == "" {
			t.Error("Summary should not be empty")
		}
		if !strings.Contains(result.Summary, "<context_summary>") {
			t.Error("Summary should have XML structure")
		}

		// Verify recent messages preserved
		lastMsg := compressed[len(compressed)-1]
		if lastMsg.Role != protocol.RoleAssistant {
			t.Logf("Last message role: %s", lastMsg.Role)
		}

		// Verify system message preserved
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

		// Log the summary for inspection
		t.Logf("\n=== Generated Summary ===\n%s\n========================", result.Summary)
	}
}

func TestE2E_ToolChainPreservation(t *testing.T) {
	c := NewCompressor(CompressorConfig{
		ContextWindowSize: 2000,
		TriggerRatio:      0.5,
		PreserveRatio:     0.4,
	})

	// Build conversation with multiple tool chains
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

	// Verify no tool chain is broken
	var pendingToolCalls []string
	for i, msg := range compressed {
		for _, part := range msg.Content {
			if part.Type == protocol.ContentTypeToolCall && part.ToolCall != nil {
				pendingToolCalls = append(pendingToolCalls, part.ToolCall.ID)
			}
			if part.Type == protocol.ContentTypeToolResult && part.ToolResult != nil {
				// Find and remove the matching tool call
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
	c := NewCompressor(CompressorConfig{
		ContextWindowSize:   10000,
		ToolOutputThreshold: 100, // Very low threshold
	})

	// Create messages with large tool outputs
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
	// Compare our tokenizer against known token counts
	testCases := []struct {
		text           string
		expectedTokens int // Approximate expected tokens
		tolerance      int // Allowed deviation
	}{
		{"Hello, world!", 4, 1},
		{"The quick brown fox jumps over the lazy dog.", 10, 2},
		{"func main() {\n\tfmt.Println(\"Hello, World!\")\n}", 15, 3},
		{strings.Repeat("token ", 100), 100, 10},
		// Chinese text
		{"你好世界，这是一个测试。", 10, 5},
	}

	tk, err := DefaultTokenizer()
	if err != nil {
		t.Fatalf("Failed to create tokenizer: %v", err)
	}

	for _, tc := range testCases {
		actual := tk.Count(tc.text)
		diff := abs(actual - tc.expectedTokens)

		t.Logf("Text: %q\n  Expected: ~%d, Actual: %d, Diff: %d",
			truncateString(tc.text, 40), tc.expectedTokens, actual, diff)

		if diff > tc.tolerance {
			t.Errorf("Token count for %q is off by %d (expected ~%d, got %d)",
				truncateString(tc.text, 20), diff, tc.expectedTokens, actual)
		}
	}
}

func TestE2E_CompressionPerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping performance test in short mode")
	}

	c := NewCompressor(CompressorConfig{
		ContextWindowSize: 1000,
		TriggerRatio:      0.3,
		PreserveRatio:     0.3,
	})

	// Create a large conversation
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

	// Measure compression time
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

	// Compression should complete in reasonable time
	if duration > 5*time.Second {
		t.Errorf("Compression took too long: %v", duration)
	}
}

// TestE2E_WithRealSummarizer tests with a real LLM summarizer using e2e.config.json.
func TestE2E_WithRealSummarizer(t *testing.T) {
	cfg, err := testutil.LoadE2EConfig()
	if err != nil {
		t.Fatalf("Failed to load E2E config: %v", err)
	}

	profileCfg := cfg.GetProfileConfig()
	if profileCfg.APIKey == "" {
		t.Skip("No API key configured in e2e.config.json (or legacy config.json)")
	}

	t.Logf("Using provider: %s, model: %s", profileCfg.Provider, profileCfg.Model)

	// Create real provider
	provider, err := providers.NewProvider(profileCfg)
	if err != nil {
		t.Fatalf("Failed to create provider: %v", err)
	}

	// Create summarizer with real provider
	summarizer := NewProviderSummarizer(provider)

	// Create compressor with summarizer
	// Use very small window to ensure compression is triggered
	c := NewCompressor(CompressorConfig{
		ContextWindowSize:   500, // Very small to trigger compression
		TriggerRatio:        0.5, // 250 token threshold
		PreserveRatio:       0.3,
		ToolOutputThreshold: 100,
	})
	c.SetSummarizer(summarizer)

	// Build test conversation
	msgs := buildRealisticConversation()
	t.Logf("Test conversation: %d messages", len(msgs))

	tokensBefore := c.CountTokens(msgs)
	t.Logf("Tokens before: %d", tokensBefore)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Compress with real LLM summarization
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

		// Verify summary has meaningful content
		if len(result.Summary) < 100 {
			t.Error("Summary seems too short")
		}

		// Verify compression reduced tokens
		if result.CompressedTokens >= result.OriginalTokens {
			t.Error("Compression should reduce token count")
		}

		// Verify we have fewer messages but preserved structure
		if len(compressed) < 3 {
			t.Error("Should have at least system, summary, and some preserved messages")
		}
	}
}

// ============ Helper Functions ============

func buildRealisticConversation() []protocol.Message {
	return []protocol.Message{
		{Role: protocol.RoleSystem, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeText, Text: "You are an expert Go developer helping with code review and debugging."},
		}},
		{Role: protocol.RoleUser, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeText, Text: "I'm working on a context compression feature for my AI agent CLI. Can you help me review the implementation?"},
		}},
		{Role: protocol.RoleAssistant, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeText, Text: "I'd be happy to help review your context compression implementation! Could you share the code you'd like me to look at? I'm particularly interested in seeing how you handle the compression algorithm, token counting, and preservation of important context."},
		}},
		{Role: protocol.RoleUser, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeText, Text: "Let me show you the main compressor file. Can you read it?"},
		}},
		{Role: protocol.RoleAssistant, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeToolCall, ToolCall: &protocol.ToolCall{
				ID: "call_1", Name: "read_file", Arguments: `{"path": "compressor.go"}`,
			}},
		}},
		{Role: protocol.RoleTool, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeToolResult, ToolResult: &protocol.ToolResult{
				ToolCallID: "call_1",
				Type:       protocol.ToolResultTypeText,
				Text:       "package services\n\n// Compressor manages context...\ntype Compressor struct {\n\tconfig CompressorConfig\n\t// ... more code ...\n}\n\nfunc (c *Compressor) Compress(...) {\n\t// Implementation\n}",
			}},
		}},
		{Role: protocol.RoleAssistant, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeText, Text: "I've reviewed the compressor code. Here are my observations:\n\n1. The structure looks clean with good separation of concerns\n2. The token counting approach is reasonable\n3. I noticed you're using a configurable threshold - that's good for flexibility\n\nA few suggestions:\n- Consider adding more detailed logging for debugging\n- The partition algorithm could be optimized for edge cases\n- You might want to add benchmarks for performance testing"},
		}},
		{Role: protocol.RoleUser, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeText, Text: "Good points. Can you also check the test file to see if we have good coverage?"},
		}},
		{Role: protocol.RoleAssistant, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeToolCall, ToolCall: &protocol.ToolCall{
				ID: "call_2", Name: "read_file", Arguments: `{"path": "compressor_test.go"}`,
			}},
		}},
		{Role: protocol.RoleTool, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeToolResult, ToolResult: &protocol.ToolResult{
				ToolCallID: "call_2",
				Type:       protocol.ToolResultTypeText,
				Text:       "package services\n\nfunc TestCompressor_Compress(t *testing.T) {\n\t// Test implementation\n}\n\nfunc TestCompressor_TokenCount(t *testing.T) {\n\t// More tests\n}",
			}},
		}},
		{Role: protocol.RoleAssistant, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeText, Text: "The test coverage looks good! I see tests for the main compression logic and token counting. To improve further, you could add:\n\n1. Edge case tests (empty input, single message, etc.)\n2. Benchmark tests for performance\n3. Integration tests with the agent\n\nWould you like me to help write any of these additional tests?"},
		}},
		{Role: protocol.RoleUser, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeText, Text: "Yes, please help me add the edge case tests first."},
		}},
		{Role: protocol.RoleAssistant, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeText, Text: "I'll create comprehensive edge case tests for you. Here's what I'm planning to cover:\n\n1. Empty message slice\n2. Only system message\n3. Only tool messages without user messages\n4. Single user message\n5. Messages that don't need compression\n6. Messages at exactly the threshold\n\nLet me write these tests now..."},
		}},
	}
}

func buildConversationWithToolChains() []protocol.Message {
	msgs := []protocol.Message{
		{Role: protocol.RoleSystem, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeText, Text: "You are a helpful coding assistant."},
		}},
	}

	// Add multiple tool chains
	for i := 0; i < 5; i++ {
		msgs = append(msgs,
			protocol.Message{Role: protocol.RoleUser, Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: strings.Repeat("Please help with task ", 10) + string(rune('A'+i))},
			}},
			protocol.Message{Role: protocol.RoleAssistant, Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeToolCall, ToolCall: &protocol.ToolCall{
					ID: "call_" + string(rune('a'+i)), Name: "read_file", Arguments: `{"path": "file.go"}`,
				}},
			}},
			protocol.Message{Role: protocol.RoleTool, Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeToolResult, ToolResult: &protocol.ToolResult{
					ToolCallID: "call_" + string(rune('a'+i)),
					Type:       protocol.ToolResultTypeText,
					Text:       strings.Repeat("file content line ", 20),
				}},
			}},
			protocol.Message{Role: protocol.RoleAssistant, Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: strings.Repeat("Here's my analysis ", 15)},
			}},
		)
	}

	return msgs
}

func generateLargeCodeOutput() string {
	var sb strings.Builder
	sb.WriteString("package main\n\n")
	sb.WriteString("import (\n\t\"fmt\"\n\t\"os\"\n)\n\n")

	for i := 0; i < 50; i++ {
		sb.WriteString("// Function " + string(rune('A'+i%26)) + " does something important\n")
		sb.WriteString("func function" + string(rune('A'+i%26)) + "(x int) int {\n")
		sb.WriteString("\tresult := x * 2\n")
		sb.WriteString("\tif result > 100 {\n")
		sb.WriteString("\t\treturn result - 50\n")
		sb.WriteString("\t}\n")
		sb.WriteString("\treturn result\n")
		sb.WriteString("}\n\n")
	}

	return sb.String()
}

func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
