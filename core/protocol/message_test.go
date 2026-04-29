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

func TestUsageHelpers(t *testing.T) {
	u := protocol.Usage{
		InputTokens:              100,
		OutputTokens:             200,
		CacheCreationInputTokens: 50,
		CacheReadInputTokens:     150,
	}

	if got := u.TotalInput(); got != 300 {
		t.Errorf("TotalInput = %d, want 300", got)
	}
	if got := u.Normalized().TotalTokens; got != 500 {
		t.Errorf("Normalized().TotalTokens = %d, want 500", got)
	}

	// Normalized is a no-op once TotalTokens is populated.
	u2 := u
	u2.TotalTokens = 999
	if got := u2.Normalized().TotalTokens; got != 999 {
		t.Errorf("Normalized() should not overwrite an existing TotalTokens, got %d", got)
	}

	sum := u.Add(u)
	if sum.InputTokens != 200 || sum.CacheReadInputTokens != 300 || sum.OutputTokens != 400 {
		t.Errorf("Add component-wise wrong: %+v", sum)
	}
}

func TestSessionAndLastTurnUsage(t *testing.T) {
	usage1 := &protocol.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}
	usage2 := &protocol.Usage{InputTokens: 30, CacheReadInputTokens: 100, OutputTokens: 7, TotalTokens: 137}

	history := []protocol.Message{
		protocol.NewUserMessage("hi"),
		{
			Role:     protocol.RoleAssistant,
			Metadata: &protocol.MessageMetadata{Usage: usage1},
		},
		protocol.NewUserMessage("again"),
		{
			Role:     protocol.RoleAssistant,
			Metadata: &protocol.MessageMetadata{Usage: usage2},
		},
	}

	total := protocol.SessionUsage(history)
	if total.InputTokens != 40 || total.CacheReadInputTokens != 100 || total.OutputTokens != 12 || total.TotalTokens != 152 {
		t.Errorf("SessionUsage wrong: %+v", total)
	}

	last, ok := protocol.LastTurnUsage(history)
	if !ok {
		t.Fatal("LastTurnUsage returned ok=false on populated history")
	}
	if last.OutputTokens != 7 {
		t.Errorf("LastTurnUsage returned wrong turn: %+v", last)
	}

	if _, ok := protocol.LastTurnUsage(nil); ok {
		t.Error("LastTurnUsage should be ok=false on empty history")
	}

	if got := protocol.CountAssistantTurns(history); got != 2 {
		t.Errorf("CountAssistantTurns = %d, want 2", got)
	}
}

// Guards the regression where usage records persisted before the agent
// started filling TotalTokens caused SessionUsage to roll up a total
// of 0, which made the TUI exit summary hide itself.
func TestSessionUsage_NormalizesLegacyTurns(t *testing.T) {
	legacy := &protocol.Usage{
		InputTokens:          100,
		CacheReadInputTokens: 5_000,
		OutputTokens:         42,
	}
	history := []protocol.Message{
		{
			Role:     protocol.RoleAssistant,
			Metadata: &protocol.MessageMetadata{Usage: legacy},
		},
	}
	got := protocol.SessionUsage(history)
	if got.TotalTokens == 0 {
		t.Fatalf("SessionUsage failed to normalize legacy usage: %+v", got)
	}
	if want := legacy.TotalInput() + legacy.OutputTokens; got.TotalTokens != want {
		t.Errorf("SessionUsage.TotalTokens = %d, want %d", got.TotalTokens, want)
	}
}
