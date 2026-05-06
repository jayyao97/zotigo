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
// answer. Numbers retain the spirit of the old enabled-mode mapping
// (low/medium/high thinking budgets of 2048/8192/32768 plus 4096 for
// response): low and medium round their budget+4096 up to the next
// power of two (8192, 16384); high keeps 32768 because that's both
// the old high-budget cap and around the practical output ceiling
// most current Anthropic models accept. Callers that explicitly set
// MaxTokens higher keep their override.
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
	applyThinkingConfig(&params, level, resolved.ToolChoice)

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
		//   - message_delta carries the final cumulative usage.
		//
		// Trap: message_delta restates input/cache values as cumulative
		// absolute values, not increments. Clients that add them on top
		// of message_start (LangChain.js, early Cline) end up with cache
		// counts that exceed input_tokens, which is impossible. We dodge
		// this by using `=` everywhere, never `+=`. Don't change that.
		var usage protocol.Usage

		for stream.Next() {
			event := stream.Current()

			switch event.Type {
			case "message_start":
				updateUsage(&usage,
					event.Message.Usage.InputTokens,
					event.Message.Usage.CacheCreationInputTokens,
					event.Message.Usage.CacheReadInputTokens,
					event.Message.Usage.OutputTokens,
				)

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
				updateUsage(&usage,
					event.Usage.InputTokens,
					event.Usage.CacheCreationInputTokens,
					event.Usage.CacheReadInputTokens,
					event.Usage.OutputTokens,
				)
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

func applyThinkingConfig(params *anthropic.MessageNewParams, level string, toolChoice providers.ToolChoice) {
	// Anthropic rejects thinking when tool_choice forces tool use.
	// Classifier calls pin record_decision, so those must stay
	// non-thinking even when the main profile has thinking enabled.
	if level != "" && toolChoice.Mode == providers.ToolChoiceAuto {
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
}

func updateUsage(usage *protocol.Usage, input, cacheCreate, cacheRead, output int64) {
	if input > 0 {
		usage.InputTokens = int(input)
	}
	if cacheCreate > 0 {
		usage.CacheCreationInputTokens = int(cacheCreate)
	}
	if cacheRead > 0 {
		usage.CacheReadInputTokens = int(cacheRead)
	}
	if output > 0 {
		usage.OutputTokens = int(output)
	}
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
