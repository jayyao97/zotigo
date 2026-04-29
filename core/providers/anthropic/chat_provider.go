package anthropic

import (
	"context"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
)

type ChatProvider struct {
	client        *anthropic.Client
	model         string
	thinkingLevel string // "", "low", "medium", "high"
}

func (p *ChatProvider) Name() string {
	return "anthropic-chat"
}

// effortFromLevel maps our cross-provider thinking_level string into
// Anthropic's adaptive output_config.effort enum. "" → empty enum,
// callers must gate before sending.
func effortFromLevel(level string) anthropic.OutputConfigEffort {
	switch level {
	case "low":
		return anthropic.OutputConfigEffortLow
	case "medium":
		return anthropic.OutputConfigEffortMedium
	case "high":
		return anthropic.OutputConfigEffortHigh
	default:
		return ""
	}
}

// maxTokensForLevel returns a generous max_tokens ceiling for the
// given thinking effort. Adaptive thinking output counts toward
// max_tokens, so the converter's 4096 default would silently truncate
// high-effort reasoning chains before the model gets to write its
// answer. Numbers mirror the old enabled-mode budget+4096 layout
// (2048/8192/32768 thinking budget + 4096 response room) rounded to
// power-of-two ceilings; callers that explicitly set MaxTokens higher
// keep their override.
func maxTokensForLevel(level string) int64 {
	switch level {
	case "low":
		return 8192
	case "medium":
		return 16384
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

	// Adaptive thinking — Claude 4.7+ rejects the legacy
	// `thinking.type: enabled` + budget_tokens shape; older 4.x still
	// accept adaptive, so we use it uniformly. Per-call ReasoningEffort
	// overrides the provider-level default. When level is "", we send
	// no thinking config at all (model decides on its own / no effort
	// hint).
	level := resolved.ReasoningEffort
	if level == "" {
		level = p.thinkingLevel
	}
	if level != "" {
		params.Thinking = anthropic.ThinkingConfigParamUnion{
			OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{
				Display: anthropic.ThinkingConfigAdaptiveDisplaySummarized,
			},
		}
		params.OutputConfig.Effort = effortFromLevel(level)
		if want := maxTokensForLevel(level); params.MaxTokens < want {
			params.MaxTokens = want
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
