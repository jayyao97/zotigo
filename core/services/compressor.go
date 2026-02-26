package services

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"text/template"

	"github.com/jayyao97/zotigo/core/protocol"
)

// Compressor manages conversation context by compressing older messages
// when the token count exceeds a threshold. It uses intelligent partitioning
// to preserve tool call chains and generates structured summaries.
type Compressor struct {
	mu sync.RWMutex

	config     CompressorConfig
	summarizer Summarizer
	counter    TokenCounter
}

// TokenCounter estimates token count for text
type TokenCounter func(text string) int

// Summarizer generates summaries using an LLM
type Summarizer interface {
	// SummarizeMessages generates a structured summary of messages
	SummarizeMessages(ctx context.Context, messages []protocol.Message) (string, error)

	// SummarizeText generates a summary of text with a specific instruction
	SummarizeText(ctx context.Context, text string, instruction string) (string, error)
}

// CompressorConfig holds configuration for the compressor
type CompressorConfig struct {
	// ContextWindowSize is the model's context window in tokens
	// If 0, defaults to 128000 (GPT-4 Turbo / Claude 3)
	ContextWindowSize int

	// TriggerRatio is when to trigger compression (default 0.7 = 70% of context)
	TriggerRatio float64

	// TargetRatio is the target after compression (default 0.5 = 50% of context)
	TargetRatio float64

	// PreserveRatio is how much of recent messages to preserve (default 0.3 = 30%)
	PreserveRatio float64

	// ToolOutputThreshold is max tokens for a single tool output before summarizing
	ToolOutputThreshold int

	// TokenCounter is an optional custom token counter
	TokenCounter TokenCounter
}

// DefaultCompressorConfig returns sensible defaults
func DefaultCompressorConfig() CompressorConfig {
	return CompressorConfig{
		ContextWindowSize:   128000, // GPT-4 Turbo / Claude 3 default
		TriggerRatio:        0.7,    // Trigger at 70%
		TargetRatio:         0.5,    // Target 50%
		PreserveRatio:       0.3,    // Preserve 30% of recent
		ToolOutputThreshold: 2000,   // Summarize outputs > 2000 tokens
		TokenCounter:        nil,    // Use default estimator
	}
}

// NewCompressor creates a new compressor with the given config
func NewCompressor(cfg CompressorConfig) *Compressor {
	// Apply defaults
	if cfg.ContextWindowSize <= 0 {
		cfg.ContextWindowSize = 128000
	}
	if cfg.TriggerRatio <= 0 {
		cfg.TriggerRatio = 0.7
	}
	if cfg.TargetRatio <= 0 {
		cfg.TargetRatio = 0.5
	}
	if cfg.PreserveRatio <= 0 {
		cfg.PreserveRatio = 0.3
	}
	if cfg.ToolOutputThreshold <= 0 {
		cfg.ToolOutputThreshold = 2000
	}
	if cfg.TokenCounter == nil {
		// Try to use accurate tokenizer, fallback to estimate
		if tk, err := DefaultTokenizer(); err == nil {
			cfg.TokenCounter = tk.AsTokenCounter()
		} else {
			cfg.TokenCounter = estimateTokens
		}
	}

	return &Compressor{
		config:  cfg,
		counter: cfg.TokenCounter,
	}
}

// SetSummarizer sets the LLM summarizer for high-quality summaries
func (c *Compressor) SetSummarizer(s Summarizer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.summarizer = s
}

// CompressionResult contains the result of a compression operation
type CompressionResult struct {
	OriginalTokens   int
	CompressedTokens int
	MessagesBefore   int
	MessagesAfter    int
	Summary          string
	Compressed       bool
	PartitionIndex   int // Index where messages were split
}

// NeedsCompression checks if the messages need compression
func (c *Compressor) NeedsCompression(messages []protocol.Message) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	tokens := c.countTokens(messages)
	threshold := int(float64(c.config.ContextWindowSize) * c.config.TriggerRatio)
	return tokens > threshold
}

// CountTokens returns the estimated token count for messages
func (c *Compressor) CountTokens(messages []protocol.Message) int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.countTokens(messages)
}

