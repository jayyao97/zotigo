package session

// DisplayContentKind classifies a record's content, independently of its outer
// event type: tool calls and results can also be assistant_message records.
type DisplayContentKind string

const (
	DisplayContentConversation DisplayContentKind = "conversation"
	DisplayContentTool         DisplayContentKind = "tool"
	DisplayContentEvent        DisplayContentKind = "event"
)

func (part DisplayContentPart) IsTool() bool {
	return part.Type == "tool_call" || part.Type == "tool_result"
}

func (item DisplayItem) ContentKind() DisplayContentKind {
	switch item.Type {
	case DisplayItemUserMessage, DisplayItemSteeringMessage:
		// Image-only prompts (and historical prompts with no content parts) are
		// still user input and must not disappear from the conversation.
		return DisplayContentConversation
	case DisplayItemAssistantMessage:
		hasTool := false
		for _, part := range item.Content {
			if part.IsTool() {
				hasTool = true
			} else {
				return DisplayContentConversation
			}
		}
		if hasTool {
			return DisplayContentTool
		}
	case DisplayItemToolExecutionStarted:
		return DisplayContentTool
	}
	return DisplayContentEvent
}
