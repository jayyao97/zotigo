package services

import (
	"context"
	"strings"
	"testing"

	"github.com/jayyao97/zotigo/core/protocol"
)

func TestCompressor_NeedsCompression(t *testing.T) {
	// Create a compressor with a small context window for testing
	c := NewCompressor(CompressorConfig{
		ContextWindowSize: 100, // 100 tokens
		TriggerRatio:      0.7, // 70 tokens threshold
	})

	// Create messages with ~20 tokens (should not need compression)
	smallMsgs := []protocol.Message{
		{
			Role: protocol.RoleUser,
			Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: strings.Repeat("word ", 10)}, // ~50 chars = ~12 tokens
			},
		},
	}

	if c.NeedsCompression(smallMsgs) {
		t.Error("Small messages should not need compression")
	}

	// Create messages with many tokens (should need compression)
	largeMsgs := []protocol.Message{
		{
			Role: protocol.RoleUser,
			Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: strings.Repeat("word ", 200)}, // ~1000 chars = ~250 tokens
			},
		},
	}

	if !c.NeedsCompression(largeMsgs) {
		t.Error("Large messages should need compression")
	}
}

func TestCompressor_CountTokens(t *testing.T) {
	c := NewCompressor(DefaultCompressorConfig())

	msgs := []protocol.Message{
		{
			Role: protocol.RoleUser,
			Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: "Hello world"}, // 11 chars = ~3 tokens
			},
		},
		{
			Role: protocol.RoleAssistant,
			Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: "Hi there"}, // 8 chars = ~2 tokens
			},
		},
	}

	tokens := c.CountTokens(msgs)
	// Should be approximately 5 content tokens + 8 overhead (4 per message)
	if tokens < 10 || tokens > 20 {
		t.Errorf("Expected ~13 tokens, got %d", tokens)
	}
}

func TestCompressor_Compress(t *testing.T) {
	c := NewCompressor(CompressorConfig{
		ContextWindowSize: 50,
		TriggerRatio:      0.5, // Trigger at 25 tokens
		PreserveRatio:     0.3, // Preserve 30%
	})

	// Create messages that exceed the threshold
	msgs := []protocol.Message{
		{
			Role: protocol.RoleSystem,
			Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: "System prompt"},
			},
		},
		{
			Role: protocol.RoleUser,
			Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: strings.Repeat("old message ", 20)},
			},
		},
		{
			Role: protocol.RoleAssistant,
			Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: strings.Repeat("old response ", 20)},
			},
		},
		{
			Role: protocol.RoleUser,
			Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: "Recent message"},
			},
		},
		{
			Role: protocol.RoleAssistant,
			Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: "Recent response"},
			},
		},
	}

	ctx := context.Background()
	compressed, result, err := c.Compress(ctx, msgs)
	if err != nil {
		t.Fatalf("Compress failed: %v", err)
	}

	if !result.Compressed {
		t.Error("Should have compressed")
	}

	if result.MessagesAfter >= result.MessagesBefore {
		t.Errorf("Should have fewer messages after compression: %d >= %d",
			result.MessagesAfter, result.MessagesBefore)
	}

	// Check that system message is preserved
	if compressed[0].Role != protocol.RoleSystem {
		t.Error("System message should be preserved")
	}

	// Check that recent messages are preserved
	lastMsg := compressed[len(compressed)-1]
	if lastMsg.Content[0].Text != "Recent response" {
		t.Error("Recent messages should be preserved")
	}
}

func TestCompressor_NoCompressionNeeded(t *testing.T) {
	c := NewCompressor(CompressorConfig{
		ContextWindowSize: 10000,
		TriggerRatio:      0.7,
	})

	msgs := []protocol.Message{
		{
			Role: protocol.RoleUser,
			Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: "Hello"},
			},
		},
	}

	ctx := context.Background()
	compressed, result, err := c.Compress(ctx, msgs)
	if err != nil {
		t.Fatalf("Compress failed: %v", err)
	}

	if result.Compressed {
		t.Error("Should not have compressed")
	}

	if len(compressed) != len(msgs) {
		t.Error("Messages should be unchanged")
	}
}

