package gemini

import (
	"context"
	"testing"

	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/tools"
	"google.golang.org/genai"
)

func TestConvertToGeminiParams_TextOnly(t *testing.T) {
	msgs := []protocol.Message{
		protocol.NewUserMessage("Hello"),
	}

	contents, config, err := convertToGeminiParams(msgs, nil, providers.ToolChoice{})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if len(contents) != 1 {
		t.Fatalf("Expected 1 content, got %d", len(contents))
	}
	if contents[0].Role != "user" {
		t.Errorf("Expected role 'user', got '%s'", contents[0].Role)
	}
	if len(contents[0].Parts) != 1 {
		t.Fatalf("Expected 1 part, got %d", len(contents[0].Parts))
	}
	if contents[0].Parts[0].Text != "Hello" {
		t.Errorf("Expected text 'Hello', got '%s'", contents[0].Parts[0].Text)
	}
	if config.SystemInstruction != nil {
		t.Error("Expected no system instruction")
	}
}

func TestConvertToGeminiParams_SystemInstruction(t *testing.T) {
	msgs := []protocol.Message{
		protocol.NewSystemMessage("You are a helpful assistant."),
		protocol.NewUserMessage("Hi"),
	}

	contents, config, err := convertToGeminiParams(msgs, nil, providers.ToolChoice{})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// System message should not appear in contents
	if len(contents) != 1 {
		t.Fatalf("Expected 1 content (user only), got %d", len(contents))
	}

	if config.SystemInstruction == nil {
		t.Fatal("Expected system instruction to be set")
	}
	if len(config.SystemInstruction.Parts) != 1 {
		t.Fatalf("Expected 1 system instruction part, got %d", len(config.SystemInstruction.Parts))
	}
	if config.SystemInstruction.Parts[0].Text != "You are a helpful assistant." {
		t.Errorf("Expected system text, got '%s'", config.SystemInstruction.Parts[0].Text)
	}
}

func TestConvertToGeminiParams_AssistantWithToolCall(t *testing.T) {
	msg := protocol.NewAssistantMessage("Let me check.")
	msg.AddToolCall(protocol.ToolCall{
		ID:        "call_1",
		Name:      "read_file",
		Arguments: `{"path":"/tmp/test"}`,
	})

	contents, _, err := convertToGeminiParams([]protocol.Message{msg}, nil, providers.ToolChoice{})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if len(contents) != 1 {
		t.Fatalf("Expected 1 content, got %d", len(contents))
	}
	if contents[0].Role != "model" {
		t.Errorf("Expected role 'model', got '%s'", contents[0].Role)
	}

	// Should have 2 parts: text + function call
	if len(contents[0].Parts) != 2 {
		t.Fatalf("Expected 2 parts, got %d", len(contents[0].Parts))
	}

	// First part: text
	if contents[0].Parts[0].Text != "Let me check." {
		t.Errorf("Expected text part, got '%s'", contents[0].Parts[0].Text)
	}

	// Second part: function call
	fc := contents[0].Parts[1].FunctionCall
	if fc == nil {
		t.Fatal("Expected function call part")
	}
	if fc.Name != "read_file" {
		t.Errorf("Expected function name 'read_file', got '%s'", fc.Name)
	}
	if fc.ID != "call_1" {
		t.Errorf("Expected function call ID 'call_1', got '%s'", fc.ID)
	}
	if path, ok := fc.Args["path"].(string); !ok || path != "/tmp/test" {
		t.Errorf("Expected args path '/tmp/test', got %v", fc.Args)
	}
}

func TestConvertToGeminiParams_ToolResult(t *testing.T) {
	// Set up assistant message with tool call for name recovery
	assistantMsg := protocol.NewAssistantMessage("")
	assistantMsg.AddToolCall(protocol.ToolCall{
		ID:   "call_1",
		Name: "read_file",
	})

	tr := protocol.NewTextToolResult("call_1", "file content here", false)
	toolMsg := protocol.NewToolMessage([]protocol.ToolResult{tr})

	contents, _, err := convertToGeminiParams([]protocol.Message{assistantMsg, toolMsg}, nil, providers.ToolChoice{})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	// assistantMsg + toolMsg = 2 contents
	if len(contents) != 2 {
		t.Fatalf("Expected 2 contents, got %d", len(contents))
	}

	// Tool result should be in user role
	toolContent := contents[1]
	if toolContent.Role != "user" {
		t.Errorf("Expected role 'user' for tool result, got '%s'", toolContent.Role)
	}

	if len(toolContent.Parts) != 1 {
		t.Fatalf("Expected 1 part, got %d", len(toolContent.Parts))
	}

	fr := toolContent.Parts[0].FunctionResponse
	if fr == nil {
		t.Fatal("Expected function response part")
	}
	if fr.Name != "read_file" {
		t.Errorf("Expected function name 'read_file', got '%s'", fr.Name)
	}
	if fr.ID != "call_1" {
		t.Errorf("Expected function response ID 'call_1', got '%s'", fr.ID)
	}
	if output, ok := fr.Response["output"].(string); !ok || output != "file content here" {
		t.Errorf("Expected output 'file content here', got %v", fr.Response)
	}
}

