package tui

import (
	"fmt"
	"log"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigodclient"
)

func (m *Model) handleServiceEvent(event zotigodclient.Event) tea.Cmd {
	switch {
	case event.Item != nil:
		m.reconnecting = false
		return m.handleItem(*event.Item)
	case event.Delta != nil:
		m.reconnecting = false
		return m.handleDelta(*event.Delta)
	case event.Reconnecting:
		if m.reconnecting {
			return nil
		}
		m.reconnecting = true
		log.Printf("cli_stream_reconnect session=%s err=%v", m.session.ID, event.Err)
		return m.commitLine(warningStyle.Render("⚠ ") + "Lost connection to the session service; reconnecting…")
	case event.Err != nil:
		m.streamClosed = true
		m.thinking = false
		log.Printf("cli_stream_closed session=%s err=%v", m.session.ID, event.Err)
		return m.commitLine(errorStyle.Render("✗ ") + "Session stream ended: " + event.Err.Error())
	}
	return nil
}

func (m *Model) handleDelta(delta zotigodclient.Delta) tea.Cmd {
	if delta.Subagent != nil {
		return nil
	}
	var partType protocol.ContentType
	switch delta.PartType {
	case string(protocol.ContentTypeText):
		partType = protocol.ContentTypeText
	case string(protocol.ContentTypeReasoning):
		partType = protocol.ContentTypeReasoning
	default:
		// Tool progress previews are superseded by durable tool results.
		return nil
	}
	if m.finalized[delta.ItemID] {
		return nil
	}
	var cmds []tea.Cmd
	if m.streamItemID != "" && m.streamItemID != delta.ItemID {
		cmds = append(cmds, m.finishStreamBlock())
	}
	m.streamItemID = delta.ItemID
	if m.streamed[delta.ItemID] == nil {
		m.streamed[delta.ItemID] = make(map[string]string)
	}
	m.streamed[delta.ItemID][delta.PartType] += delta.Delta
	cmds = append(cmds, m.appendStreamText(partType, delta.Delta))
	return tea.Batch(cmds...)
}

// appendStreamText renders streamed text into the live block and commits
// complete lines to scrollback.
func (m *Model) appendStreamText(partType protocol.ContentType, text string) tea.Cmd {
	if text == "" {
		return nil
	}
	isReasoning := partType == protocol.ContentTypeReasoning
	var cmds []tea.Cmd
	if isReasoning && !m.streamingReasoning {
		m.streamingReasoning = true
		m.currentAsstMsg += reasoningLabelStyle.Render("⏺ Thinking...") + "\n"
		m.needsAsstMarker = true
	}
	if !isReasoning && m.streamingReasoning {
		m.streamingReasoning = false
		if !strings.HasSuffix(m.currentAsstMsg, "\n") {
			m.currentAsstMsg += "\n"
		}
		cmds = append(cmds, m.flushStreamedLines())
	}
	if !isReasoning && m.needsAsstMarker {
		m.currentAsstMsg += asstMarkerStyle.Render("⏺ ")
		m.needsAsstMarker = false
	}
	if isReasoning {
		m.currentAsstMsg += reasoningStyle.Render(text)
	} else {
		m.currentAsstMsg += text
	}
	cmds = append(cmds, m.flushStreamedLines())
	return tea.Batch(cmds...)
}

// finishStreamBlock commits whatever is left of the live block.
func (m *Model) finishStreamBlock() tea.Cmd {
	var cmd tea.Cmd
	if m.currentAsstMsg != "" {
		prefix := ""
		if m.streamFlushed == 0 {
			prefix = "\n"
		}
		cmd = m.commitLine(prefix + strings.TrimRight(m.currentAsstMsg, "\n"))
	}
	m.currentAsstMsg = ""
	m.streamFlushed = 0
	m.streamItemID = ""
	m.streamingReasoning = false
	m.needsAsstMarker = true
	return cmd
}

func (m *Model) flushStreamedLines() tea.Cmd {
	idx := strings.LastIndex(m.currentAsstMsg, "\n")
	if idx < 0 {
		return nil
	}
	toCommit := m.currentAsstMsg[:idx]
	m.currentAsstMsg = m.currentAsstMsg[idx+1:]

	prefix := ""
	if m.streamFlushed == 0 {
		prefix = "\n"
	}
	m.streamFlushed++
	return m.commitLine(prefix + toCommit)
}

