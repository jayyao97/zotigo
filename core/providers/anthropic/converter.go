package anthropic

import (
	"encoding/base64"
	"encoding/json"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
)

// convertToAnthropicParams converts internal protocol messages to Anthropic MessageNewParams.
func convertToAnthropicParams(msgs []protocol.Message, toolsList []tools.Tool, toolChoice providers.ToolChoice) (anthropic.MessageNewParams, error) {
	msgs = providers.MergeConsecutiveUserMessages(msgs)

	var anthropicMsgs []anthropic.MessageParam
	var systemTexts []string

	for _, msg := range msgs {
		switch msg.Role {
		case protocol.RoleSystem:
			// Collect each system message as a separate text (preserves multi-block structure)
			var text string
			for _, p := range msg.Content {
				if p.Type == protocol.ContentTypeText {
					text += p.Text
				}
			}
			if text != "" {
				systemTexts = append(systemTexts, text)
			}

		case protocol.RoleUser:
			var parts []anthropic.ContentBlockParamUnion
			for _, p := range msg.Content {
				switch p.Type {
				case protocol.ContentTypeText:
					parts = append(parts, anthropic.NewTextBlock(p.Text))
				case protocol.ContentTypeImage:
					if len(p.Image.Data) > 0 {
						mime := p.Image.MediaType
						if mime == "" {
							mime = "image/png"
						}
						b64 := base64.StdEncoding.EncodeToString(p.Image.Data)
						parts = append(parts, anthropic.NewImageBlockBase64(mime, b64))
					}
				case protocol.ContentTypeToolResult:
					if p.ToolResult != nil {
						parts = append(parts, convertToolResult(p.ToolResult))
					}
				}
			}
			if len(parts) > 0 {
				anthropicMsgs = append(anthropicMsgs, anthropic.NewUserMessage(parts...))
			}

		case protocol.RoleAssistant:
			var parts []anthropic.ContentBlockParamUnion
			for _, p := range msg.Content {
				switch p.Type {
				case protocol.ContentTypeText:
					if p.Text != "" {
						parts = append(parts, anthropic.NewTextBlock(p.Text))
					}
				case protocol.ContentTypeReasoning:
					if p.Text != "" {
						parts = append(parts, anthropic.NewThinkingBlock(p.Signature, p.Text))
					}
				case protocol.ContentTypeToolCall:
					if p.ToolCall != nil {
						tc := p.ToolCall
						// Best-effort parse: a malformed argument string falls
						// back to nil and the SDK rejects it downstream.
						var inputMap map[string]interface{}
						_ = json.Unmarshal([]byte(tc.Arguments), &inputMap)

						parts = append(parts, anthropic.ContentBlockParamUnion{
							OfToolUse: &anthropic.ToolUseBlockParam{
								ID:    tc.ID,
								Name:  tc.Name,
								Input: inputMap,
							},
						})
					}
				}
			}
			if len(parts) > 0 {
				anthropicMsgs = append(anthropicMsgs, anthropic.NewAssistantMessage(parts...))
			}

		case protocol.RoleTool:
			// Tool results in Anthropic are embedded in user messages
			var parts []anthropic.ContentBlockParamUnion
			for _, p := range msg.Content {
				if p.Type == protocol.ContentTypeToolResult && p.ToolResult != nil {
					parts = append(parts, convertToolResult(p.ToolResult))
				}
			}
			if len(parts) > 0 {
				anthropicMsgs = append(anthropicMsgs, anthropic.NewUserMessage(parts...))
			}
		}
	}
	markLastMessageBlockCacheable(anthropicMsgs)

	params := anthropic.MessageNewParams{
		Messages:  anthropicMsgs,
		MaxTokens: 4096,
	}

	// Set system prompt blocks — one per system message, cache_control on the first (static) block
	if len(systemTexts) > 0 {
		var blocks []anthropic.TextBlockParam
		for i, text := range systemTexts {
			block := anthropic.TextBlockParam{Text: text}
			if i == 0 {
				block.CacheControl = anthropic.NewCacheControlEphemeralParam()
			}
			blocks = append(blocks, block)
		}
		params.System = blocks
	}

	// Convert Tools
	if len(toolsList) > 0 {
		var anthropicTools []anthropic.ToolUnionParam
		for _, t := range toolsList {
			schema := t.Schema()

			schemaMap, ok := schema.(map[string]any)
			if !ok {
				b, _ := json.Marshal(schema)
				_ = json.Unmarshal(b, &schemaMap)
			}

			// Extract properties from schema
			properties, _ := schemaMap["properties"].(map[string]any)
			required, _ := schemaMap["required"].([]any)

			var requiredStrings []string
			for _, r := range required {
				if s, ok := r.(string); ok {
					requiredStrings = append(requiredStrings, s)
				}
			}

			anthropicTools = append(anthropicTools, anthropic.ToolUnionParam{
				OfTool: &anthropic.ToolParam{
					Name:        t.Name(),
					Description: anthropic.String(t.Description()),
					InputSchema: anthropic.ToolInputSchemaParam{
						Properties: properties,
						Required:   requiredStrings,
					},
				},
			})
		}
		// Cache tool definitions — they don't change within a session
		if len(anthropicTools) > 0 {
			last := anthropicTools[len(anthropicTools)-1].OfTool
			if last != nil {
				last.CacheControl = anthropic.NewCacheControlEphemeralParam()
			}
		}
		params.Tools = anthropicTools
	}

	// Translate tool choice.
	switch toolChoice.Mode {
	case providers.ToolChoiceRequired:
		params.ToolChoice = anthropic.ToolChoiceUnionParam{
			OfAny: &anthropic.ToolChoiceAnyParam{},
		}
	case providers.ToolChoiceSpecific:
		params.ToolChoice = anthropic.ToolChoiceUnionParam{
			OfTool: &anthropic.ToolChoiceToolParam{Name: toolChoice.Name},
		}
	}

	return params, nil
}

func markLastMessageBlockCacheable(msgs []anthropic.MessageParam) {
	if len(msgs) == 0 {
		return
	}
	parts := msgs[len(msgs)-1].Content
	for i := len(parts) - 1; i >= 0; i-- {
		switch {
		case parts[i].OfText != nil:
			parts[i].OfText.CacheControl = anthropic.NewCacheControlEphemeralParam()
			return
		case parts[i].OfToolUse != nil:
			parts[i].OfToolUse.CacheControl = anthropic.NewCacheControlEphemeralParam()
			return
		case parts[i].OfToolResult != nil:
			parts[i].OfToolResult.CacheControl = anthropic.NewCacheControlEphemeralParam()
			return
		}
	}
}

// convertToolResult converts a protocol ToolResult to an Anthropic ContentBlockParamUnion
func convertToolResult(tr *protocol.ToolResult) anthropic.ContentBlockParamUnion {
	contentStr := tr.Text
	if tr.JSON != nil {
		b, _ := json.Marshal(tr.JSON)
		contentStr = string(b)
	} else if tr.Type == protocol.ToolResultTypeExecutionDenied {
		contentStr = "User denied execution: " + tr.Reason
	}

	return anthropic.ContentBlockParamUnion{
		OfToolResult: &anthropic.ToolResultBlockParam{
			ToolUseID: tr.ToolCallID,
			Content: []anthropic.ToolResultBlockParamContentUnion{
				{
					OfText: &anthropic.TextBlockParam{
						Text: contentStr,
					},
				},
			},
		},
	}
}
