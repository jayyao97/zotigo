package openai

import (
	"context"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/tools"
	"github.com/openai/openai-go/v3"
)

type ChatProvider struct {
	client *openai.Client
	model  string
}

func (p *ChatProvider) Name() string {
	return "openai-chat"
}

func (p *ChatProvider) StreamChat(ctx context.Context, messages []protocol.Message, toolsList []tools.Tool) (<-chan protocol.Event, error) {
	params, err := convertToChatParams(messages, toolsList)
	if err != nil {
		return nil, err
	}

	params.Model = p.model

	stream := p.client.Chat.Completions.NewStreaming(ctx, params)

	ch := make(chan protocol.Event)

	go func() {
		defer close(ch)
		defer stream.Close()

		acc := openai.ChatCompletionAccumulator{}

		contentStarted := false
		contentIndex := 0
		var finishReason string

		for stream.Next() {
			chunk := stream.Current()
			acc.AddChunk(chunk)

			for _, choice := range chunk.Choices {
				if choice.Index != 0 {
					continue
				}

				delta := choice.Delta

				// 1. Content Delta
				if delta.Content != "" {
					contentStarted = true
					ch <- protocol.Event{
						Type:             protocol.EventTypeContentDelta,
						Index:            contentIndex,
						ContentPartDelta: &protocol.ContentPartDelta{Text: delta.Content},
					}
				}

				// 2. Tool Call Delta
				if len(delta.ToolCalls) > 0 {
					for _, tc := range delta.ToolCalls {
						idx := int(tc.Index)
						ch <- protocol.Event{
							Type:  protocol.EventTypeToolCallDelta,
							Index: idx,
							ToolCallDelta: &protocol.ToolCallDelta{
								ID:        tc.ID,
								Name:      tc.Function.Name,
								Arguments: tc.Function.Arguments,
							},
						}
					}
				}

				// 3. Accumulated Full Objects (End Events)
				// Must be handled BEFORE FinishReason
				if tool, ok := acc.JustFinishedToolCall(); ok {
					idx := int(tool.Index)

					ptc := &protocol.ToolCall{
						Index:     idx,
						ID:        tool.ID,
						Name:      tool.Name,
						Arguments: tool.Arguments,
					}

					ch <- protocol.Event{
						Type:     protocol.EventTypeToolCallEnd,
						Index:    idx,
						ToolCall: ptc,
					}
				}

				if content, ok := acc.JustFinishedContent(); ok {
					if contentStarted {
						ch <- protocol.Event{
							Type:  protocol.EventTypeContentEnd,
							Index: contentIndex,
							ContentPart: &protocol.ContentPart{
								Type: protocol.ContentTypeText,
								Text: content,
							},
						}
						contentStarted = false
					}
				}

				// 4. Finish Reason — save it but don't return yet;
				// the usage chunk (with empty choices) arrives after this.
				if choice.FinishReason != "" {
					if contentStarted {
						ch <- protocol.Event{
							Type:  protocol.EventTypeContentEnd,
							Index: contentIndex,
						}
						contentStarted = false
					}
					finishReason = choice.FinishReason
				}
			}
		}

		if err := stream.Err(); err != nil {
			ch <- protocol.NewErrorEvent(err)
			return
		}

		// Emit finish event after stream ends so acc.Usage is fully populated
		reason := mapFinishReason(finishReason)
		finishEvt := protocol.NewFinishEvent(reason)
		if acc.Usage.TotalTokens > 0 {
			finishEvt.Usage = &protocol.Usage{
				InputTokens:          int(acc.Usage.PromptTokens),
				OutputTokens:         int(acc.Usage.CompletionTokens),
				TotalTokens:          int(acc.Usage.TotalTokens),
				CacheReadInputTokens: int(acc.Usage.PromptTokensDetails.CachedTokens),
			}
		}
		ch <- finishEvt
	}()

	return ch, nil
}

func mapFinishReason(fr string) protocol.FinishReason {
	switch fr {
	case "stop":
		return protocol.FinishReasonStop
	case "length":
		return protocol.FinishReasonLength
	case "tool_calls":
		return protocol.FinishReasonToolCalls
	case "content_filter":
		return protocol.FinishReasonContentFilter
	default:
		return protocol.FinishReasonUnknown
	}
}
