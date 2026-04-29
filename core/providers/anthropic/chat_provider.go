package anthropic

import (
	"context"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
)

type ChatProvider struct {
	client         *anthropic.Client
	model          string
	thinkingLevel  string // "", "low", "medium", "high"
	thinkingBudget int64  // explicit override; 0 = use level default
}

func (p *ChatProvider) Name() string {
	return "anthropic-chat"
}

// thinkingBudgetTokens returns the budget_tokens for the given thinking level.
// An explicit budget override wins; otherwise the level string maps to a
// fixed token budget.
func thinkingBudgetTokens(level string, explicit int64) int64 {
	if explicit > 0 {
		return explicit
	}
	switch level {
	case "low":
		return 2048
	case "medium":
		return 8192
	case "high":
		return 32768
	default:
		return 0
	}
}

func (p *ChatProvider) StreamChat(ctx context.Context, messages []protocol.Message, toolsList []tools.Tool, opts ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	resolved := providers.ResolveOptions(opts)
	params, err := convertToAnthropicParams(messages, toolsList, resolved.ToolChoice)
	if err != nil {
		return nil, err
	}

	params.Model = anthropic.Model(p.model)

	// Configure thinking — per-call override wins over the provider default.
	level := resolved.ReasoningEffort
	if level == "" {
		level = p.thinkingLevel
	}
	budget := thinkingBudgetTokens(level, p.thinkingBudget)
	if budget > 0 {
		params.Thinking = anthropic.ThinkingConfigParamOfEnabled(budget)
		// MaxTokens must exceed budget; set a reasonable total
		if params.MaxTokens < budget+4096 {
			params.MaxTokens = budget + 4096
		}
	}

	stream := p.client.Messages.NewStreaming(ctx, params)

	ch := make(chan protocol.Event)

	go func() {
		defer close(ch)
		defer func() { _ = stream.Close() }()

		contentIndex := 0
		toolCallIndex := 0
		currentToolCall := &protocol.ToolCall{}
		inToolUse := false
		inThinking := false
		var thinkingText string
		var thinkingSignature string

		// Track usage across events:
		//   - message_start carries input_tokens / cache_creation_input_tokens
		//     / cache_read_input_tokens (absolute, not delta).
		//   - message_delta carries the final output_tokens.
		//
		// Trap: message_delta ALSO restates cache_creation/cache_read as
		// cumulative absolute values — they're the same numbers as
		// message_start, not increments. Clients that add them on top of
		// message_start (LangChain.js, early Cline) end up with cache
		// counts that exceed input_tokens, which is impossible. We dodge
		// this by (a) only reading OutputTokens from message_delta and
		// (b) using `=` everywhere, never `+=`. Don't change either.
		var usage protocol.Usage

		for stream.Next() {
			event := stream.Current()

			switch event.Type {
			case "message_start":
				// Capture input token counts from the initial message
				if event.Message.Usage.InputTokens > 0 {
					usage.InputTokens = int(event.Message.Usage.InputTokens)
				}
				if event.Message.Usage.CacheCreationInputTokens > 0 {
					usage.CacheCreationInputTokens = int(event.Message.Usage.CacheCreationInputTokens)
				}
				if event.Message.Usage.CacheReadInputTokens > 0 {
					usage.CacheReadInputTokens = int(event.Message.Usage.CacheReadInputTokens)
				}

			case "content_block_start":
				switch event.ContentBlock.Type {
				case "tool_use":
					inToolUse = true
					currentToolCall = &protocol.ToolCall{
						Index: toolCallIndex,
						ID:    event.ContentBlock.ID,
						Name:  event.ContentBlock.Name,
					}
					ch <- protocol.Event{
						Type:  protocol.EventTypeToolCallDelta,
						Index: toolCallIndex,
						ToolCallDelta: &protocol.ToolCallDelta{
							Type: protocol.ToolCallDeltaTypeID,
							ID:   event.ContentBlock.ID,
						},
					}
					ch <- protocol.Event{
						Type:  protocol.EventTypeToolCallDelta,
						Index: toolCallIndex,
						ToolCallDelta: &protocol.ToolCallDelta{
							Type: protocol.ToolCallDeltaTypeName,
							Name: event.ContentBlock.Name,
						},
					}
				case "thinking":
					inThinking = true
					thinkingText = ""
				}

			case "content_block_delta":
				switch event.Delta.Type {
				case "thinking_delta":
					thinkingText += event.Delta.Thinking
					ch <- protocol.NewReasoningDeltaEvent(event.Delta.Thinking)
				case "signature_delta":
					thinkingSignature += event.Delta.Signature
				case "text_delta":
					ch <- protocol.Event{
						Type:             protocol.EventTypeContentDelta,
						Index:            contentIndex,
						ContentPartDelta: &protocol.ContentPartDelta{Text: event.Delta.Text},
					}
				case "input_json_delta":
					ch <- protocol.Event{
						Type:  protocol.EventTypeToolCallDelta,
						Index: toolCallIndex,
						ToolCallDelta: &protocol.ToolCallDelta{
							Type:      protocol.ToolCallDeltaTypeArgs,
							Arguments: event.Delta.PartialJSON,
						},
					}
					currentToolCall.Arguments += event.Delta.PartialJSON
				}

			case "content_block_stop":
				if inToolUse {
					ch <- protocol.Event{
						Type:     protocol.EventTypeToolCallEnd,
						Index:    toolCallIndex,
						ToolCall: currentToolCall,
					}
					toolCallIndex++
					inToolUse = false
				} else if inThinking {
					ch <- protocol.Event{
						Type:  protocol.EventTypeContentEnd,
						Index: contentIndex,
						ContentPart: &protocol.ContentPart{
							Type:      protocol.ContentTypeReasoning,
							Text:      thinkingText,
							Signature: thinkingSignature,
						},
					}
					contentIndex++
					inThinking = false
					thinkingSignature = ""
				} else {
					ch <- protocol.Event{
						Type:  protocol.EventTypeContentEnd,
						Index: contentIndex,
					}
					contentIndex++
				}

			case "message_delta":
				if event.Usage.OutputTokens > 0 {
					usage.OutputTokens = int(event.Usage.OutputTokens)
				}
				if event.Delta.StopReason != "" {
					reason := mapStopReason(event.Delta.StopReason)
					finishEvt := protocol.NewFinishEvent(reason)
					finishEvt.Usage = &usage
					ch <- finishEvt
					return
				}

			case "message_stop":
				return
			}
		}

		if err := stream.Err(); err != nil {
			ch <- protocol.NewErrorEvent(providers.WrapIfContextLength(err))
		}
	}()

	return ch, nil
}

func mapStopReason(reason anthropic.StopReason) protocol.FinishReason {
	switch reason {
	case anthropic.StopReasonEndTurn:
		return protocol.FinishReasonStop
	case anthropic.StopReasonMaxTokens:
		return protocol.FinishReasonLength
	case anthropic.StopReasonToolUse:
		return protocol.FinishReasonToolCalls
	case anthropic.StopReasonStopSequence:
		return protocol.FinishReasonStop
	default:
		return protocol.FinishReasonUnknown
	}
}
