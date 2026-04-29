package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jayyao97/zotigo/core/debug"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

// ResponseProvider talks to OpenAI's Responses API (/v1/responses).
//
// Unlike Chat Completions, the Responses API is the official path for
// gpt-5 / o-series reasoning models: it exposes a dedicated
// response.reasoning_text.* event stream, lets the server own
// conversation state (previous_response_id), and handles tool calls as
// typed items rather than a string-args delta slurp.
//
// We translate the SSE event union into the same protocol.Event shape
// the rest of the agent already speaks — reasoning deltas become
// ContentTypeReasoning events, text deltas become normal content, and
// function-call-arguments delta/done feed our ToolCall stream.
type ResponseProvider struct {
	client          *openai.Client
	model           string
	reasoningEffort string // "", "low", "medium", "high" — default for calls
}

func (p *ResponseProvider) Name() string {
	return "openai-response"
}

func (p *ResponseProvider) StreamChat(ctx context.Context, messages []protocol.Message, toolsList []tools.Tool, opts ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	resolved := providers.ResolveOptions(opts)
	effort := resolved.ReasoningEffort
	if effort == "" {
		effort = p.reasoningEffort
	}

	params, err := buildResponseParams(p.model, messages, toolsList, effort, resolved.ToolChoice)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	debug.Logf("openai-response stream start model=%s messages=%d tools=%d reasoning=%s",
		p.model, len(messages), len(toolsList), effort)

	stream := p.client.Responses.NewStreaming(ctx, params)

	ch := make(chan protocol.Event)
	go func() {
		defer close(ch)
		defer func() { _ = stream.Close() }()

		// Track per-output-index state for content/reasoning streaming so we
		// can emit matching ContentEnd events when a block finishes.
		type blockKind int
		const (
			kindNone blockKind = iota
			kindText
			kindReasoning
		)
		blockType := map[int64]blockKind{}
		contentBuf := map[int64]*strings.Builder{}

		// Function-call state keyed by item_id.
		type pendingCall struct {
			id        string
			name      string
			arguments strings.Builder
			callID    string
		}
		pending := map[string]*pendingCall{}

		finishReason := protocol.FinishReasonUnknown
		var firstChunkAt time.Time
		var finalUsage *responses.ResponseUsage

		for stream.Next() {
			if firstChunkAt.IsZero() {
				firstChunkAt = time.Now()
				debug.Logf("openai-response stream first_chunk model=%s ttft=%s",
					p.model, firstChunkAt.Sub(start).Round(time.Millisecond))
			}
			evt := stream.Current()

			switch evt.Type {

			// --- Text content ---

			case "response.output_text.delta":
				if evt.Delta == "" {
					continue
				}
				blockType[evt.OutputIndex] = kindText
				if contentBuf[evt.OutputIndex] == nil {
					contentBuf[evt.OutputIndex] = &strings.Builder{}
				}
				contentBuf[evt.OutputIndex].WriteString(evt.Delta)
				ch <- protocol.Event{
					Type:             protocol.EventTypeContentDelta,
					Index:            int(evt.OutputIndex),
					ContentPartDelta: &protocol.ContentPartDelta{Text: evt.Delta},
				}

			case "response.output_text.done":
				if blockType[evt.OutputIndex] == kindText {
					full := ""
					if b := contentBuf[evt.OutputIndex]; b != nil {
						full = b.String()
					}
					ch <- protocol.Event{
						Type:  protocol.EventTypeContentEnd,
						Index: int(evt.OutputIndex),
						ContentPart: &protocol.ContentPart{
							Type: protocol.ContentTypeText,
							Text: full,
						},
					}
					delete(contentBuf, evt.OutputIndex)
					delete(blockType, evt.OutputIndex)
				}

			// --- Reasoning content ---
			//
			// Two sibling event families carry thinking: reasoning_text is
			// the raw chain of thought, reasoning_summary_text is the
			// higher-level summary. We surface both as ContentTypeReasoning
			// so the TUI renders a Thinking block either way.
			//
			// ContentEnd is intentionally deferred until
			// response.output_item.done — that's when the SDK hands us
			// the reasoning item's ID + encrypted_content, which we must
			// preserve so the next turn can include them as input for
			// stateless chain-of-thought continuity.

			case "response.reasoning_text.delta",
				"response.reasoning_summary_text.delta":
				if evt.Delta == "" {
					continue
				}
				blockType[evt.OutputIndex] = kindReasoning
				if contentBuf[evt.OutputIndex] == nil {
					contentBuf[evt.OutputIndex] = &strings.Builder{}
				}
				contentBuf[evt.OutputIndex].WriteString(evt.Delta)
				ch <- protocol.NewReasoningDeltaEvent(evt.Delta)

			case "response.reasoning_text.done",
				"response.reasoning_summary_text.done":
				// No-op: we'll emit ContentEnd once output_item.done fires
				// with the full reasoning item (including encrypted_content).

			// --- Function / tool calls ---
			//
			// The Responses API emits tool call metadata (name + call_id) via
			// response.output_item.added with item.type == "function_call",
			// then streams the JSON arguments as
			// response.function_call_arguments.delta, and signals completion
			// via .done. We mirror that into our protocol as
			// ToolCallDelta (per fragment) + ToolCallEnd (with the full
			// assembled call).

			case "response.output_item.added":
				if fn := evt.Item.AsFunctionCall(); fn.ID != "" || fn.CallID != "" {
					pc := &pendingCall{
						id:     fn.ID,
						name:   fn.Name,
						callID: fn.CallID,
					}
					pending[fn.ID] = pc
					ch <- protocol.Event{
						Type:  protocol.EventTypeToolCallDelta,
						Index: int(evt.OutputIndex),
						ToolCallDelta: &protocol.ToolCallDelta{
							ID:   pc.callID,
							Name: pc.name,
						},
					}
				}

			case "response.function_call_arguments.delta":
				pc := pending[evt.ItemID]
				if pc == nil {
					// Missed the added event — fall back to an empty record.
					pc = &pendingCall{id: evt.ItemID}
					pending[evt.ItemID] = pc
				}
				pc.arguments.WriteString(evt.Delta)
				ch <- protocol.Event{
					Type:  protocol.EventTypeToolCallDelta,
					Index: int(evt.OutputIndex),
					ToolCallDelta: &protocol.ToolCallDelta{
						Arguments: evt.Delta,
					},
				}

			case "response.function_call_arguments.done":
				pc := pending[evt.ItemID]
				if pc == nil {
					continue
				}
				args := pc.arguments.String()
				if args == "" && evt.Arguments != "" {
					args = evt.Arguments
				}
				ch <- protocol.Event{
					Type:  protocol.EventTypeToolCallEnd,
					Index: int(evt.OutputIndex),
					ToolCall: &protocol.ToolCall{
						Index:     int(evt.OutputIndex),
						ID:        pc.callID,
						Name:      pc.name,
						Arguments: args,
					},
				}
				delete(pending, evt.ItemID)

			// response.output_item.done fires for every top-level output
			// item at completion. For reasoning items it carries the ID
			// + encrypted_content we need to preserve for stateless
			// chain-of-thought continuity — emit ContentEnd here (with
			// blockType check to avoid firing on indexes we never saw
			// reasoning on).
			case "response.output_item.done":
				rs := evt.Item.AsReasoning()
				if rs.ID == "" && rs.EncryptedContent == "" {
					continue
				}
				full := ""
				if b := contentBuf[evt.OutputIndex]; b != nil {
					full = b.String()
				}
				ch <- protocol.Event{
					Type:  protocol.EventTypeContentEnd,
					Index: int(evt.OutputIndex),
					ContentPart: &protocol.ContentPart{
						Type:             protocol.ContentTypeReasoning,
						Text:             full,
						ReasoningID:      rs.ID,
						EncryptedContent: rs.EncryptedContent,
					},
				}
				delete(contentBuf, evt.OutputIndex)
				delete(blockType, evt.OutputIndex)

			// --- Terminal events ---

			case "response.completed":
				finishReason = mapResponseCompletedReason(&evt.Response)
				if evt.Response.Usage.TotalTokens > 0 {
					u := evt.Response.Usage
					finalUsage = &u
				}

			case "response.incomplete":
				finishReason = protocol.FinishReasonLength

			case "response.failed", "error":
				msg := evt.Message
				if msg == "" {
					msg = "response API failed"
				}
				debug.Logf("openai-response stream error model=%s type=%s msg=%s",
					p.model, evt.Type, msg)
				ch <- protocol.NewErrorEvent(&responseStreamError{Msg: msg, Code: evt.Code})
				return
			}
		}

		if err := stream.Err(); err != nil {
			debug.Logf("openai-response stream err model=%s duration=%s err=%v",
				p.model, time.Since(start).Round(time.Millisecond), err)
			ch <- protocol.NewErrorEvent(err)
			return
		}

		// Flush any in-flight content blocks we never saw a matching .done for.
		for idx, kind := range blockType {
			buf := contentBuf[idx]
			if buf == nil {
				continue
			}
			ct := protocol.ContentTypeText
			if kind == kindReasoning {
				ct = protocol.ContentTypeReasoning
			}
			ch <- protocol.Event{
				Type:  protocol.EventTypeContentEnd,
				Index: int(idx),
				ContentPart: &protocol.ContentPart{
					Type: ct,
					Text: buf.String(),
				},
			}
		}

		finishEvt := protocol.NewFinishEvent(finishReason)
		if finalUsage != nil {
			// Responses API mirrors Chat Completions: input_tokens already
			// includes cached input. Subtract so InputTokens stays the
			// uncached count consistent with protocol.Usage.
			cached := int(finalUsage.InputTokensDetails.CachedTokens)
			input := max(int(finalUsage.InputTokens)-cached, 0)
			finishEvt.Usage = &protocol.Usage{
				InputTokens:          input,
				OutputTokens:         int(finalUsage.OutputTokens),
				TotalTokens:          int(finalUsage.TotalTokens),
				CacheReadInputTokens: cached,
			}
		}
		debug.Logf("openai-response stream end model=%s duration=%s finish=%s",
			p.model, time.Since(start).Round(time.Millisecond), finishReason)
		ch <- finishEvt
	}()

	return ch, nil
}

