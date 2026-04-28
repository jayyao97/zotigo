package gemini

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
	"google.golang.org/genai"
)

// convertToGeminiParams converts internal protocol messages and tools to Gemini SDK types.
func convertToGeminiParams(msgs []protocol.Message, toolsList []tools.Tool, toolChoice providers.ToolChoice) ([]*genai.Content, *genai.GenerateContentConfig, error) {
	var contents []*genai.Content
	var systemText string

	config := &genai.GenerateContentConfig{}

	for _, msg := range msgs {
		switch msg.Role {
		case protocol.RoleSystem:
			for _, p := range msg.Content {
				if p.Type == protocol.ContentTypeText {
					systemText += p.Text
				}
			}

		case protocol.RoleUser:
			var parts []*genai.Part
			for _, p := range msg.Content {
				switch p.Type {
				case protocol.ContentTypeText:
					parts = append(parts, genai.NewPartFromText(p.Text))
				case protocol.ContentTypeImage:
					if len(p.Image.Data) > 0 {
						mime := p.Image.MediaType
						if mime == "" {
							mime = "image/png"
						}
						parts = append(parts, genai.NewPartFromBytes(p.Image.Data, mime))
					} else if p.Image.URL != "" {
						mime := p.Image.MediaType
						if mime == "" {
							mime = "image/png"
						}
						parts = append(parts, genai.NewPartFromURI(p.Image.URL, mime))
					}
				}
			}
			if len(parts) > 0 {
				contents = append(contents, genai.NewContentFromParts(parts, genai.RoleUser))
			}

		case protocol.RoleAssistant:
			var parts []*genai.Part
			for _, p := range msg.Content {
				switch p.Type {
				case protocol.ContentTypeText:
					if p.Text != "" {
						parts = append(parts, genai.NewPartFromText(p.Text))
					}
				case protocol.ContentTypeReasoning:
					if p.Text != "" {
						parts = append(parts, &genai.Part{Text: p.Text, Thought: true})
					}
				case protocol.ContentTypeToolCall:
					if p.ToolCall != nil {
						var argsMap map[string]any
						_ = json.Unmarshal([]byte(p.ToolCall.Arguments), &argsMap)
						part := &genai.Part{
							FunctionCall: &genai.FunctionCall{
								ID:   p.ToolCall.ID,
								Name: p.ToolCall.Name,
								Args: argsMap,
							},
						}
						parts = append(parts, part)
					}
				}
			}
			if len(parts) > 0 {
				contents = append(contents, genai.NewContentFromParts(parts, genai.RoleModel))
			}

		case protocol.RoleTool:
			var parts []*genai.Part
			for _, p := range msg.Content {
				if p.Type == protocol.ContentTypeToolResult && p.ToolResult != nil {
					tr := p.ToolResult
					name := tr.ToolName
					if name == "" {
						name = findToolNameByCallID(msgs, tr.ToolCallID)
					}
					response := buildFunctionResponse(tr)
					part := &genai.Part{
						FunctionResponse: &genai.FunctionResponse{
							ID:       tr.ToolCallID,
							Name:     name,
							Response: response,
						},
					}
					parts = append(parts, part)
				}
			}
			if len(parts) > 0 {
				contents = append(contents, genai.NewContentFromParts(parts, genai.RoleUser))
			}
		}
	}

	// Set system instruction
	if systemText != "" {
		config.SystemInstruction = genai.NewContentFromText(systemText, genai.RoleUser)
	}

	// Convert tools
	if len(toolsList) > 0 {
		var funcDecls []*genai.FunctionDeclaration
		for _, t := range toolsList {
			schema := t.Schema()
			geminiSchema := convertSchemaToGemini(schema)
			funcDecls = append(funcDecls, &genai.FunctionDeclaration{
				Name:        t.Name(),
				Description: t.Description(),
				Parameters:  geminiSchema,
			})
		}
		config.Tools = []*genai.Tool{
			{FunctionDeclarations: funcDecls},
		}
	}

	// Translate tool choice.
	switch toolChoice.Mode {
	case providers.ToolChoiceRequired:
		config.ToolConfig = &genai.ToolConfig{
			FunctionCallingConfig: &genai.FunctionCallingConfig{
				Mode: genai.FunctionCallingConfigModeAny,
			},
		}
	case providers.ToolChoiceSpecific:
		config.ToolConfig = &genai.ToolConfig{
			FunctionCallingConfig: &genai.FunctionCallingConfig{
				Mode:                 genai.FunctionCallingConfigModeAny,
				AllowedFunctionNames: []string{toolChoice.Name},
			},
		}
	}

	return contents, config, nil
}

