package openai

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"
)

// convertToChatParams converts internal protocol messages to OpenAI ChatCompletionNewParams.
func convertToChatParams(msgs []protocol.Message, toolsList []tools.Tool, reasoningEffort string, toolChoice providers.ToolChoice) (openai.ChatCompletionNewParams, error) {
	var oaMsgs []openai.ChatCompletionMessageParamUnion

	for _, msg := range msgs {
		switch msg.Role {
		case protocol.RoleSystem:
			text := ""
			for _, p := range msg.Content {
				if p.Type == protocol.ContentTypeText {
					text += p.Text
				}
			}
			oaMsgs = append(oaMsgs, openai.SystemMessage(text))

		case protocol.RoleUser:
			var parts []openai.ChatCompletionContentPartUnionParam

			for _, p := range msg.Content {
				switch p.Type {
				case protocol.ContentTypeText:
					parts = append(parts, openai.TextContentPart(p.Text))
				case protocol.ContentTypeImage:
					url := ""
					if p.Image.URL != "" {
						url = p.Image.URL
					} else if len(p.Image.Data) > 0 {
						mime := p.Image.MediaType
						if mime == "" {
							mime = "image/png"
						}
						b64 := base64.StdEncoding.EncodeToString(p.Image.Data)
						url = fmt.Sprintf("data:%s;base64,%s", mime, b64)
					}
					parts = append(parts, openai.ImageContentPart(openai.ChatCompletionContentPartImageImageURLParam{
						URL: url,
					}))
				}
			}
			oaMsgs = append(oaMsgs, openai.UserMessage(parts))

		case protocol.RoleAssistant:
			var toolCalls []openai.ChatCompletionMessageToolCallUnionParam
			text := ""

			for _, p := range msg.Content {
				if p.Type == protocol.ContentTypeText || p.Type == protocol.ContentTypeReasoning {
					text += p.Text
				} else if p.Type == protocol.ContentTypeToolCall && p.ToolCall != nil {
					tcParam := openai.ChatCompletionMessageFunctionToolCallParam{
						ID:   p.ToolCall.ID,
						Type: "function",
						Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
							Name:      p.ToolCall.Name,
							Arguments: p.ToolCall.Arguments,
						},
					}
					toolCalls = append(toolCalls, openai.ChatCompletionMessageToolCallUnionParam{
						OfFunction: &tcParam,
					})
				}
			}

			paramUnion := openai.AssistantMessage(text)
			if len(toolCalls) > 0 {
				if paramUnion.OfAssistant != nil {
					paramUnion.OfAssistant.ToolCalls = toolCalls
				}
			}
			oaMsgs = append(oaMsgs, paramUnion)

		case protocol.RoleTool:
			for _, p := range msg.Content {
				if p.Type == protocol.ContentTypeToolResult && p.ToolResult != nil {
					tr := p.ToolResult
					contentStr := tr.Text
					if tr.JSON != nil {
						b, _ := json.Marshal(tr.JSON)
						contentStr = string(b)
					} else if tr.Type == protocol.ToolResultTypeExecutionDenied {
						contentStr = fmt.Sprintf("User denied execution: %s", tr.Reason)
					} else if tr.Type == protocol.ToolResultTypeContent {
						for _, c := range tr.Content {
							if c.Type == protocol.ContentTypeText {
								contentStr += c.Text
							}
						}
					}

					oaMsgs = append(oaMsgs, openai.ToolMessage(contentStr, tr.ToolCallID))
				}
			}
		}
	}

	params := openai.ChatCompletionNewParams{
		Messages: oaMsgs,
		StreamOptions: openai.ChatCompletionStreamOptionsParam{
			IncludeUsage: openai.Bool(true),
		},
	}

	// Set reasoning effort if provided
	switch reasoningEffort {
	case "low":
		params.ReasoningEffort = shared.ReasoningEffortLow
	case "medium":
		params.ReasoningEffort = shared.ReasoningEffortMedium
	case "high":
		params.ReasoningEffort = shared.ReasoningEffortHigh
	}

	// Convert Tools
	if len(toolsList) > 0 {
		var oaTools []openai.ChatCompletionToolUnionParam
		for _, t := range toolsList {
			schema := t.Schema()

			schemaMap, ok := schema.(map[string]any)
			if !ok {
				b, _ := json.Marshal(schema)
				_ = json.Unmarshal(b, &schemaMap)
			}

			// Raw assignments, no openai.String/F
			oaTools = append(oaTools, openai.ChatCompletionToolUnionParam{
				OfFunction: &openai.ChatCompletionFunctionToolParam{
					Type: "function",
					Function: shared.FunctionDefinitionParam{
						Name:        t.Name(),
						Description: openai.String(t.Description()),
						Parameters:  shared.FunctionParameters(schemaMap),
					},
				},
			})
		}
		params.Tools = oaTools
	}

	// Translate tool choice.
	switch toolChoice.Mode {
	case providers.ToolChoiceRequired:
		params.ToolChoice = openai.ChatCompletionToolChoiceOptionUnionParam{
			OfAuto: openai.String("required"),
		}
	case providers.ToolChoiceSpecific:
		params.ToolChoice = openai.ToolChoiceOptionFunctionToolChoice(
			openai.ChatCompletionNamedToolChoiceFunctionParam{Name: toolChoice.Name},
		)
	}

	return params, nil
}
