package anthropic

import (
	"context"
	"encoding/json"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/tools"
)

type ChatProvider struct {
	client *anthropic.Client
	model  string
}

func (p *ChatProvider) Name() string {
	return "anthropic-chat"
}

func (p *ChatProvider) StreamChat(ctx context.Context, messages []protocol.Message, toolsList []tools.Tool) (<-chan protocol.Event, error) {
	params, err := convertToAnthropicParams(messages, toolsList)
	if err != nil {
		return nil, err
	}

	params.Model = anthropic.Model(p.model)

	stream := p.client.Messages.NewStreaming(ctx, params)

	ch := make(chan protocol.Event)

	go func() {
		defer close(ch)
		defer stream.Close()

		contentIndex := 0
		toolCallIndex := 0
		currentToolCall := &protocol.ToolCall{}
		inToolUse := false

		for stream.Next() {
			event := stream.Current()

			switch event.Type {
			case "content_block_start":
				if event.ContentBlock.Type == "tool_use" {
					// Start of a tool use block
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
				}

			case "content_block_delta":
				if event.Delta.Type == "text_delta" {
					// Text content delta
					ch <- protocol.Event{
						Type:             protocol.EventTypeContentDelta,
						Index:            contentIndex,
						ContentPartDelta: &protocol.ContentPartDelta{Text: event.Delta.Text},
					}
				} else if event.Delta.Type == "input_json_delta" {
					// Tool input delta
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
					// End of tool use block
					ch <- protocol.Event{
						Type:     protocol.EventTypeToolCallEnd,
						Index:    toolCallIndex,
						ToolCall: currentToolCall,
					}
					toolCallIndex++
					inToolUse = false
				} else {
					// End of text content
					ch <- protocol.Event{
						Type:  protocol.EventTypeContentEnd,
						Index: contentIndex,
					}
					contentIndex++
				}

			case "message_delta":
				// Message is complete
				if event.Delta.StopReason != "" {
					reason := mapStopReason(event.Delta.StopReason)
					finishEvt := protocol.NewFinishEvent(reason)
					finishEvt.Usage = &protocol.Usage{
						InputTokens:              int(event.Usage.InputTokens),
						OutputTokens:             int(event.Usage.OutputTokens),
						CacheCreationInputTokens: int(event.Usage.CacheCreationInputTokens),
						CacheReadInputTokens:     int(event.Usage.CacheReadInputTokens),
					}
					ch <- finishEvt
					return
				}

			case "message_stop":
				// Final stop event
				return
			}
		}

		if err := stream.Err(); err != nil {
			ch <- protocol.NewErrorEvent(err)
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

// Helper to convert interface to JSON string
func toJSONString(v interface{}) string {
	if v == nil {
		return "{}"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