// findToolNameByCallID scans message history to find the function name for a given tool call ID.
func findToolNameByCallID(msgs []protocol.Message, callID string) string {
	for _, msg := range msgs {
		for _, p := range msg.Content {
			if p.Type == protocol.ContentTypeToolCall && p.ToolCall != nil && p.ToolCall.ID == callID {
				return p.ToolCall.Name
			}
		}
	}
	return ""
}

// buildFunctionResponse converts a protocol ToolResult to a map suitable for genai.FunctionResponse.
func buildFunctionResponse(tr *protocol.ToolResult) map[string]any {
	response := make(map[string]any)

	switch {
	case tr.JSON != nil:
		if m, ok := tr.JSON.(map[string]any); ok {
			return m
		}
		response["output"] = tr.JSON
	case tr.Type == protocol.ToolResultTypeExecutionDenied:
		response["error"] = fmt.Sprintf("User denied execution: %s", tr.Reason)
	case tr.Type == protocol.ToolResultTypeContent:
		var textParts []string
		for _, c := range tr.Content {
			if c.Type == protocol.ContentTypeText {
				textParts = append(textParts, c.Text)
			}
		}
		response["output"] = strings.Join(textParts, "\n")
	case tr.IsError:
		response["error"] = tr.Text
	default:
		response["output"] = tr.Text
	}

	return response
}

// convertSchemaToGemini converts a JSON schema (any) to a genai.Schema.
func convertSchemaToGemini(schema any) *genai.Schema {
	schemaMap, ok := schema.(map[string]any)
	if !ok {
		b, _ := json.Marshal(schema)
		_ = json.Unmarshal(b, &schemaMap)
	}
	return convertSchemaMap(schemaMap)
}

// convertSchemaMap recursively converts a JSON schema map to a genai.Schema.
func convertSchemaMap(m map[string]any) *genai.Schema {
	s := &genai.Schema{
		Type: mapSchemaType(m),
	}

	if desc, ok := m["description"].(string); ok {
		s.Description = desc
	}

	if props, ok := m["properties"].(map[string]any); ok {
		s.Properties = make(map[string]*genai.Schema)
		for key, val := range props {
			if propMap, ok := val.(map[string]any); ok {
				s.Properties[key] = convertSchemaMap(propMap)
			}
		}
	}

	if req, ok := m["required"].([]any); ok {
		for _, r := range req {
			if rs, ok := r.(string); ok {
				s.Required = append(s.Required, rs)
			}
		}
	}

	if items, ok := m["items"].(map[string]any); ok {
		s.Items = convertSchemaMap(items)
	}

	if enum, ok := m["enum"].([]any); ok {
		for _, e := range enum {
			if es, ok := e.(string); ok {
				s.Enum = append(s.Enum, es)
			}
		}
	}

	return s
}

func mapSchemaType(m map[string]any) genai.Type {
	typeStr, _ := m["type"].(string)
	switch typeStr {
	case "string":
		return genai.TypeString
	case "number":
		return genai.TypeNumber
	case "integer":
		return genai.TypeInteger
	case "boolean":
		return genai.TypeBoolean
	case "array":
		return genai.TypeArray
	case "object":
		return genai.TypeObject
	default:
		return genai.TypeObject
	}
}
