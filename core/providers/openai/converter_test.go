package openai

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
)

func TestConvertToChatParams_TextOnly(t *testing.T) {
	msgs := []protocol.Message{
		protocol.NewUserMessage("Hello"),
	}

	params, err := convertToChatParams(msgs, nil, "", providers.ToolChoice{})
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}

	if len(params.Messages) != 1 {
		t.Fatalf("Expected 1 message, got %d", len(params.Messages))
	}

	b, _ := json.Marshal(params.Messages[0])
	jsonStr := string(b)

	if !contains(jsonStr, `"role":"user"`) {
		t.Errorf("Expected role user, got json: %s", jsonStr)
	}
	if !contains(jsonStr, `"text":"Hello"`) {
		t.Errorf("Expected content Hello, got json: %s", jsonStr)
	}
}

func TestConvertToChatParams_ToolResult(t *testing.T) {
	tr := protocol.NewTextToolResult("call_1", "success", false)
	msg := protocol.NewToolMessage([]protocol.ToolResult{tr})

	params, err := convertToChatParams([]protocol.Message{msg}, nil, "", providers.ToolChoice{})
	if err != nil {
		t.Fatalf("Error: %v", err)
	}

	if len(params.Messages) != 1 {
		t.Fatalf("Expected 1 message")
	}

	b, _ := json.Marshal(params.Messages[0])
	jsonStr := string(b)

	if !contains(jsonStr, `"role":"tool"`) {
		t.Errorf("Expected role tool")
	}
	if !contains(jsonStr, `"tool_call_id":"call_1"`) {
		t.Errorf("Expected call_1")
	}
}

func TestConvertToChatParams_Image(t *testing.T) {
	data := []byte("fake")
	msgData := protocol.Message{
		Role: protocol.RoleUser,
		Content: []protocol.ContentPart{
			{Type: protocol.ContentTypeImage, Image: &protocol.MediaPart{Data: data, MediaType: "image/jpeg"}},
		},
	}

	params, _ := convertToChatParams([]protocol.Message{msgData}, nil, "", providers.ToolChoice{})

	b, _ := json.Marshal(params.Messages[0])
	jsonStr := string(b)

	expectedB64 := base64.StdEncoding.EncodeToString(data)
	if !contains(jsonStr, expectedB64) {
		t.Errorf("Expected base64 data in json")
	}
	if !contains(jsonStr, "image_url") {
		t.Errorf("Expected image_url type")
	}
}

func contains(s, substr string) bool {
	for i := 0; i < len(s)-len(substr)+1; i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
