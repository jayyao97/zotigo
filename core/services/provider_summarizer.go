package services

import (
	"context"
	"fmt"
	"strings"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
)

// conversationSummaryInstruction is the prompt for summarizing conversation history.
const conversationSummaryInstruction = `Please summarize the following conversation. Focus on:
- The user's main goals and intent
- Key progress made
- Important files, paths, or data mentioned
- Any pending tasks or blockers

Format your response as XML:
<context_summary>
  <goal>Main goal</goal>
  <progress>Progress made</progress>
  <current_state>Current state</current_state>
  <pending>Pending tasks</pending>
  <key_info>Important files/data</key_info>
</context_summary>

Conversation to summarize:

`

// ProviderSummarizer implements Summarizer using an LLM provider
type ProviderSummarizer struct {
	provider providers.Provider
}

// NewProviderSummarizer creates a summarizer that uses the given provider
func NewProviderSummarizer(provider providers.Provider) *ProviderSummarizer {
	return &ProviderSummarizer{
		provider: provider,
	}
}

// SummarizeMessages generates a structured summary of messages using the LLM
func (s *ProviderSummarizer) SummarizeMessages(ctx context.Context, messages []protocol.Message) (string, error) {
	var sb strings.Builder
	sb.WriteString(conversationSummaryInstruction)

	// Append message content
	for _, msg := range messages {
		sb.WriteString(fmt.Sprintf("[%s]: ", msg.Role))
		for _, part := range msg.Content {
			switch part.Type {
			case protocol.ContentTypeText:
				sb.WriteString(part.Text)
			case protocol.ContentTypeToolCall:
				if part.ToolCall != nil {
					sb.WriteString(fmt.Sprintf("[Called tool: %s]", part.ToolCall.Name))
				}
			case protocol.ContentTypeToolResult:
				if part.ToolResult != nil {
					// Truncate long tool results
					text := part.ToolResult.Text
					if len(text) > 500 {
						text = text[:500] + "..."
					}
					sb.WriteString(fmt.Sprintf("[Tool result: %s]", text))
				}
			}
		}
		sb.WriteString("\n\n")
	}

	// Create request messages
	reqMessages := []protocol.Message{
		{
			Role: protocol.RoleUser,
			Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: sb.String()},
			},
		},
	}

	// Stream the response
	events, err := s.provider.StreamChat(ctx, reqMessages, nil)
	if err != nil {
		return "", fmt.Errorf("summarize request failed: %w", err)
	}

	// Collect response
	var response strings.Builder
	for event := range events {
		if event.Type == protocol.EventTypeContentDelta && event.ContentPartDelta != nil {
			response.WriteString(event.ContentPartDelta.Text)
		}
	}

	result := response.String()
	if result == "" {
		return "", fmt.Errorf("empty response from summarizer")
	}

	return result, nil
}

// SummarizeText generates a summary of text with a specific instruction
func (s *ProviderSummarizer) SummarizeText(ctx context.Context, text string, instruction string) (string, error) {
	prompt := fmt.Sprintf("%s\n\nText to summarize:\n%s", instruction, text)

	reqMessages := []protocol.Message{
		{
			Role: protocol.RoleUser,
			Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: prompt},
			},
		},
	}

	events, err := s.provider.StreamChat(ctx, reqMessages, nil)
	if err != nil {
		return "", fmt.Errorf("summarize text request failed: %w", err)
	}

	var response strings.Builder
	for event := range events {
		if event.Type == protocol.EventTypeContentDelta && event.ContentPartDelta != nil {
			response.WriteString(event.ContentPartDelta.Text)
		}
	}

	result := response.String()
	if result == "" {
		return "", fmt.Errorf("empty response from summarizer")
	}

	return result, nil
}