func TestCompressor_TruncateToolResults(t *testing.T) {
	c := NewCompressor(DefaultCompressorConfig())

	longResult := strings.Repeat("line\n", 1000) // Very long result

	msgs := []protocol.Message{
		{
			Role: protocol.RoleTool,
			Content: []protocol.ContentPart{
				{
					Type: protocol.ContentTypeToolResult,
					ToolResult: &protocol.ToolResult{
						ToolCallID: "call_123",
						Type:       protocol.ToolResultTypeText,
						Text:       longResult,
					},
				},
			},
		},
	}

	truncated := c.TruncateToolResults(msgs, 100) // Max 100 tokens

	if len(truncated) != 1 {
		t.Error("Should have same number of messages")
	}

	resultText := truncated[0].Content[0].ToolResult.Text
	if !strings.Contains(resultText, "truncated") {
		t.Error("Should indicate truncation")
	}

	if len(resultText) >= len(longResult) {
		t.Error("Result should be shorter")
	}
}

func TestCompressor_SimpleSummary(t *testing.T) {
	c := NewCompressor(DefaultCompressorConfig())

	msgs := []protocol.Message{
		{
			Role: protocol.RoleUser,
			Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: "Can you help me fix a bug?"},
			},
		},
		{
			Role: protocol.RoleAssistant,
			Content: []protocol.ContentPart{
				{
					Type: protocol.ContentTypeToolCall,
					ToolCall: &protocol.ToolCall{
						Name:      "read_file",
						Arguments: `{"path": "test.go"}`,
					},
				},
			},
		},
	}

	summary := c.simpleSummary(msgs)

	// Check XML structure
	if !strings.Contains(summary, "<context_summary>") {
		t.Error("Summary should have context_summary tag")
	}
	if !strings.Contains(summary, "<goal>") {
		t.Error("Summary should have goal tag")
	}
	if !strings.Contains(summary, "<progress>") {
		t.Error("Summary should have progress tag")
	}

	// Check content
	if !strings.Contains(summary, "bug") {
		t.Error("Summary should mention topic")
	}
	if !strings.Contains(summary, "read_file") {
		t.Error("Summary should mention tools used")
	}
}

func TestCompressor_FindSafePartitionPoint(t *testing.T) {
	c := NewCompressor(CompressorConfig{
		ContextWindowSize: 1000,
		PreserveRatio:     0.3,
	})

	// Test case: don't break tool call chains
	// Sequence: user -> assistant(tool_call) -> tool -> user -> assistant
	msgs := []protocol.Message{
		{Role: protocol.RoleUser, Content: []protocol.ContentPart{{Type: protocol.ContentTypeText, Text: "First request"}}},
		{Role: protocol.RoleAssistant, Content: []protocol.ContentPart{{Type: protocol.ContentTypeToolCall, ToolCall: &protocol.ToolCall{Name: "test"}}}},
		{Role: protocol.RoleTool, Content: []protocol.ContentPart{{Type: protocol.ContentTypeToolResult, ToolResult: &protocol.ToolResult{Text: "result"}}}},
		{Role: protocol.RoleUser, Content: []protocol.ContentPart{{Type: protocol.ContentTypeText, Text: "Second request"}}},
		{Role: protocol.RoleAssistant, Content: []protocol.ContentPart{{Type: protocol.ContentTypeText, Text: "Response"}}},
	}

	// With 30% preserve ratio, we want to keep the last ~30% of tokens
	// The safe partition point should be at index 3 (before "Second request")
	// NOT at index 1 (which would break the tool chain)
	partitionIdx := c.findSafePartitionPoint(msgs, 50)

	// The partition should not be between assistant(tool_call) and tool
	if partitionIdx == 2 {
		t.Error("Partition should not break tool call chain (between assistant and tool)")
	}

	// The partition should be at a user message that doesn't follow a tool message
	if partitionIdx > 0 && partitionIdx < len(msgs) {
		if msgs[partitionIdx].Role != protocol.RoleUser {
			// It's OK if it falls on user message or at boundaries
		}
		// Check it doesn't break a chain
		if partitionIdx > 0 && msgs[partitionIdx-1].Role == protocol.RoleTool {
			t.Errorf("Partition at %d breaks tool chain (previous message is tool)", partitionIdx)
		}
	}
}