// Compress compresses the messages if they exceed the threshold.
// It uses intelligent partitioning to avoid breaking tool call chains.
func (c *Compressor) Compress(ctx context.Context, messages []protocol.Message) ([]protocol.Message, CompressionResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	result := CompressionResult{
		OriginalTokens: c.countTokens(messages),
		MessagesBefore: len(messages),
	}

	// Check if compression is needed
	threshold := int(float64(c.config.ContextWindowSize) * c.config.TriggerRatio)
	if result.OriginalTokens <= threshold {
		result.CompressedTokens = result.OriginalTokens
		result.MessagesAfter = len(messages)
		result.Compressed = false
		return messages, result, nil
	}

	// Separate system messages
	var systemMsgs []protocol.Message
	var conversationMsgs []protocol.Message

	for _, msg := range messages {
		if msg.Role == protocol.RoleSystem {
			systemMsgs = append(systemMsgs, msg)
		} else {
			conversationMsgs = append(conversationMsgs, msg)
		}
	}

	// If no conversation messages, nothing to compress
	if len(conversationMsgs) == 0 {
		result.CompressedTokens = result.OriginalTokens
		result.MessagesAfter = len(messages)
		result.Compressed = false
		return messages, result, nil
	}

	// Calculate how many tokens to preserve (30% of conversation)
	conversationTokens := c.countTokens(conversationMsgs)
	preserveTokens := int(float64(conversationTokens) * c.config.PreserveRatio)

	// Find safe partition point (doesn't break tool call chains)
	partitionIdx := c.findSafePartitionPoint(conversationMsgs, preserveTokens)
	result.PartitionIndex = partitionIdx

	// If partition is at the start, nothing to compress
	if partitionIdx <= 0 {
		result.CompressedTokens = result.OriginalTokens
		result.MessagesAfter = len(messages)
		result.Compressed = false
		return messages, result, nil
	}

	toCompress := conversationMsgs[:partitionIdx]
	toPreserve := conversationMsgs[partitionIdx:]

	// Generate summary
	var summary string
	var err error

	if c.summarizer != nil {
		summary, err = c.summarizer.SummarizeMessages(ctx, toCompress)
		if err != nil {
			// Fallback to simple summary
			summary = c.simpleSummary(toCompress)
		}
	} else {
		summary = c.simpleSummary(toCompress)
	}

	// Summarize long tool outputs in preserved messages
	toPreserve = c.summarizeToolOutputs(ctx, toPreserve)

	// Create summary message
	summaryMsg := protocol.Message{
		Role: protocol.RoleUser,
		Content: []protocol.ContentPart{
			{
				Type: protocol.ContentTypeText,
				Text: "[Previous conversation summary]\n" + summary,
			},
		},
	}

	// Rebuild messages: system + summary + preserved
	compressed := make([]protocol.Message, 0, len(systemMsgs)+1+len(toPreserve))
	compressed = append(compressed, systemMsgs...)
	compressed = append(compressed, summaryMsg)
	compressed = append(compressed, toPreserve...)

	result.Summary = summary
	result.CompressedTokens = c.countTokens(compressed)
	result.MessagesAfter = len(compressed)
	result.Compressed = true

	return compressed, result, nil
}

// findSafePartitionPoint finds a partition index that doesn't break tool call chains.
// It scans from the end to find a point where we have approximately preserveTokens,
// then adjusts to find the nearest safe boundary (before a user message).
func (c *Compressor) findSafePartitionPoint(messages []protocol.Message, preserveTokens int) int {
	if len(messages) == 0 {
		return 0
	}

	// Calculate cumulative tokens from the end
	cumulativeTokens := make([]int, len(messages))
	runningTotal := 0

	for i := len(messages) - 1; i >= 0; i-- {
		runningTotal += c.countSingleMessage(messages[i])
		cumulativeTokens[i] = runningTotal
	}

	// Find the rough partition point
	roughIdx := 0
	for i := len(messages) - 1; i >= 0; i-- {
		if cumulativeTokens[i] >= preserveTokens {
			roughIdx = i
			break
		}
	}

	// Adjust to safe boundary: find the nearest user message boundary
	// A safe boundary is just before a user message (after the previous tool/assistant)
	for idx := roughIdx; idx < len(messages); idx++ {
		if messages[idx].Role == protocol.RoleUser {
			// Check if previous message exists and isn't a tool message
			// (which would mean we're in the middle of a chain)
			if idx > 0 && messages[idx-1].Role == protocol.RoleTool {
				continue // Keep looking, this breaks a chain
			}
			return idx
		}
	}

	// If no safe boundary found, try searching backwards
	for idx := roughIdx; idx > 0; idx-- {
		if messages[idx].Role == protocol.RoleUser {
			if idx > 0 && messages[idx-1].Role == protocol.RoleTool {
				continue
			}
			return idx
		}
	}

	// Fallback: just use the rough index
	return roughIdx
}

