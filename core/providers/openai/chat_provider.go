package openai

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jayyao97/zotigo/core/debug"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/respjson"
)

type ChatProvider struct {
	client          *openai.Client
	model           string
	reasoningEffort string // "", "low", "medium", "high" — default for calls
}

func (p *ChatProvider) Name() string {
	return "openai-chat"
}

func (p *ChatProvider) StreamChat(ctx context.Context, messages []protocol.Message, toolsList []tools.Tool, opts ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	resolved := providers.ResolveOptions(opts)
	effort := resolved.ReasoningEffort
	if effort == "" {
		effort = p.reasoningEffort
	}

	params, err := convertToChatParams(messages, toolsList, effort, resolved.ToolChoice)
	if err != nil {
		return nil, err
	}

	params.Model = p.model
	start := time.Now()
	debug.Logf("openai stream start model=%s messages=%d tools=%d reasoning=%s", p.model, len(messages), len(toolsList), effort)

	stream := p.client.Chat.Completions.NewStreaming(ctx, params)

	ch := make(chan protocol.Event)

	go func() {
		defer close(ch)
		defer func() { _ = stream.Close() }()

		acc := openai.ChatCompletionAccumulator{}

		contentStarted := false
		contentIndex := 0
		var finishReason string
		var lastUsage *openai.CompletionUsage
		var firstChunkAt time.Time

		for stream.Next() {
			if firstChunkAt.IsZero() {
				firstChunkAt = time.Now()
				debug.Logf("openai stream first_chunk model=%s ttft=%s", p.model, firstChunkAt.Sub(start).Round(time.Millisecond))
			}
			chunk := stream.Current()
			acc.AddChunk(chunk)

			// The final chunk (with empty choices) carries full usage including
			// PromptTokensDetails. The SDK accumulator doesn't accumulate nested
			// detail fields, so we capture the raw usage from the last chunk.
			if chunk.Usage.TotalTokens > 0 {
				u := chunk.Usage
				lastUsage = &u
			}

			for _, choice := range chunk.Choices {
				if choice.Index != 0 {
					continue
				}

				delta := choice.Delta

				// 1. Reasoning Delta (non-standard OpenAI fields).
				// DeepSeek, llama.cpp (--reasoning-format deepseek), OpenRouter,
				// and gpt-oss all stream thinking content under custom keys that
				// the official SDK drops into ExtraFields. Surface them as
				// reasoning content so the TUI can render a Thinking block.
				if text := extractReasoningDelta(delta.JSON.ExtraFields); text != "" {
					ch <- protocol.NewReasoningDeltaEvent(text)
				}

				// 2. Content Delta
				if delta.Content != "" {
					contentStarted = true
					ch <- protocol.Event{
						Type:             protocol.EventTypeContentDelta,
						Index:            contentIndex,
						ContentPartDelta: &protocol.ContentPartDelta{Text: delta.Content},
					}
				}

				// 3. Tool Call Delta
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

				// 4. Accumulated Full Objects (End Events)
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

				// 5. Finish Reason — save it but don't return yet;
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
			debug.Logf("openai stream error model=%s duration=%s err=%v", p.model, time.Since(start).Round(time.Millisecond), err)
			ch <- protocol.NewErrorEvent(providers.WrapIfContextLength(err))
			return
		}

		// Emit finish event after stream ends with usage from the final chunk
		reason := mapFinishReason(finishReason)
		finishEvt := protocol.NewFinishEvent(reason)
		if lastUsage != nil {
			// OpenAI's prompt_tokens already includes cached tokens; subtract
			// them so InputTokens means "new uncached input" — see
			// protocol.Usage doc for the cross-provider convention.
			cached := int(lastUsage.PromptTokensDetails.CachedTokens)
			input := max(int(lastUsage.PromptTokens)-cached, 0)
			finishEvt.Usage = &protocol.Usage{
				InputTokens:          input,
				OutputTokens:         int(lastUsage.CompletionTokens),
				TotalTokens:          int(lastUsage.TotalTokens),
				CacheReadInputTokens: cached,
			}
		}
		debug.Logf(
			"openai stream end model=%s duration=%s finish=%s usage_in=%d usage_out=%d usage_total=%d",
			p.model,
			time.Since(start).Round(time.Millisecond),
			reason,
			func() int {
				if lastUsage == nil {
					return 0
				}
				return int(lastUsage.PromptTokens)
			}(),
			func() int {
				if lastUsage == nil {
					return 0
				}
				return int(lastUsage.CompletionTokens)
			}(),
			func() int {
				if lastUsage == nil {
					return 0
				}
				return int(lastUsage.TotalTokens)
			}(),
		)
		ch <- finishEvt
	}()

	return ch, nil
}

// reasoningDeltaKeys enumerates the non-standard JSON keys different
// OpenAI-compatible servers use to stream thinking content alongside
// the regular `content` field. We probe each in order and return the
// first non-empty string found on a given chunk.
//
//   - reasoning_content: DeepSeek, llama.cpp (--reasoning-format deepseek),
//     Qwen3-thinking via llama.cpp, gpt-oss
//   - reasoning:         OpenRouter passthrough, some LM Studio builds
//   - thinking:          occasional older llama.cpp builds
var reasoningDeltaKeys = []string{"reasoning_content", "reasoning", "thinking"}

// extractReasoningDelta pulls a thinking-content fragment out of the
// SDK's ExtraFields map for a single streaming chunk. Returns "" when
// no recognized key carries a non-empty string.
func extractReasoningDelta(extra map[string]respjson.Field) string {
	if len(extra) == 0 {
		return ""
	}
	for _, key := range reasoningDeltaKeys {
		f, ok := extra[key]
		if !ok || !f.Valid() {
			continue
		}
		var s string
		if err := json.Unmarshal([]byte(f.Raw()), &s); err != nil {
			continue
		}
		if s != "" {
			return s
		}
	}
	return ""
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
