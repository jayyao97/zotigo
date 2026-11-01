package tui

import (
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/session"
)

func displayText(parts []session.DisplayContentPart) string {
	var text string
	for _, part := range parts {
		partText := partDisplayText(part)
		if partText == "" {
			continue
		}
		if text != "" {
			text += "\n"
		}
		text += partText
	}
	return text
}

func partDisplayText(part session.DisplayContentPart) string {
	switch part.Type {
	case string(protocol.ContentTypeToolCall):
		if part.ToolCall != nil {
			return formatDisplayToolCall(part.ToolCall)
		}
	case string(protocol.ContentTypeToolResult):
		if part.ToolResult != nil {
			return toolResultTextFromDisplay(part.ToolResult, defaultToolResultMaxLines)
		}
	default:
		return part.Text
	}
	return ""
}

func formatDisplayToolCall(call *session.DisplayToolCall) string {
	if call == nil {
		return ""
	}
	return formatToolCall(&protocol.ToolCall{
		ID:        call.ID,
		Name:      call.Name,
		Arguments: call.Arguments,
	})
}

func lastOpenDisplayTurnID(items []session.DisplayItem) string {
	openTurnID := ""
	for _, item := range items {
		if item.Turn == nil || item.Turn.ID == "" {
			continue
		}
		switch item.Type {
		case session.DisplayItemTurnStarted, session.DisplayItemTurnPaused:
			openTurnID = item.Turn.ID
		case session.DisplayItemTurnCompleted, session.DisplayItemTurnFailed, session.DisplayItemTurnInterrupted:
			openTurnID = ""
		}
	}
	return openTurnID
}