func (m *Model) handleItem(item session.DisplayItem) tea.Cmd {
	if item.Sequence > m.cursor {
		m.cursor = item.Sequence
	}
	if item.Subagent != nil {
		return nil
	}
	if item.Turn != nil && (item.Type == session.DisplayItemTurnCompleted || item.Type == session.DisplayItemTurnFailed || item.Type == session.DisplayItemTurnInterrupted) {
		m.forgetSteering(item.Turn.ID)
	}
	switch item.Type {
	case session.DisplayItemSessionCommand:
		// Steering is recorded as a session command.
		delete(m.ownInputs, item.ID)
	case session.DisplayItemUserMessage, session.DisplayItemSteeringMessage:
		if m.ownInputs[item.ID] {
			delete(m.ownInputs, item.ID)
			return nil
		}
		// Input from another client (Desktop, ACP, another TUI).
		if str, ok := renderDisplayItem(item); ok {
			return tea.Batch(m.finishStreamBlock(), m.commitLine(str))
		}
	case session.DisplayItemAssistantMessage:
		return m.reconcileAssistantItem(item)
	case session.DisplayItemTurnStarted:
		m.thinking = true
		m.stopping = false
		if item.Turn != nil {
			m.openTurnID = item.Turn.ID
		}
		// Inputs already recorded as user messages started this turn.
		for id := range m.pendingInputs {
			if !m.ownInputs[id] && m.steeringTurns[id] == "" {
				delete(m.pendingInputs, id)
			}
		}
		m.forgetSteering(m.openTurnID)
		if m.turnStartTime.IsZero() {
			m.turnStartTime = time.Now()
		}
	case session.DisplayItemApprovalRequest:
		if item.Approval != nil && len(item.Approval.Pending) > 0 {
			return tea.Batch(m.finishStreamBlock(), m.showApproval(item.Approval))
		}
	case session.DisplayItemApprovalDecision:
		return m.handleApprovalDecision(item)
	case session.DisplayItemTurnCompleted:
		if item.Turn != nil && item.Turn.Usage != nil {
			m.addTurnUsage(*item.Turn.Usage)
		}
		elapsed := time.Since(m.turnStartTime)
		m.endTurn()
		return tea.Sequence(m.finishStreamBlock(),
			m.commitLine(timingStyle.Render("✻ Completed in "+formatDuration(elapsed))),
			m.refreshSession())
	case session.DisplayItemTurnFailed:
		m.endTurn()
		reason := item.Error
		if reason == "" && item.Turn != nil {
			reason = item.Turn.Reason
		}
		return tea.Sequence(m.finishStreamBlock(), m.commitLine("\n"+errorStyle.Render("✗ ")+"Turn failed: "+reason), m.refreshSession())
	case session.DisplayItemTurnInterrupted:
		m.endTurn()
		reason := ""
		if item.Turn != nil && item.Turn.Reason != "" {
			reason = " (" + item.Turn.Reason + ")"
		}
		return tea.Sequence(m.finishStreamBlock(), m.commitLine(warningStyle.Render("⏹ ")+"Turn interrupted"+reason), m.refreshSession())
	case session.DisplayItemApprovalPolicyChanged:
		if item.ApprovalPolicy != nil && item.ApprovalPolicy.To != "" {
			m.approvalPolicy = item.ApprovalPolicy.To
			return m.commitLine(timingStyle.Render("Approval policy: " + approvalPolicyLabel(m.approvalPolicy)))
		}
	case session.DisplayItemProfileChanged:
		if item.Profile != nil {
			m.session.Profile = item.Profile.To
			return m.commitLine(timingStyle.Render("Profile: " + item.Profile.To))
		}
	case session.DisplayItemProfileFailed:
		return m.commitLine(errorStyle.Render("✗ ") + "Profile change failed: " + item.Error)
	case session.DisplayItemInteractionRequest:
		return m.commitLine(warningStyle.Render("⚠ ") + "The agent asked a question this terminal cannot answer; respond from Desktop.")
	case session.DisplayItemError, session.DisplayItemContextCompacted:
		if str, ok := renderDisplayItem(item); ok {
			return tea.Batch(m.finishStreamBlock(), m.commitLine(str))
		}
	}
	return nil
}

