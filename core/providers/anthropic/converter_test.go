package anthropic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/tools"
)

func TestConvertToAnthropicParams_TextOnly(t *testing.T) {
	msgs := []protocol.Message{
		protocol.NewUserMessage("Hello"),
	}

	params, err := convertToAnthropicParams(msgs, nil)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if len(params.Messages) != 1 {
		t.Fatalf("Expected 1 message, got %d", len(params.Messages))
	}

	b, _ := json.Marshal(params.Messages[0])
	jsonStr := string(b)
	if !strings.Contains(jsonStr, `"role":"user"`) {
		t.Errorf("Expected user role, got: %s", jsonStr)
	}
	if !strings.Contains(jsonStr, `"text":"Hello"`) {
		t.Errorf("Expected text Hello, got: %s", jsonStr)
	}
}

func TestConvertToAnthropicParams_SystemPrompt(t *testing.T) {
	msgs := []protocol.Message{
		protocol.NewSystemMessage("You are helpful."),
		protocol.NewUserMessage("Hi"),
	}

	params, err := convertToAnthropicParams(msgs, nil)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if len(params.Messages) != 1 {
		t.Fatalf("Expected 1 non-system message, got %d", len(params.Messages))
	}

	b, _ := json.Marshal(params)
	jsonStr := string(b)
	if !strings.Contains(jsonStr, `"system"`) {
		t.Errorf("Expected system field in params: %s", jsonStr)
	}
	if !strings.Contains(jsonStr, "You are helpful.") {
		t.Errorf("Expected system text in params: %s", jsonStr)
	}
}

func TestConvertToAnthropicParams_AssistantWithToolCall(t *testing.T) {
	msg := protocol.NewAssistantMessage("Let me check.")
	msg.AddToolCall(protocol.ToolCall{
		ID:        "call_1",
		Name:      "read_file",
		Arguments: `{"path":"/tmp/test"}`,
	})

	params, err := convertToAnthropicParams([]protocol.Message{msg}, nil)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if len(params.Messages) != 1 {
		t.Fatalf("Expected 1 message, got %d", len(params.Messages))
	}

	b, _ := json.Marshal(params.Messages[0])
	jsonStr := string(b)
	if !strings.Contains(jsonStr, `"type":"tool_use"`) {
		t.Errorf("Expected tool_use block, got: %s", jsonStr)
	}
	if !strings.Contains(jsonStr, `"id":"call_1"`) {
		t.Errorf("Expected tool call id call_1, got: %s", jsonStr)
	}
	if !strings.Contains(jsonStr, `"name":"read_file"`) {
		t.Errorf("Expected tool name read_file, got: %s", jsonStr)
	}
	if !strings.Contains(jsonStr, `"/tmp/test"`) {
		t.Errorf("Expected tool args in output, got: %s", jsonStr)
	}
}

func TestConvertToAnthropicParams_ToolResult(t *testing.T) {
	tr := protocol.NewTextToolResult("call_1", "success", false)
	msg := protocol.NewToolMessage([]protocol.ToolResult{tr})

	params, err := convertToAnthropicParams([]protocol.Message{msg}, nil)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if len(params.Messages) != 1 {
		t.Fatalf("Expected 1 message, got %d", len(params.Messages))
	}

	b, _ := json.Marshal(params.Messages[0])
	jsonStr := string(b)
	if !strings.Contains(jsonStr, `"type":"tool_result"`) {
		t.Errorf("Expected tool_result block, got: %s", jsonStr)
	}
	if !strings.Contains(jsonStr, `"tool_use_id":"call_1"`) {
		t.Errorf("Expected tool_use_id call_1, got: %s", jsonStr)
	}
	if !strings.Contains(jsonStr, `"text":"success"`) {
		t.Errorf("Expected tool result text, got: %s", jsonStr)
	}
}

func TestConvertToAnthropicParams_Image(t *testing.T) {
	data := []byte("fake")
	msg := protocol.Message{
		Role: protocol.RoleUser,
		Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeImage, Image: &protocol.MediaPart{Data: data, MediaType: "image/jpeg"}},
		},
	}

	params, err := convertToAnthropicParams([]protocol.Message{msg}, nil)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	b, _ := json.Marshal(params.Messages[0])
	jsonStr := string(b)
	expectedB64 := base64.StdEncoding.EncodeToString(data)
	if !strings.Contains(jsonStr, expectedB64) {
		t.Errorf("Expected base64 image data in params: %s", jsonStr)
	}
	if !strings.Contains(jsonStr, "image/jpeg") {
		t.Errorf("Expected image mime type in params: %s", jsonStr)
	}
}

type mockTool struct {
	name        string
	description string
	schema      any
}

func (m *mockTool) Name() string        { return m.name }
func (m *mockTool) Description() string { return m.description }
func (m *mockTool) Schema() any         { return m.schema }
func (m *mockTool) Execute(_ context.Context, _ executor.Executor, _ string) (any, error) {
	return nil, nil
}

var _ tools.Tool = (*mockTool)(nil)

func TestConvertToAnthropicParams_Tools(t *testing.T) {
	tool := &mockTool{
		name:        "read_file",
		description: "Read file content",
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

	params, err := convertToAnthropicParams([]protocol.Message{protocol.NewUserMessage("read")}, []tools.Tool{tool})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if len(params.Tools) != 1 {
		t.Fatalf("Expected 1 tool, got %d", len(params.Tools))
	}

	b, _ := json.Marshal(params.Tools[0])
	jsonStr := string(b)
	if !strings.Contains(jsonStr, `"name":"read_file"`) {
		t.Errorf("Expected tool name in params: %s", jsonStr)
	}
	if !strings.Contains(jsonStr, "Read file content") {
		t.Errorf("Expected tool description in params: %s", jsonStr)
	}
	if !strings.Contains(jsonStr, `"path"`) {
		t.Errorf("Expected path property in schema: %s", jsonStr)
	}
}

func TestConvertToolResult_ExecutionDenied(t *testing.T) {
	part := convertToolResult(&protocol.ToolResult{
		ToolCallID: "call_1",
		Type:       protocol.ToolResultTypeExecutionDenied,
		Reason:     "not allowed",
	})

	b, _ := json.Marshal(part)
	jsonStr := string(b)
	if !strings.Contains(jsonStr, `"tool_use_id":"call_1"`) {
		t.Errorf("Expected tool_use_id in result: %s", jsonStr)
	}
	if !strings.Contains(jsonStr, "User denied execution: not allowed") {
		t.Errorf("Expected denied reason in result: %s", jsonStr)
	}
}
