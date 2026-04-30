package services

import (
	"context"
	"fmt"
	"strings"

	"github.com/jayyao97/zotigo/core/observability"
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
//
// Named returns (result, err) let the deferred EndGeneration capture
// final state regardless of which return path the function takes.
func (s *ProviderSummarizer) SummarizeMessages(ctx context.Context, messages []protocol.Message) (result string, err error) {
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

	ctx = s.observer.StartGeneration(ctx, observability.GenerationCompactor, s.model, reqMessages, nil, nil)
	var usage *protocol.Usage
	defer func() {
		s.observer.EndGeneration(ctx, observability.GenerationOutput{Text: result}, usage, err)
	}()

	// Stream the response
	events, sErr := s.provider.StreamChat(ctx, reqMessages, nil)
	if sErr != nil {
		err = fmt.Errorf("summarize request failed: %w", sErr)
		return "", err
	}

	// Collect response + usage. Usage shows up in the finish event;
	// missing it on a successful summary just leaves cost telemetry
	// blank for that generation, never errors.
	var response strings.Builder
	for event := range events {
		switch event.Type {
		case protocol.EventTypeContentDelta:
			if event.ContentPartDelta != nil {
				response.WriteString(event.ContentPartDelta.Text)
			}
		case protocol.EventTypeFinish:
			usage = event.Usage
		}
	}

	result = response.String()
	if result == "" {
		err = fmt.Errorf("empty response from summarizer")
		return "", err
	}
	return result, nil
}

// SummarizeText generates a summary of text with a specific instruction.
func (s *ProviderSummarizer) SummarizeText(ctx context.Context, text string, instruction string) (result string, err error) {
	prompt := fmt.Sprintf("%s\n\nText to summarize:\n%s", instruction, text)

	reqMessages := []protocol.Message{
		{
			Role: protocol.RoleUser,
			Content: []protocol.ContentPart{
				{Type: protocol.ContentTypeText, Text: prompt},
			},
		},
	}

	ctx = s.observer.StartGeneration(ctx, observability.GenerationCompactor, s.model, reqMessages, nil, nil)
	var usage *protocol.Usage
	defer func() {
		s.observer.EndGeneration(ctx, observability.GenerationOutput{Text: result}, usage, err)
	}()

	events, sErr := s.provider.StreamChat(ctx, reqMessages, nil)
	if sErr != nil {
		err = fmt.Errorf("summarize text request failed: %w", sErr)
		return "", err
	}

	var response strings.Builder
	for event := range events {
		switch event.Type {
		case protocol.EventTypeContentDelta:
			if event.ContentPartDelta != nil {
				response.WriteString(event.ContentPartDelta.Text)
			}
		case protocol.EventTypeFinish:
			usage = event.Usage
		}
	}

	result = response.String()
	if result == "" {
		err = fmt.Errorf("empty response from summarizer")
		return "", err
	}
	return result, nil
}