// summarizeToolOutputs summarizes tool outputs that exceed the threshold
func (c *Compressor) summarizeToolOutputs(ctx context.Context, messages []protocol.Message) []protocol.Message {
	result := make([]protocol.Message, len(messages))
	copy(result, messages)

	for i, msg := range result {
		if msg.Role == protocol.RoleTool {
			newContent := make([]protocol.ContentPart, 0, len(msg.Content))
			for _, part := range msg.Content {
				if part.Type == protocol.ContentTypeToolResult && part.ToolResult != nil {
					text := part.ToolResult.Text
					tokens := c.counter(text)

					if tokens > c.config.ToolOutputThreshold {
						// Try LLM summarization first
						var summarized string
						if c.summarizer != nil {
							var err error
							summarized, err = c.summarizer.SummarizeText(ctx, text, ToolOutputSummarizeInstruction)
							if err != nil {
								// Fallback to truncation
								summarized = c.truncateText(text, c.config.ToolOutputThreshold)
							}
						} else {
							summarized = c.truncateText(text, c.config.ToolOutputThreshold)
						}

						newPart := part
						newPart.ToolResult = &protocol.ToolResult{
							ToolCallID: part.ToolResult.ToolCallID,
							ToolName:   part.ToolResult.ToolName,
							Type:       part.ToolResult.Type,
							Text:       summarized,
							IsError:    part.ToolResult.IsError,
						}
						newContent = append(newContent, newPart)
						continue
					}
				}
				newContent = append(newContent, part)
			}
			result[i].Content = newContent
		}
	}

	return result
}

// TruncateToolResults is a simpler version that just truncates without LLM
func (c *Compressor) TruncateToolResults(messages []protocol.Message, maxResultTokens int) []protocol.Message {
	if maxResultTokens <= 0 {
		maxResultTokens = c.config.ToolOutputThreshold
	}

	result := make([]protocol.Message, len(messages))
	copy(result, messages)

	for i, msg := range result {
		if msg.Role == protocol.RoleTool {
			newContent := make([]protocol.ContentPart, 0, len(msg.Content))
			for _, part := range msg.Content {
				if part.Type == protocol.ContentTypeToolResult && part.ToolResult != nil {
					text := part.ToolResult.Text
					tokens := c.counter(text)
					if tokens > maxResultTokens {
						truncated := c.truncateText(text, maxResultTokens)
						newPart := part
						newPart.ToolResult = &protocol.ToolResult{
							ToolCallID: part.ToolResult.ToolCallID,
							ToolName:   part.ToolResult.ToolName,
							Type:       part.ToolResult.Type,
							Text:       truncated + "\n\n[... truncated ...]",
							IsError:    part.ToolResult.IsError,
						}
						newContent = append(newContent, newPart)
						continue
					}
				}
				newContent = append(newContent, part)
			}
			result[i].Content = newContent
		}
	}

	return result
}

// countTokens estimates the total token count for messages
func (c *Compressor) countTokens(messages []protocol.Message) int {
	total := 0
	for _, msg := range messages {
		total += c.countSingleMessage(msg)
	}
	return total
}

// countSingleMessage counts tokens for a single message
func (c *Compressor) countSingleMessage(msg protocol.Message) int {
	total := 4 // Approximate overhead per message

	for _, part := range msg.Content {
		switch part.Type {
		case protocol.ContentTypeText:
			total += c.counter(part.Text)
		case protocol.ContentTypeToolCall:
			if part.ToolCall != nil {
				total += c.counter(part.ToolCall.Name)
				total += c.counter(part.ToolCall.Arguments)
			}
		case protocol.ContentTypeToolResult:
			if part.ToolResult != nil {
				total += c.counter(part.ToolResult.Text)
			}
		}
	}
	return total
}

// summaryData holds extracted facts from a conversation for template rendering.
type summaryData struct {
	Goal    string
	Tools   []string // e.g. "read_file (3 times)"
	Files   []string
	HasMore bool // true when files were truncated
}

