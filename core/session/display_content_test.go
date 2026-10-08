package session

import "testing"

func TestDisplayContentKind(t *testing.T) {
	for _, tc := range []struct {
		name string
		item DisplayItem
		want DisplayContentKind
	}{
		{"user image", DisplayItem{Type: DisplayItemUserMessage, Content: []DisplayContentPart{{Type: "image"}}}, DisplayContentConversation},
		{"steering", DisplayItem{Type: DisplayItemSteeringMessage}, DisplayContentConversation},
		{"assistant text", DisplayItem{Type: DisplayItemAssistantMessage, Content: []DisplayContentPart{{Type: "text", Text: "answer"}}}, DisplayContentConversation},
		{"assistant image", DisplayItem{Type: DisplayItemAssistantMessage, Content: []DisplayContentPart{{Type: "image"}}}, DisplayContentConversation},
		{"mixed", DisplayItem{Type: DisplayItemAssistantMessage, Content: []DisplayContentPart{{Type: "tool_call"}, {Type: "text", Text: "answer"}, {Type: "tool_result"}}}, DisplayContentConversation},
		{"tools only", DisplayItem{Type: DisplayItemAssistantMessage, Content: []DisplayContentPart{{Type: "tool_call"}, {Type: "tool_result"}}}, DisplayContentTool},
		{"empty assistant", DisplayItem{Type: DisplayItemAssistantMessage}, DisplayContentEvent},
		{"tool start", DisplayItem{Type: DisplayItemToolExecutionStarted}, DisplayContentTool},
		{"turn end", DisplayItem{Type: DisplayItemTurnCompleted}, DisplayContentEvent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.item.ContentKind(); got != tc.want {
				t.Fatalf("kind=%s want %s", got, tc.want)
			}
		})
	}
}