func TestConvertToGeminiParams_Image(t *testing.T) {
	data := []byte("fake-image-data")
	msg := protocol.Message{
		Role: protocol.RoleUser,
		Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeImage, Image: &protocol.MediaPart{Data: data, MediaType: "image/jpeg"}},
		},
	}

	contents, _, err := convertToGeminiParams([]protocol.Message{msg}, nil, providers.ToolChoice{})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if len(contents) != 1 {
		t.Fatalf("Expected 1 content, got %d", len(contents))
	}
	part := contents[0].Parts[0]
	if part.InlineData == nil {
		t.Fatal("Expected inline data for image")
	}
	if part.InlineData.MIMEType != "image/jpeg" {
		t.Errorf("Expected mime type 'image/jpeg', got '%s'", part.InlineData.MIMEType)
	}
	if string(part.InlineData.Data) != string(data) {
		t.Error("Expected matching image data")
	}
}

func TestConvertToGeminiParams_ReasoningBlock(t *testing.T) {
	msg := protocol.Message{
		Role: protocol.RoleAssistant,
		Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeReasoning, Text: "Let me think step by step..."},
			{Type: protocol.ContentTypeText, Text: "The answer is 42."},
		},
	}

	contents, _, err := convertToGeminiParams([]protocol.Message{msg}, nil, providers.ToolChoice{})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if len(contents) != 1 {
		t.Fatalf("Expected 1 content, got %d", len(contents))
	}
	if len(contents[0].Parts) != 2 {
		t.Fatalf("Expected 2 parts, got %d", len(contents[0].Parts))
	}

	// First part: reasoning with Thought=true
	reasoningPart := contents[0].Parts[0]
	if reasoningPart.Text != "Let me think step by step..." {
		t.Errorf("Expected reasoning text, got '%s'", reasoningPart.Text)
	}
	if !reasoningPart.Thought {
		t.Error("Reasoning part should have Thought=true")
	}

	// Second part: regular text without Thought
	textPart := contents[0].Parts[1]
	if textPart.Text != "The answer is 42." {
		t.Errorf("Expected text, got '%s'", textPart.Text)
	}
	if textPart.Thought {
		t.Error("Text part should not have Thought=true")
	}
}

// mockTool implements tools.Tool for testing.
type mockTool struct {
	name        string
	description string
	schema      any
}

func (m *mockTool) Name() string        { return m.name }
func (m *mockTool) Description() string { return m.description }
func (m *mockTool) Schema() any         { return m.schema }
func (m *mockTool) Classify(_ tools.SafetyCall) tools.SafetyDecision {
	return tools.SafetyDecision{Level: tools.LevelSafe}
}
func (m *mockTool) Execute(_ context.Context, _ executor.Executor, _ string) (any, error) {
	return nil, nil
}

var _ tools.Tool = (*mockTool)(nil)