func TestCompressor_DynamicThreshold(t *testing.T) {
	// Test that threshold is calculated dynamically based on context window
	smallWindow := NewCompressor(CompressorConfig{
		ContextWindowSize: 200, // Small window: threshold = 200 * 0.7 = 140 tokens
		TriggerRatio:      0.7,
	})

	largeWindow := NewCompressor(CompressorConfig{
		ContextWindowSize: 100000, // Large window: threshold = 70000 tokens
		TriggerRatio:      0.7,
	})

	// ~250 tokens (1000 chars / 4)
	msgs := []protocol.Message{
		{
			Role: protocol.RoleUser,
			Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: strings.Repeat("word ", 200)}, // ~1000 chars = ~250 tokens
			},
		},
	}

	// Small window should need compression (250 > 140)
	if !smallWindow.NeedsCompression(msgs) {
		t.Error("Small context window should need compression")
	}

	// Large window should not need compression (250 < 70000)
	if largeWindow.NeedsCompression(msgs) {
		t.Error("Large context window should not need compression")
	}
}

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		text        string
		minExpected int
		maxExpected int
	}{
		{"", 0, 0},
		{"hello", 1, 3},
		{"hello world", 2, 5},
		{strings.Repeat("word ", 100), 100, 150},
	}

	for _, tc := range tests {
		tokens := estimateTokens(tc.text)
		if tokens < tc.minExpected || tokens > tc.maxExpected {
			t.Errorf("estimateTokens(%q) = %d, expected between %d and %d",
				tc.text[:min(len(tc.text), 20)], tokens, tc.minExpected, tc.maxExpected)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// MockSummarizer for testing
type MockSummarizer struct {
	MessagesSummary string
	TextSummary     string
	ShouldFail      bool
}

func (m *MockSummarizer) SummarizeMessages(ctx context.Context, messages []protocol.Message) (string, error) {
	if m.ShouldFail {
		return "", context.Canceled
	}
	return m.MessagesSummary, nil
}

func (m *MockSummarizer) SummarizeText(ctx context.Context, text string, instruction string) (string, error) {
	if m.ShouldFail {
		return "", context.Canceled
	}
	return m.TextSummary, nil
}

func TestCompressor_WithSummarizer(t *testing.T) {
	c := NewCompressor(CompressorConfig{
		ContextWindowSize: 50,
		TriggerRatio:      0.5,
		PreserveRatio:     0.3,
	})

	mockSummarizer := &MockSummarizer{
		MessagesSummary: "<context_summary><goal>Test goal</goal></context_summary>",
	}
	c.SetSummarizer(mockSummarizer)

	msgs := []protocol.Message{
		{
			Role: protocol.RoleUser,
			Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: strings.Repeat("old ", 100)},
			},
		},
		{
			Role: protocol.RoleAssistant,
			Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: strings.Repeat("response ", 100)},
			},
		},
		{
			Role: protocol.RoleUser,
			Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: "Recent"},
			},
		},
	}

	ctx := context.Background()
	compressed, result, err := c.Compress(ctx, msgs)
	if err != nil {
		t.Fatalf("Compress failed: %v", err)
	}

	if !result.Compressed {
		t.Error("Should have compressed")
	}

	// Check that the mock summarizer's output is used
	if !strings.Contains(result.Summary, "Test goal") {
		t.Error("Should use mock summarizer output")
	}

	_ = compressed // Use the variable
}

func TestCompressor_SummarizerFallback(t *testing.T) {
	c := NewCompressor(CompressorConfig{
		ContextWindowSize: 50,
		TriggerRatio:      0.5,
		PreserveRatio:     0.3,
	})

	// Set a summarizer that fails
	mockSummarizer := &MockSummarizer{ShouldFail: true}
	c.SetSummarizer(mockSummarizer)

	// Create more messages with proper conversation structure
	msgs := []protocol.Message{
		{
			Role: protocol.RoleUser,
			Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: strings.Repeat("old user message ", 50)},
			},
		},
		{
			Role: protocol.RoleAssistant,
			Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: strings.Repeat("old assistant response ", 50)},
			},
		},
		{
			Role: protocol.RoleUser,
			Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: "Recent message"},
			},
		},
		{
			Role: protocol.RoleAssistant,
			Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: "Recent response"},
			},
		},
	}

	ctx := context.Background()
	_, result, err := c.Compress(ctx, msgs)
	if err != nil {
		t.Fatalf("Compress failed: %v", err)
	}

	// Should fallback to simple summary
	if !result.Compressed {
		t.Error("Should have compressed with fallback")
	}

	// Simple summary should have XML structure
	if !strings.Contains(result.Summary, "<context_summary>") {
		t.Error("Fallback should produce XML summary")
	}
}

