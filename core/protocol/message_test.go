package protocol_test

import (
	"encoding/json"
	"testing"

	"github.com/jayyao97/zotigo/core/protocol"
)

func TestMessageSerialization(t *testing.T) {
	userMsg := protocol.NewUserMessage("Hello world")
	
	data, err := json.Marshal(userMsg)
	if err != nil {
		t.Fatalf("Failed to marshal user message: %v", err)
	}

	var unmarshaledMsg protocol.Message
	if err := json.Unmarshal(data, &unmarshaledMsg); err != nil {
		t.Fatalf("Failed to unmarshal user message: %v", err)
	}

	if unmarshaledMsg.Role != protocol.RoleUser {
		t.Errorf("Expected role %s, got %s", protocol.RoleUser, unmarshaledMsg.Role)
	}
	if len(unmarshaledMsg.Content) != 1 {
		t.Errorf("Expected 1 content part, got %d", len(unmarshaledMsg.Content))
	}
	if unmarshaledMsg.Content[0].Text != "Hello world" {
		t.Errorf("Expected content 'Hello world', got '%s'", unmarshaledMsg.Content[0].Text)
	}
}

func TestAssistantMessageWithToolCall(t *testing.T) {
	tc := protocol.ToolCall{
		ID:        "call_123",
		Name:      "get_weather",
		Arguments: `{"location": "Tokyo"}`,
	}
	
	msg := protocol.NewAssistantMessage("Checking weather...")
	msg.AddToolCall(tc)
	
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Failed to marshal assistant message: %v", err)
	}

	var unmarshaledMsg protocol.Message
	if err := json.Unmarshal(data, &unmarshaledMsg); err != nil {
		t.Fatalf("Failed to unmarshal assistant message: %v", err)
	}

	if unmarshaledMsg.Role != protocol.RoleAssistant {
		t.Errorf("Expected role %s, got %s", protocol.RoleAssistant, unmarshaledMsg.Role)
	}
	
	// Expect 2 parts: Text + ToolCall
	if len(unmarshaledMsg.Content) != 2 {
		t.Fatalf("Expected 2 content parts, got %d", len(unmarshaledMsg.Content))
	}
	
	// Check Tool Call Part
	found := false
	for _, part := range unmarshaledMsg.Content {
		if part.Type == protocol.ContentTypeToolCall {
			found = true
			if part.ToolCall.Name != "get_weather" {
				t.Errorf("Expected tool name 'get_weather', got '%s'", part.ToolCall.Name)
			}
		}
	}
	if !found {
		t.Error("Tool call part not found")
	}
}