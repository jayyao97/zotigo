package anthropic

import (
	"encoding/base64"
	"encoding/json"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/tools"
)

// convertToAnthropicParams converts internal protocol messages to Anthropic MessageNewParams.
func convertToAnthropicParams(msgs []protocol.Message, toolsList []tools.Tool) (anthropic.MessageNewParams, error) {
	var anthropicMsgs []anthropic.MessageParam
	var systemPrompt string

	for _, msg := range msgs {
		switch msg.Role {
		case protocol.RoleSystem:
			// Anthropic uses a separate system parameter
			for _, p := range msg.Content {
				if p.Type == protocol.ContentTypeText {
					systemPrompt += p.Text
				}
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
				case protocol.ContentTypeToolCall:
					if p.ToolCall != nil {
						tc := p.ToolCall
						// Parse arguments JSON to map
						var inputMap map[string]interface{}
						json.Unmarshal([]byte(tc.Arguments), &inputMap)

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

	params := anthropic.MessageNewParams{
		Messages:  anthropicMsgs,
		MaxTokens: 4096,
	}

	// Set system prompt if present
	if systemPrompt != "" {
		params.System = []anthropic.TextBlockParam{
			{Text: systemPrompt},
		}
	}

	// Convert Tools
	if len(toolsList) > 0 {
		var anthropicTools []anthropic.ToolUnionParam
		for _, t := range toolsList {
			schema := t.Schema()

			schemaMap, ok := schema.(map[string]any)
			if !ok {
				b, _ := json.Marshal(schema)
				json.Unmarshal(b, &schemaMap)
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
		params.Tools = anthropicTools
	}

	return params, nil
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