// ============ Boundary Condition Tests ============

func TestCompressor_EmptyMessages(t *testing.T) {
	c := NewCompressor(DefaultCompressorConfig())
	ctx := context.Background()

	compressed, result, err := c.Compress(ctx, []protocol.Message{})
	if err != nil {
		t.Fatalf("Compress failed: %v", err)
	}

	if result.Compressed {
		t.Error("Empty messages should not be compressed")
	}
	if len(compressed) != 0 {
		t.Error("Should return empty slice")
	}
}

func TestCompressor_OnlySystemMessage(t *testing.T) {
	c := NewCompressor(CompressorConfig{
		ContextWindowSize: 10, // Very small to trigger compression check
		TriggerRatio:      0.5,
	})
	ctx := context.Background()

	msgs := []protocol.Message{
		{
			Role: protocol.RoleSystem,
			Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: "You are a helpful assistant."},
			},
		},
	}

	compressed, result, err := c.Compress(ctx, msgs)
	if err != nil {
		t.Fatalf("Compress failed: %v", err)
	}

	// System messages alone should not be compressed (no conversation to compress)
	if result.Compressed {
		t.Error("Only system messages should not be compressed")
	}
	if len(compressed) != 1 {
		t.Error("Should preserve system message")
	}
}

func TestCompressor_OnlyToolMessages(t *testing.T) {
	c := NewCompressor(CompressorConfig{
		ContextWindowSize: 100,
		TriggerRatio:      0.1, // Low threshold to trigger
		PreserveRatio:     0.3,
	})
	ctx := context.Background()

	msgs := []protocol.Message{
		{
			Role: protocol.RoleTool,
			Content: []protocol.ContentPart{
				{
					Type: protocol.ContentTypeToolResult,
					ToolResult: &protocol.ToolResult{
						ToolCallID: "call_1",
						Text:       strings.Repeat("result ", 100),
					},
				},
			},
		},
	}

	_, result, err := c.Compress(ctx, msgs)
	if err != nil {
		t.Fatalf("Compress failed: %v", err)
	}

	// Tool messages without user messages won't find a safe partition point
	// This is expected behavior
	t.Logf("Compressed: %v, PartitionIndex: %d", result.Compressed, result.PartitionIndex)
}

func TestCompressor_SingleUserMessage(t *testing.T) {
	c := NewCompressor(CompressorConfig{
		ContextWindowSize: 10,
		TriggerRatio:      0.1, // Very low to trigger
		PreserveRatio:     0.3,
	})
	ctx := context.Background()

	msgs := []protocol.Message{
		{
			Role: protocol.RoleUser,
			Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: strings.Repeat("test message ", 50)},
			},
		},
	}

	_, result, err := c.Compress(ctx, msgs)
	if err != nil {
		t.Fatalf("Compress failed: %v", err)
	}

	// Single message can't really be compressed (nothing to summarize before it)
	t.Logf("Compressed: %v, PartitionIndex: %d", result.Compressed, result.PartitionIndex)
}

func TestCompressor_PreservesToolChain(t *testing.T) {
	c := NewCompressor(CompressorConfig{
		ContextWindowSize: 100,
		TriggerRatio:      0.1,
		PreserveRatio:     0.5, // Preserve half
	})
	ctx := context.Background()

	// Create a sequence with tool chain at the end
	msgs := []protocol.Message{
		{Role: protocol.RoleUser, Content: []protocol.ContentPart{{Type: protocol.ContentTypeText, Text: "Old request"}}},
		{Role: protocol.RoleAssistant, Content: []protocol.ContentPart{{Type: protocol.ContentTypeText, Text: "Old response"}}},
		{Role: protocol.RoleUser, Content: []protocol.ContentPart{{Type: protocol.ContentTypeText, Text: "New request"}}},
		{Role: protocol.RoleAssistant, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeToolCall, ToolCall: &protocol.ToolCall{ID: "1", Name: "test", Arguments: "{}"}},
		}},
		{Role: protocol.RoleTool, Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeToolResult, ToolResult: &protocol.ToolResult{ToolCallID: "1", Text: "result"}},
		}},
	}

	compressed, result, err := c.Compress(ctx, msgs)
	if err != nil {
		t.Fatalf("Compress failed: %v", err)
	}

	if result.Compressed {
		// Check that the tool chain is not broken
		hasToolCall := false
		hasToolResult := false
		for _, msg := range compressed {
			for _, part := range msg.Content {
				if part.Type == protocol.ContentTypeToolCall {
					hasToolCall = true
				}
				if part.Type == protocol.ContentTypeToolResult {
					hasToolResult = true
				}
			}
		}

		// If we have tool call, we must have tool result (chain preserved)
		if hasToolCall && !hasToolResult {
			t.Error("Tool chain broken: has tool call but no tool result")
		}
		if !hasToolCall && hasToolResult {
			t.Error("Tool chain broken: has tool result but no tool call")
		}
	}
}

