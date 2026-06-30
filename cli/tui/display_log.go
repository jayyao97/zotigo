package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/session"
)

func (m *Model) appendTurnPaused() {
	m.appendAssistantMessageFromDisplayContent()
	m.appendDisplayItem(session.DisplayItem{
		Type: session.DisplayItemTurnPaused,
		Turn: &session.DisplayTurn{
			ID:     m.currentDisplayTurnID(),
			Reason: "need_approval",
		},
	})
}

func (m *Model) appendTurnCompleted(finish protocol.FinishReason) {
	lastMessage := m.appendAssistantMessageFromDisplayContent()
	turnID := m.currentDisplayTurnID()
	m.appendDisplayItem(session.DisplayItem{
		Type: session.DisplayItemTurnCompleted,
		Turn: &session.DisplayTurn{
			ID:                   turnID,
			Status:               "completed",
			ProviderFinishReason: string(finish),
			LastAgentMessage:     displayText(lastMessage.Content),
			DurationMS:           time.Since(m.turnStartTime).Milliseconds(),
		},
	})
	m.displayTurnID = ""
}

func (m *Model) appendTurnInterrupted(reason string) {
	m.appendAssistantMessageFromDisplayContent()
	turnID := m.currentDisplayTurnID()
	m.appendDisplayItem(session.DisplayItem{
		Type: session.DisplayItemTurnInterrupted,
		Turn: &session.DisplayTurn{
			ID:         turnID,
			Status:     "interrupted",
			Reason:     reason,
			DurationMS: time.Since(m.turnStartTime).Milliseconds(),
		},
	})
	m.displayTurnID = ""
}

func (m *Model) appendTurnFailed(err error) {
	errText := fmt.Sprintf("%v", err)
	m.appendAssistantMessageFromDisplayContent()
	turnID := m.currentDisplayTurnID()
	m.appendDisplayItem(session.DisplayItem{
		Type:  session.DisplayItemError,
		Error: errText,
	})
	m.appendDisplayItem(session.DisplayItem{
		Type:  session.DisplayItemTurnFailed,
		Error: errText,
		Turn: &session.DisplayTurn{
			ID:         turnID,
			Status:     "failed",
			DurationMS: time.Since(m.turnStartTime).Milliseconds(),
		},
	})
	m.displayTurnID = ""
	m.eventCh = nil
}

func (m *Model) appendDeniedToolResults(outputs []protocol.ToolResult) {
	for _, output := range outputs {
		result := output
		m.appendToolResultDisplayPart(&result)
	}
}

func (m *Model) appendAssistantDisplayPart(partType string, text string) {
	if text == "" {
		return
	}
	lastIdx := len(m.displayAsstContent) - 1
	if lastIdx >= 0 && m.displayAsstContent[lastIdx].Type == partType {
		m.displayAsstContent[lastIdx].Text += text
		return
	}
	m.displayAsstContent = append(m.displayAsstContent, session.DisplayContentPart{
		Type: partType,
		Text: text,
	})
}

func (m *Model) appendToolCallDisplayPart(call *protocol.ToolCall) {
	if call == nil {
		return
	}
	m.displayAsstContent = append(m.displayAsstContent, session.DisplayContentPart{
		Type:     string(protocol.ContentTypeToolCall),
		Summary:  formatToolCall(call),
		ToolCall: displayToolCallFromProtocol(call),
	})
}

func displayToolCallFromProtocol(call *protocol.ToolCall) *session.DisplayToolCall {
	if call == nil {
		return nil
	}
	return &session.DisplayToolCall{
		ID:        call.ID,
		Name:      call.Name,
		Arguments: call.Arguments,
	}
}

func (m *Model) appendToolResultDisplayPart(result *protocol.ToolResult) {
	if result == nil {
		return
	}
	m.displayAsstContent = append(m.displayAsstContent, session.DisplayContentPart{
		Type:       string(protocol.ContentTypeToolResult),
		Summary:    displayToolResultText(result, 10),
		ToolResult: displayToolResultFromProtocol(result),
	})
}