// simpleSummaryTmpl renders summaryData into a <context_summary> XML block.
var simpleSummaryTmpl = template.Must(template.New("summary").Parse(`<context_summary>
  <goal>{{.Goal}}</goal>
  <progress>
{{- if .Tools}}
{{- range .Tools}}
    - Used {{.}}
{{- end}}
{{- else}}
    - Discussion without tool usage
{{- end}}
  </progress>
{{- if .Files}}
  <key_info>
{{- range .Files}}
    - {{.}}
{{- end}}
{{- if .HasMore}}
    - ... and more files
{{- end}}
  </key_info>
{{- end}}
</context_summary>`))

// simpleSummary creates a basic summary without using an LLM.
func (c *Compressor) simpleSummary(messages []protocol.Message) string {
	data := c.extractSummaryData(messages)
	var buf strings.Builder
	if err := simpleSummaryTmpl.Execute(&buf, data); err != nil {
		return "<context_summary><goal>General conversation</goal></context_summary>"
	}
	return buf.String()
}

// extractSummaryData walks messages and pulls out goals, tool usage, and file paths.
func (c *Compressor) extractSummaryData(messages []protocol.Message) summaryData {
	var goals []string
	toolCounts := make(map[string]int)
	fileSet := make(map[string]bool)

	for _, msg := range messages {
		for _, part := range msg.Content {
			switch part.Type {
			case protocol.ContentTypeText:
				if msg.Role == protocol.RoleUser && len(part.Text) > 0 {
					goals = append(goals, firstSentence(part.Text, 150))
				}
			case protocol.ContentTypeToolCall:
				if part.ToolCall != nil {
					toolCounts[part.ToolCall.Name]++
					if p := extractPath(part.ToolCall.Arguments); p != "" {
						fileSet[p] = true
					}
				}
			}
		}
	}

	// Build goal string
	goal := "General conversation"
	if len(goals) > 0 {
		goal = goals[0]
		if len(goals) > 1 {
			goal += " (and related requests)"
		}
	}

	// Build sorted tool list
	var toolList []string
	for name, count := range toolCounts {
		toolList = append(toolList, fmt.Sprintf("%s (%d times)", name, count))
	}

	// Collect files, cap at 10
	const maxFiles = 10
	var files []string
	for f := range fileSet {
		if len(files) >= maxFiles {
			break
		}
		files = append(files, f)
	}

	return summaryData{
		Goal:    goal,
		Tools:   toolList,
		Files:   files,
		HasMore: len(fileSet) > maxFiles,
	}
}

// firstSentence returns the first sentence of text, capped at maxLen.
func firstSentence(text string, maxLen int) string {
	if idx := strings.Index(text, "."); idx > 0 && idx < maxLen {
		return text[:idx+1]
	}
	if len(text) > maxLen {
		return text[:maxLen] + "..."
	}
	return text
}

// extractPath pulls a "path" value out of a JSON argument string.
// Best-effort; returns empty string if not found.
func extractPath(argsJSON string) string {
	idx := strings.Index(argsJSON, `"path"`)
	if idx < 0 {
		return ""
	}
	rest := argsJSON[idx+len(`"path"`):]
	// skip to the colon then the opening quote of the value
	start := strings.Index(rest, `"`)
	if start < 0 {
		return ""
	}
	rest = rest[start+1:]
	end := strings.Index(rest, `"`)
	if end <= 0 {
		return ""
	}
	return rest[:end]
}

// truncateText truncates text to approximately the given token count
func (c *Compressor) truncateText(text string, maxTokens int) string {
	maxChars := maxTokens * 4 // Rough estimate
	if len(text) <= maxChars {
		return text
	}

	// Try to truncate at a newline
	truncated := text[:maxChars]
	if idx := strings.LastIndex(truncated, "\n"); idx > maxChars/2 {
		return truncated[:idx]
	}

	return truncated
}

// estimateTokens provides a rough token count estimation
func estimateTokens(text string) int {
	if len(text) == 0 {
		return 0
	}
	return (len(text) + 3) / 4
}

// ToolOutputSummarizeInstruction is the prompt for summarizing tool outputs
const ToolOutputSummarizeInstruction = `Summarize this tool output concisely while preserving:
- Error messages and stack traces
- File paths and line numbers
- Key data points and results
- Status/success indicators

Keep it informative but brief.`