func TestCompressor_CompressionReducesTokens(t *testing.T) {
	c := NewCompressor(CompressorConfig{
		ContextWindowSize: 100,
		TriggerRatio:      0.1,
		PreserveRatio:     0.3,
	})
	ctx := context.Background()

	// Create many messages to compress
	msgs := []protocol.Message{
		{Role: protocol.RoleSystem, Content: []protocol.ContentPart{{Type: protocol.ContentTypeText, Text: "System"}}},
	}
	for i := 0; i < 10; i++ {
		msgs = append(msgs,
			protocol.Message{Role: protocol.RoleUser, Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: strings.Repeat("user message ", 10)},
			}},
			protocol.Message{Role: protocol.RoleAssistant, Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: strings.Repeat("assistant response ", 10)},
			}},
		)
	}

	_, result, err := c.Compress(ctx, msgs)
	if err != nil {
		t.Fatalf("Compress failed: %v", err)
	}

	if result.Compressed {
		if result.CompressedTokens >= result.OriginalTokens {
			t.Errorf("Compression should reduce tokens: %d >= %d",
				result.CompressedTokens, result.OriginalTokens)
		}
		t.Logf("Token reduction: %d -> %d (%.1f%%)",
			result.OriginalTokens, result.CompressedTokens,
			float64(result.OriginalTokens-result.CompressedTokens)/float64(result.OriginalTokens)*100)
	}
}

func TestCompressor_LargeToolOutput(t *testing.T) {
	c := NewCompressor(CompressorConfig{
		ContextWindowSize:   10000,
		ToolOutputThreshold: 50, // Low threshold
	})

	// Create a tool result with large output
	largeOutput := strings.Repeat("This is a very long output line.\n", 100)
	msgs := []protocol.Message{
		{
			Role: protocol.RoleTool,
			Content: []protocol.ContentPart{
				{
					Type: protocol.ContentTypeToolResult,
					ToolResult: &protocol.ToolResult{
						ToolCallID: "call_1",
						Type:       protocol.ToolResultTypeText,
						Text:       largeOutput,
					},
				},
			},
		},
	}

	truncated := c.TruncateToolResults(msgs, 50)

	resultText := truncated[0].Content[0].ToolResult.Text
	if len(resultText) >= len(largeOutput) {
		t.Error("Large tool output should be truncated")
	}
	if !strings.Contains(resultText, "truncated") {
		t.Error("Should indicate truncation")
	}
}

func TestCompressor_AccurateTokenCounting(t *testing.T) {
	// Create compressor with default (accurate) tokenizer
	c := NewCompressor(DefaultCompressorConfig())

	// Known token counts (approximate)
	tests := []struct {
		text      string
		minTokens int
		maxTokens int
	}{
		{"Hello", 1, 2},
		{"Hello, world!", 3, 5},
		{"The quick brown fox jumps over the lazy dog.", 8, 12},
	}

	for _, tc := range tests {
		msg := []protocol.Message{{
			Role: protocol.RoleUser,
			Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: tc.text},
			},
		}}

		tokens := c.CountTokens(msg)
		// Account for message overhead (4 tokens)
		contentTokens := tokens - 4

		if contentTokens < tc.minTokens || contentTokens > tc.maxTokens {
			t.Errorf("CountTokens for %q = %d (content: %d), expected content between %d and %d",
				tc.text, tokens, contentTokens, tc.minTokens, tc.maxTokens)
		}
	}
}
