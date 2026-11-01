package services

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jayyao97/zotigo/core/observability"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
)

// conversationSummaryInstruction is the prompt for summarizing conversation history.
const conversationSummaryInstruction = `Please summarize the conversation. Focus on:
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
</context_summary>`

// ProviderSummarizer implements Summarizer using an LLM provider.
//
// When configured with an observer (via WithSummarizerObserver), each
// SummarizeMessages / SummarizeText call emits a Langfuse generation
// of kind=compactor so the compactor's hidden token cost shows up in
// the trace tree alongside the main turn's generation.
type ProviderSummarizer struct {
	provider providers.Provider
	observer observability.Observer
	model    string // recorded on the generation; informational
}

// SummarizerOption configures a ProviderSummarizer at construction.
type SummarizerOption func(*ProviderSummarizer)

// WithSummarizerObserver attaches an observability.Observer that
// captures each summarization call as a generation. Pass model so the
// observer records the same string the agent's main generation uses,
// keeping cost rollups consistent across kinds.
//
// Passing nil observer is a silent no-op (Noop stays the default).
func WithSummarizerObserver(o observability.Observer, model string) SummarizerOption {
	return func(s *ProviderSummarizer) {
		if o != nil {
			s.observer = o
		}
		s.model = model
	}
}

// NewProviderSummarizer creates a summarizer that uses the given provider.
func NewProviderSummarizer(provider providers.Provider, opts ...SummarizerOption) *ProviderSummarizer {
	s := &ProviderSummarizer{
		provider: provider,
		observer: observability.Noop{},
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// SummarizeMessages generates a structured summary of messages using the LLM.
func (s *ProviderSummarizer) SummarizeMessages(ctx context.Context, messages []protocol.Message) (string, error) {
	var sb strings.Builder
	sb.WriteString(conversationSummaryInstruction)
	sb.WriteString("\n\nConversation to summarize:\n\n")

	// Append message content
	for _, msg := range messages {
		fmt.Fprintf(&sb, "[%s]: ", msg.Role)
		for _, part := range msg.Content {
			switch part.Type {
			case protocol.ContentTypeText:
				sb.WriteString(part.Text)
			case protocol.ContentTypeToolCall:
				if part.ToolCall != nil {
					fmt.Fprintf(&sb, "[Called tool: %s %s]", part.ToolCall.Name, part.ToolCall.Arguments)
				}
			case protocol.ContentTypeToolResult:
				if part.ToolResult != nil {
					result := part.ToolResult
					fmt.Fprintf(&sb, "[Tool result: %s] %s", result.ToolName, result.Text)
					if result.JSON != nil {
						data, err := json.Marshal(result.JSON)
						if err != nil {
							return "", fmt.Errorf("encode summary tool result: %w", err)
						}
						sb.Write(data)
					}
					for _, content := range result.Content {
						sb.WriteString(content.Text)
					}
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

	return s.summarize(ctx, reqMessages, nil)
}

// SummarizeText generates a summary of text with a specific instruction.
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

	return s.summarize(ctx, reqMessages, nil)
}

// SummarizePrompt keeps the main request's prefix intact, including tools and
// multimodal/reasoning parts. Only the compaction instruction is appended.
func (s *ProviderSummarizer) SummarizePrompt(ctx context.Context, messages []protocol.Message, toolList []tools.Tool) (string, error) {
	request := append([]protocol.Message(nil), messages...)
	request = append(request, protocol.NewUserMessage(`Summarize the entire conversation above for continuation after compaction. Recent messages will also be retained verbatim; describe their current state without treating completed work as pending. Runtime context messages are context, not user goals.
Do not call any tools or continue the task. Output only the summary as text.
`+conversationSummaryInstruction))
	return s.summarize(ctx, request, toolList)
}

// A partial or interrupted stream is not a summary: committing it would
// permanently remove history that the model never had a chance to preserve.
func (s *ProviderSummarizer) summarize(ctx context.Context, messages []protocol.Message, toolList []tools.Tool) (result string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	ctx = s.observer.StartGeneration(ctx, observability.GenerationCompactor, s.model, messages, toolList, nil)
	var usage *protocol.Usage
	defer func() { s.observer.EndGeneration(ctx, observability.GenerationOutput{Text: result}, usage, err) }()
	events, err := s.provider.StreamChat(ctx, messages, toolList)
	if err != nil {
		return "", fmt.Errorf("summarize request failed: %w", err)
	}
	defer func() {
		// Providers may send a terminal event after cancellation. Consume through
		// close so their unbuffered sends and stream cleanup can finish, retaining
		// any final usage even when a tool call made the summary unusable.
		cancel()
		for event := range events {
			if event.Usage != nil {
				usage = event.Usage
			}
		}
	}()
	var response strings.Builder
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case event, ok := <-events:
			if !ok {
				return "", fmt.Errorf("summarizer stream ended without completion")
			}
			switch event.Type {
			case protocol.EventTypeToolCallDelta, protocol.EventTypeToolCallEnd:
				return "", fmt.Errorf("summarizer attempted a tool call")
			case protocol.EventTypeContentDelta:
				if event.ContentPartDelta != nil && (event.ContentPartDelta.Type == "" || event.ContentPartDelta.Type == protocol.ContentTypeText) {
					response.WriteString(event.ContentPartDelta.Text)
				}
			case protocol.EventTypeError:
				usage = event.Usage
				if event.Error != nil {
					return "", fmt.Errorf("summarizer failed: %w", event.Error)
				}
				return "", fmt.Errorf("summarizer failed")
			case protocol.EventTypeFinish:
				usage = event.Usage
				if err := ctx.Err(); err != nil {
					return "", err
				}
				if event.FinishReason != "stop" {
					return "", fmt.Errorf("summarizer did not complete: %s", event.FinishReason)
				}
				if strings.TrimSpace(response.String()) == "" {
					return "", fmt.Errorf("empty response from summarizer")
				}
				return response.String(), nil
			}
		}
	}
}