// mapResponseCompletedReason inspects the terminal Response to classify
// how generation ended. The Responses API reports an explicit
// incomplete_details.reason for truncation / tool-use cases, falling back
// to "stop" for clean completions.
func mapResponseCompletedReason(resp *responses.Response) protocol.FinishReason {
	if resp == nil {
		return protocol.FinishReasonStop
	}
	if resp.IncompleteDetails.Reason != "" {
		switch resp.IncompleteDetails.Reason {
		case "max_output_tokens":
			return protocol.FinishReasonLength
		case "content_filter":
			return protocol.FinishReasonContentFilter
		}
	}
	// When the model emitted function calls we route through ToolCalls so the
	// agent knows to dispatch them rather than treat the turn as finished.
	for _, out := range resp.Output {
		if fn := out.AsFunctionCall(); fn.CallID != "" {
			return protocol.FinishReasonToolCalls
		}
	}
	return protocol.FinishReasonStop
}

// buildResponseParams converts our protocol messages + tool list into
// the Responses API's input-item + tool shape. Messages become
// EasyInputMessage; existing tool calls in history become function_call
// items; tool results become function_call_output items.
func buildResponseParams(model string, msgs []protocol.Message, toolsList []tools.Tool, effort string, toolChoice providers.ToolChoice) (responses.ResponseNewParams, error) {
	msgs = providers.MergeConsecutiveUserMessages(msgs)

	var instructions strings.Builder
	var items responses.ResponseInputParam

	for _, m := range msgs {
		switch m.Role {
		case protocol.RoleSystem:
			// System prompts go in the top-level instructions field so the
			// server can cache them across turns.
			for _, part := range m.Content {
				if part.Type == protocol.ContentTypeText {
					if instructions.Len() > 0 {
						instructions.WriteString("\n\n")
					}
					instructions.WriteString(part.Text)
				}
			}

		case protocol.RoleUser:
			content, err := buildEasyInputContent(m.Content)
			if err != nil {
				return responses.ResponseNewParams{}, err
			}
			items = append(items, responses.ResponseInputItemUnionParam{
				OfMessage: &responses.EasyInputMessageParam{
					Content: content,
					Role:    responses.EasyInputMessageRoleUser,
				},
			})

		case protocol.RoleAssistant:
			// Split the assistant turn into its reasoning, text, and
			// tool-call parts. Reasoning items carry the encrypted
			// chain-of-thought blob — they must go back to the server
			// in the same relative order they were emitted, otherwise
			// the model sees a jumbled history.
			var text strings.Builder
			for _, part := range m.Content {
				switch part.Type {
				case protocol.ContentTypeReasoning:
					if part.ReasoningID == "" || part.EncryptedContent == "" {
						// No server-issued blob to preserve — drop.
						// (Most likely this reasoning came through a
						// Chat Completions path, or the request didn't
						// ask for encrypted content.)
						continue
					}
					items = append(items, responses.ResponseInputItemUnionParam{
						OfReasoning: &responses.ResponseReasoningItemParam{
							ID:               part.ReasoningID,
							EncryptedContent: param.NewOpt(part.EncryptedContent),
							Summary:          []responses.ResponseReasoningItemSummaryParam{},
						},
					})
				case protocol.ContentTypeText:
					text.WriteString(part.Text)
				case protocol.ContentTypeToolCall:
					if part.ToolCall == nil {
						continue
					}
					items = append(items, responses.ResponseInputItemUnionParam{
						OfFunctionCall: &responses.ResponseFunctionToolCallParam{
							CallID:    part.ToolCall.ID,
							Name:      part.ToolCall.Name,
							Arguments: part.ToolCall.Arguments,
						},
					})
				}
			}
			if text.Len() > 0 {
				items = append(items, responses.ResponseInputItemParamOfMessage(text.String(), responses.EasyInputMessageRoleAssistant))
			}

		case protocol.RoleTool:
			for _, part := range m.Content {
				if part.ToolResult == nil {
					continue
				}
				items = append(items, responses.ResponseInputItemUnionParam{
					OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
						CallID: part.ToolResult.ToolCallID,
						Output: responses.ResponseInputItemFunctionCallOutputOutputUnionParam{
							OfString: param.NewOpt(part.ToolResult.Text),
						},
					},
				})
			}
		}
	}

	params := responses.ResponseNewParams{
		Model: shared.ResponsesModel(model),
		Input: responses.ResponseNewParamsInputUnion{OfInputItemList: items},
		// Ask the server to emit encrypted reasoning blobs so we can
		// pass them back on the next turn. This is what makes stateless
		// multi-turn reasoning work without previous_response_id.
		Include: []responses.ResponseIncludable{
			responses.ResponseIncludableReasoningEncryptedContent,
		},
	}
	if instructions.Len() > 0 {
		params.Instructions = param.NewOpt(instructions.String())
	}

	// Map reasoning effort.
	switch effort {
	case "low":
		params.Reasoning = shared.ReasoningParam{Effort: shared.ReasoningEffortLow}
	case "medium":
		params.Reasoning = shared.ReasoningParam{Effort: shared.ReasoningEffortMedium}
	case "high":
		params.Reasoning = shared.ReasoningParam{Effort: shared.ReasoningEffortHigh}
	}

	// Convert tools.
	if len(toolsList) > 0 {
		var oaTools []responses.ToolUnionParam
		for _, t := range toolsList {
			schema := t.Schema()
			schemaMap, ok := schema.(map[string]any)
			if !ok {
				b, _ := json.Marshal(schema)
				_ = json.Unmarshal(b, &schemaMap)
			}
			oaTools = append(oaTools, responses.ToolUnionParam{
				OfFunction: &responses.FunctionToolParam{
					Name:        t.Name(),
					Description: param.NewOpt(t.Description()),
					Parameters:  schemaMap,
					Strict:      param.NewOpt(false),
				},
			})
		}
		params.Tools = oaTools
	}

	// Tool choice mapping.
	switch toolChoice.Mode {
	case providers.ToolChoiceRequired:
		params.ToolChoice = responses.ResponseNewParamsToolChoiceUnion{
			OfToolChoiceMode: param.NewOpt(responses.ToolChoiceOptionsRequired),
		}
	case providers.ToolChoiceSpecific:
		params.ToolChoice = responses.ResponseNewParamsToolChoiceUnion{
			OfFunctionTool: &responses.ToolChoiceFunctionParam{
				Name: toolChoice.Name,
			},
		}
	}

	return params, nil
}