// reconcileAssistantItem replaces streamed previews with the durable item.
// Text already shown through deltas is not repeated; text the stream missed
// is appended, and tool calls/results are rendered from the durable copy.
func (m *Model) reconcileAssistantItem(item session.DisplayItem) tea.Cmd {
	streamed := m.streamed[item.ID]
	delete(m.streamed, item.ID)
	m.finalized[item.ID] = true
	if m.streamItemID != item.ID && assistantItemFullyShown(item, streamed) {
		// The preview of this block is complete and a later block may be
		// streaming; leave that live block untouched.
		return nil
	}
	var cmds []tea.Cmd
	if m.streamItemID != "" && m.streamItemID != item.ID {
		// A later block started before this durable item arrived.
		cmds = append(cmds, m.finishStreamBlock())
	}
	for _, part := range item.Content {
		switch part.Type {
		case string(protocol.ContentTypeText), string(protocol.ContentTypeReasoning):
			shown := streamed[part.Type]
			remainder := part.Text
			if shown != "" && strings.HasPrefix(part.Text, shown) {
				remainder = part.Text[len(shown):]
			}
			if m.streamItemID != item.ID && shown != "" && remainder == "" {
				continue
			}
			m.streamItemID = item.ID
			cmds = append(cmds, m.appendStreamText(protocol.ContentType(part.Type), remainder))
		case string(protocol.ContentTypeToolCall):
			cmds = append(cmds, m.finishStreamBlock())
			if summary := formatDisplayToolCall(part.ToolCall); summary != "" {
				cmds = append(cmds, m.commitLine(toolMarkerStyle.Render("⏺ ")+summary))
			}
		case string(protocol.ContentTypeToolResult):
			cmds = append(cmds, m.finishStreamBlock())
			if rendered := renderDisplayToolResult(part.ToolResult); rendered != "" {
				cmds = append(cmds, m.commitLine(rendered))
			}
		}
	}
	if m.streamItemID == item.ID {
		cmds = append(cmds, m.finishStreamBlock())
	}
	return tea.Sequence(cmds...)
}

func assistantItemFullyShown(item session.DisplayItem, streamed map[string]string) bool {
	if len(streamed) == 0 {
		return false
	}
	for _, part := range item.Content {
		switch part.Type {
		case string(protocol.ContentTypeText), string(protocol.ContentTypeReasoning):
			if part.Text != streamed[part.Type] {
				return false
			}
		case string(protocol.ContentTypeToolCall), string(protocol.ContentTypeToolResult):
			return false
		}
	}
	return true
}

func (m *Model) endTurn() {
	m.thinking = false
	m.stopping = false
	m.openTurnID = ""
	m.turnStartTime = time.Time{}
	m.clearApproval()
}

func (m *Model) addTurnUsage(usage protocol.Usage) {
	m.turns++
	last := usage
	m.lastTurnUsage = &last
	m.sessionUsage.InputTokens += usage.InputTokens
	m.sessionUsage.OutputTokens += usage.OutputTokens
	m.sessionUsage.CacheCreationInputTokens += usage.CacheCreationInputTokens
	m.sessionUsage.CacheReadInputTokens += usage.CacheReadInputTokens
	total := usage.TotalTokens
	if total == 0 {
		total = usage.InputTokens + usage.OutputTokens + usage.CacheCreationInputTokens + usage.CacheReadInputTokens
	}
	m.sessionUsage.TotalTokens += total
}

// pendingApproval returns the unresolved approval request of the open turn.
func pendingApproval(items []session.DisplayItem, openTurnID string) *session.DisplayApproval {
	if openTurnID == "" {
		return nil
	}
	var pending *session.DisplayApproval
	for _, item := range items {
		switch {
		case item.Type == session.DisplayItemApprovalRequest && item.Approval != nil && item.Approval.TurnID == openTurnID:
			approval := *item.Approval
			pending = &approval
		case item.Type == session.DisplayItemApprovalDecision && item.Approval != nil && pending != nil && item.Approval.ID == pending.ID:
			pending = nil
		}
	}
	return pending
}

func approvalPolicyLabel(policy string) string {
	switch policy {
	case "bypass_permissions":
		return "Full access"
	case "manual":
		return "Manual"
	case "auto", "":
		return "Auto"
	default:
		return policy
	}
}

func describeDecisions(decisions []session.DisplayApprovalDecision) string {
	approved, denied := 0, 0
	for _, decision := range decisions {
		if decision.Approved {
			approved++
		} else {
			denied++
		}
	}
	switch {
	case denied == 0:
		return fmt.Sprintf("✅ Approved %d", approved)
	case approved == 0:
		return fmt.Sprintf("🚫 Denied %d", denied)
	default:
		return fmt.Sprintf("Approved %d, denied %d", approved, denied)
	}
}

// forgetSteering drops pending steering inputs that joined turnID once its
// events arrive: the turn itself is then the stop target.
func (m *Model) forgetSteering(turnID string) {
	for id, joined := range m.steeringTurns {
		if joined == turnID {
			delete(m.steeringTurns, id)
			delete(m.pendingInputs, id)
		}
	}
}