func displayToolResultFromProtocol(result *protocol.ToolResult) *session.DisplayToolResult {
	if result == nil {
		return nil
	}
	return &session.DisplayToolResult{
		ToolCallID: result.ToolCallID,
		ToolName:   result.ToolName,
		ResultType: string(result.Type),
		Text:       result.Text,
		JSON:       result.JSON,
		Reason:     result.Reason,
		Content:    displayToolResultContentFromProtocol(result.Content),
		IsError:    result.IsError,
	}
}

func displayToolResultContentFromProtocol(content []protocol.ToolResultContentPart) []session.DisplayToolResultContentPart {
	if len(content) == 0 {
		return nil
	}
	parts := make([]session.DisplayToolResultContentPart, 0, len(content))
	for _, part := range content {
		parts = append(parts, session.DisplayToolResultContentPart{
			Type:  string(part.Type),
			Text:  part.Text,
			Image: displayMediaPartFromProtocol(part.Image),
		})
	}
	return parts
}

func displayMediaPartFromProtocol(media *protocol.MediaPart) *session.DisplayMediaPart {
	if media == nil {
		return nil
	}
	return &session.DisplayMediaPart{
		Data:      media.Data,
		URL:       media.URL,
		FileID:    media.FileID,
		MediaType: media.MediaType,
	}
}

func (m *Model) appendAssistantMessageFromDisplayContent() session.DisplayItem {
	if len(m.displayAsstContent) == 0 {
		return session.DisplayItem{}
	}
	content := make([]session.DisplayContentPart, len(m.displayAsstContent))
	copy(content, m.displayAsstContent)
	item := session.DisplayItem{
		Type:    session.DisplayItemAssistantMessage,
		Role:    string(protocol.RoleAssistant),
		Content: content,
	}
	m.appendDisplayItem(item)
	m.displayAsstContent = nil
	return item
}

func (m *Model) appendDisplayItem(item session.DisplayItem) {
	if m.sessionMgr == nil || m.sessionID == "" {
		return
	}
	_, _ = m.sessionMgr.AppendDisplayItem(m.sessionID, item)
}

func (m *Model) currentDisplayTurnID() string {
	if m.displayTurnID != "" {
		return m.displayTurnID
	}
	return fmt.Sprintf("turn_%d", m.turnStartTime.UnixNano())
}

func (m *Model) hasOpenDisplayTurn() bool {
	return m.displayTurnID != "" || m.thinking
}

func displayMessageItem(itemType session.DisplayItemType, role protocol.Role, msg protocol.Message) session.DisplayItem {
	return session.DisplayItem{
		Type:    itemType,
		Role:    string(role),
		Content: displayContentFromMessage(msg),
	}
}

func displayContentFromMessage(msg protocol.Message) []session.DisplayContentPart {
	text := msg.String()
	if text == "" {
		return nil
	}
	return []session.DisplayContentPart{{Type: string(protocol.ContentTypeText), Text: text}}
}

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
		if part.Summary != "" {
			return part.Summary
		}
		if part.Text != "" {
			return part.Text
		}
		if part.ToolCall != nil {
			return formatDisplayToolCall(part.ToolCall)
		}
	case string(protocol.ContentTypeToolResult):
		if part.Summary != "" {
			return part.Summary
		}
		if part.Text != "" {
			return part.Text
		}
		if part.ToolResult != nil {
			return displayToolResultTextFromDisplay(part.ToolResult, 10)
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

func contextCompacted(sess *session.Session, snap agent.Snapshot) bool {
	return historyCompacted(sess.AgentSnapshot.History, snap.History)
}

func historyCompacted(previous []protocol.Message, current []protocol.Message) bool {
	if len(previous) == 0 || len(current) >= len(previous) {
		return false
	}
	for _, msg := range current {
		if strings.Contains(msg.String(), "[Previous conversation summary]") {
			return true
		}
	}
	return false
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