func TestConvertToGeminiParams_Tools(t *testing.T) {
	tool := &mockTool{
		name:        "read_file",
		description: "Read a file from disk",
		schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path",
				},
			},
			"required": []any{"path"},
		},
	}

	msgs := []protocol.Message{protocol.NewUserMessage("read /tmp")}
	_, config, err := convertToGeminiParams(msgs, []tools.Tool{tool}, providers.ToolChoice{})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if len(config.Tools) != 1 {
		t.Fatalf("Expected 1 tool group, got %d", len(config.Tools))
	}
	if len(config.Tools[0].FunctionDeclarations) != 1 {
		t.Fatalf("Expected 1 function declaration, got %d", len(config.Tools[0].FunctionDeclarations))
	}

	fd := config.Tools[0].FunctionDeclarations[0]
	if fd.Name != "read_file" {
		t.Errorf("Expected name 'read_file', got '%s'", fd.Name)
	}
	if fd.Description != "Read a file from disk" {
		t.Errorf("Expected description mismatch")
	}
	if fd.Parameters == nil {
		t.Fatal("Expected parameters schema")
	}
	if fd.Parameters.Type != genai.TypeObject {
		t.Errorf("Expected object type, got '%s'", fd.Parameters.Type)
	}
	if _, ok := fd.Parameters.Properties["path"]; !ok {
		t.Error("Expected 'path' property")
	}
	if len(fd.Parameters.Required) != 1 || fd.Parameters.Required[0] != "path" {
		t.Errorf("Expected required=['path'], got %v", fd.Parameters.Required)
	}
}

func TestConvertSchemaMap_Nested(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name": map[string]any{
				"type":        "string",
				"description": "The name",
			},
			"tags": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "string",
				},
			},
			"status": map[string]any{
				"type": "string",
				"enum": []any{"active", "inactive"},
			},
		},
		"required": []any{"name"},
	}

	s := convertSchemaMap(schema)

	if s.Type != genai.TypeObject {
		t.Errorf("Expected OBJECT type, got %s", s.Type)
	}
	if len(s.Properties) != 3 {
		t.Errorf("Expected 3 properties, got %d", len(s.Properties))
	}
	if s.Properties["name"].Type != genai.TypeString {
		t.Errorf("Expected STRING type for name")
	}
	if s.Properties["tags"].Type != genai.TypeArray {
		t.Errorf("Expected ARRAY type for tags")
	}
	if s.Properties["tags"].Items == nil || s.Properties["tags"].Items.Type != genai.TypeString {
		t.Errorf("Expected STRING items for tags array")
	}
	if len(s.Properties["status"].Enum) != 2 {
		t.Errorf("Expected 2 enum values, got %d", len(s.Properties["status"].Enum))
	}
	if len(s.Required) != 1 || s.Required[0] != "name" {
		t.Errorf("Expected required=['name'], got %v", s.Required)
	}
}

func TestBuildFunctionResponse_Text(t *testing.T) {
	tr := &protocol.ToolResult{
		Type: protocol.ToolResultTypeText,
		Text: "result text",
	}
	resp := buildFunctionResponse(tr)
	if output, ok := resp["output"].(string); !ok || output != "result text" {
		t.Errorf("Expected output 'result text', got %v", resp)
	}
}

func TestBuildFunctionResponse_JSON(t *testing.T) {
	tr := &protocol.ToolResult{
		Type: protocol.ToolResultTypeJSON,
		JSON: map[string]any{"key": "value"},
	}
	resp := buildFunctionResponse(tr)
	if v, ok := resp["key"].(string); !ok || v != "value" {
		t.Errorf("Expected map with key='value', got %v", resp)
	}
}

func TestBuildFunctionResponse_Error(t *testing.T) {
	tr := &protocol.ToolResult{
		Type:    protocol.ToolResultTypeErrorText,
		Text:    "something failed",
		IsError: true,
	}
	resp := buildFunctionResponse(tr)
	if errMsg, ok := resp["error"].(string); !ok || errMsg != "something failed" {
		t.Errorf("Expected error 'something failed', got %v", resp)
	}
}

func TestBuildFunctionResponse_ExecutionDenied(t *testing.T) {
	tr := &protocol.ToolResult{
		Type:   protocol.ToolResultTypeExecutionDenied,
		Reason: "not allowed",
	}
	resp := buildFunctionResponse(tr)
	if errMsg, ok := resp["error"].(string); !ok || errMsg != "User denied execution: not allowed" {
		t.Errorf("Expected denial error, got %v", resp)
	}
}

func TestFindToolNameByCallID(t *testing.T) {
	msg := protocol.NewAssistantMessage("")
	msg.AddToolCall(protocol.ToolCall{
		ID:   "call_42",
		Name: "write_file",
	})

	name := findToolNameByCallID([]protocol.Message{msg}, "call_42")
	if name != "write_file" {
		t.Errorf("Expected 'write_file', got '%s'", name)
	}

	name = findToolNameByCallID([]protocol.Message{msg}, "nonexistent")
	if name != "" {
		t.Errorf("Expected empty string for missing ID, got '%s'", name)
	}
}
