package gemini

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/tools"
	"google.golang.org/genai"
)

// ChatProvider implements providers.Provider for Google Gemini.
type ChatProvider struct {
	client      *genai.Client
	model       string
	temperature *float32
	maxTokens   int32
}

func (p *ChatProvider) Name() string {
	return "gemini-chat"
}

func (p *ChatProvider) StreamChat(ctx context.Context, messages []protocol.Message, toolsList []tools.Tool) (<-chan protocol.Event, error) {
	contents, config, err := convertToGeminiParams(messages, toolsList)
	if err != nil {
		return nil, err
	}

	if p.temperature != nil {
		config.Temperature = p.temperature
	}
	if p.maxTokens > 0 {
		config.MaxOutputTokens = p.maxTokens
	}

	ch := make(chan protocol.Event)

	go func() {
		defer close(ch)

		contentStarted := false
		contentIndex := 0
		toolCallIndex := 0

		for resp, err := range p.client.Models.GenerateContentStream(ctx, p.model, contents, config) {
			if err != nil {
				ch <- protocol.NewErrorEvent(err)
				return
			}

			if resp == nil || len(resp.Candidates) == 0 {
				continue
			}

			candidate := resp.Candidates[0]

			if candidate.Content != nil {
				for _, part := range candidate.Content.Parts {
					// Text content
					if part.Text != "" {
						contentStarted = true
						ch <- protocol.Event{
							Type:             protocol.EventTypeContentDelta,
							Index:            contentIndex,
							ContentPartDelta: &protocol.ContentPartDelta{Text: part.Text},
						}
					}

					// Function call
					if part.FunctionCall != nil {
						fc := part.FunctionCall

						callID := fc.ID
						if callID == "" {
							callID = fmt.Sprintf("gemini_call_%d", toolCallIndex)
						}

						argsJSON, _ := json.Marshal(fc.Args)

						// Emit delta sequence for protocol consistency
						ch <- protocol.Event{
							Type:  protocol.EventTypeToolCallDelta,
							Index: toolCallIndex,
							ToolCallDelta: &protocol.ToolCallDelta{
								Type: protocol.ToolCallDeltaTypeID,
								ID:   callID,
							},
						}
						ch <- protocol.Event{
							Type:  protocol.EventTypeToolCallDelta,
							Index: toolCallIndex,
							ToolCallDelta: &protocol.ToolCallDelta{
								Type: protocol.ToolCallDeltaTypeName,
								Name: fc.Name,
							},
						}
						ch <- protocol.Event{
							Type:  protocol.EventTypeToolCallDelta,
							Index: toolCallIndex,
							ToolCallDelta: &protocol.ToolCallDelta{
								Type:      protocol.ToolCallDeltaTypeArgs,
								Arguments: string(argsJSON),
							},
						}

						// Emit tool call end
						ch <- protocol.Event{
							Type:  protocol.EventTypeToolCallEnd,
							Index: toolCallIndex,
							ToolCall: &protocol.ToolCall{
								Index:     toolCallIndex,
								ID:        callID,
								Name:      fc.Name,
								Arguments: string(argsJSON),
							},
						}
						toolCallIndex++
					}
				}
			}

			// Check finish reason
			if candidate.FinishReason != "" && candidate.FinishReason != genai.FinishReasonUnspecified {
				if contentStarted {
					ch <- protocol.Event{
						Type:  protocol.EventTypeContentEnd,
						Index: contentIndex,
					}
					contentStarted = false
				}

				reason := mapFinishReason(candidate.FinishReason)
				ch <- protocol.NewFinishEvent(reason)
				return
			}
		}

		// If stream ends without explicit finish reason
		if contentStarted {
			ch <- protocol.Event{
				Type:  protocol.EventTypeContentEnd,
				Index: contentIndex,
			}
		}
		ch <- protocol.NewFinishEvent(protocol.FinishReasonStop)
	}()

	return ch, nil
}

func mapFinishReason(fr genai.FinishReason) protocol.FinishReason {
	switch fr {
	case genai.FinishReasonStop:
		return protocol.FinishReasonStop
	case genai.FinishReasonMaxTokens:
		return protocol.FinishReasonLength
	case genai.FinishReasonSafety, genai.FinishReasonRecitation,
		genai.FinishReasonBlocklist, genai.FinishReasonProhibitedContent,
		genai.FinishReasonSPII, genai.FinishReasonImageSafety,
		genai.FinishReasonImageProhibitedContent:
		return protocol.FinishReasonContentFilter
	default:
		return protocol.FinishReasonUnknown
	}
}