type responseStreamError struct {
	Msg  string
	Code string
}

func (e *responseStreamError) Error() string {
	if e.Code != "" {
		return e.Msg + " (code=" + e.Code + ")"
	}
	return e.Msg
}

// buildEasyInputContent converts a protocol user message's parts into
// the Responses API's rich content shape. Pure-text messages collapse
// to a string; anything with an image promotes to a content list with
// input_text + input_image items. The image URL follows the same
// data URI convention used by Chat Completions (base64 inline or a
// plain URL passthrough).
func buildEasyInputContent(parts []protocol.ContentPart) (responses.EasyInputMessageContentUnionParam, error) {
	// Fast path: pure text → single string.
	hasNonText := false
	for _, p := range parts {
		if p.Type != protocol.ContentTypeText {
			hasNonText = true
			break
		}
	}
	if !hasNonText {
		var sb strings.Builder
		for _, p := range parts {
			if p.Type != protocol.ContentTypeText {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(p.Text)
		}
		return responses.EasyInputMessageContentUnionParam{
			OfString: param.NewOpt(sb.String()),
		}, nil
	}

	// Rich path: build a content list.
	var list responses.ResponseInputMessageContentListParam
	for _, p := range parts {
		switch p.Type {
		case protocol.ContentTypeText:
			list = append(list, responses.ResponseInputContentUnionParam{
				OfInputText: &responses.ResponseInputTextParam{Text: p.Text},
			})
		case protocol.ContentTypeImage:
			img, err := newInputImageParam(p.Image)
			if err != nil {
				return responses.EasyInputMessageContentUnionParam{}, err
			}
			list = append(list, responses.ResponseInputContentUnionParam{
				OfInputImage: img,
			})
		}
	}
	return responses.EasyInputMessageContentUnionParam{
		OfInputItemContentList: list,
	}, nil
}

// newInputImageParam builds a Responses API input_image from a
// protocol MediaPart. Preference order: explicit FileID (upload-first
// workflow) → URL passthrough → inline base64 data URI.
func newInputImageParam(media *protocol.MediaPart) (*responses.ResponseInputImageParam, error) {
	if media == nil {
		return nil, fmt.Errorf("image part is nil")
	}
	img := &responses.ResponseInputImageParam{
		Detail: responses.ResponseInputImageDetailAuto,
	}
	switch {
	case media.FileID != "":
		img.FileID = param.NewOpt(media.FileID)
	case media.URL != "":
		img.ImageURL = param.NewOpt(media.URL)
	case len(media.Data) > 0:
		mime := media.MediaType
		if mime == "" {
			mime = "image/png"
		}
		b64 := base64.StdEncoding.EncodeToString(media.Data)
		img.ImageURL = param.NewOpt(fmt.Sprintf("data:%s;base64,%s", mime, b64))
	default:
		return nil, fmt.Errorf("image part has no data, url, or file_id")
	}
	return img, nil
}
