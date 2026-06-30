package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/transport"
)

func (m *Model) submitApproval() (*Model, tea.Cmd) {
	m.approving = false
	approvalMsg := fmt.Sprintf("\n%s\n%s", m.pendingToolName, "✅ Approved")
	m.thinking = true
	m.turnStartTime = time.Now()

	cmd := func() tea.Msg {
		ch, err := m.agent.ApproveAndExecutePendingActions(m.ctx)
		if err != nil {
			return errMsg(err)
		}
		return streamReadyMsg(ch)
	}

	return m, tea.Batch(m.commitLine(approvalMsg), cmd)
}

func (m *Model) setPendingApprovals(actions []*agent.PendingAction) {
	m.pendingApprovals = append([]*agent.PendingAction(nil), actions...)
	m.approvalDecisions = make(map[string]bool, len(actions))
	m.approvalItemChoice = 0
	m.pendingToolName = formatPendingActions(actions)
}

func (m *Model) acceptCurrentApproval() (*Model, tea.Cmd) {
	if m.approvalItemChoice < 0 || m.approvalItemChoice >= len(m.pendingApprovals) {
		return m, nil
	}
	m.approvalDecisions[m.pendingApprovals[m.approvalItemChoice].ToolCallID] = true
	if m.approvalItemChoice < len(m.pendingApprovals)-1 {
		m.approvalItemChoice++
		m.approvalChoice = 0
		return m, nil
	}
	return m.submitApprovalBatch("")
}

func (m *Model) denyCurrentApproval(reason string) (*Model, tea.Cmd) {
	if m.approvalItemChoice < 0 || m.approvalItemChoice >= len(m.pendingApprovals) {
		return m, nil
	}
	m.approvalDecisions[m.pendingApprovals[m.approvalItemChoice].ToolCallID] = false
	return m.submitApprovalBatch(reason)
}

func (m *Model) submitApprovalBatch(reason string) (*Model, tea.Cmd) {
	m.approving = false
	if strings.TrimSpace(reason) == "" {
		reason = "User denied in TUI"
	}

	results := make([]transport.ApprovalResult, 0, len(m.pendingApprovals))
	denied := 0
	approved := 0
	for _, action := range m.pendingApprovals {
		approvedDecision, decided := m.approvalDecisions[action.ToolCallID]
		if !decided {
			continue
		}
		result := transport.ApprovalResult{ToolCallID: action.ToolCallID, Approved: approvedDecision}
		if !approvedDecision {
			result.Reason = reason
			denied++
		} else {
			approved++
		}
		results = append(results, result)
	}

	status := fmt.Sprintf("✅ Approved %d", approved)
	if denied > 0 {
		status = fmt.Sprintf("🚫 Denied %d/%d · skipped all pending calls", denied, len(m.pendingApprovals))
	}
	approvalMsg := fmt.Sprintf("\n%s\n%s", m.pendingToolName, status)
	m.thinking = true
	m.turnStartTime = time.Now()

	cmd := func() tea.Msg {
		ch, err := m.agent.ResolvePendingActions(m.ctx, deniedToolResultsFromTUI(results, m.pendingApprovals))
		if err != nil {
			return errMsg(err)
		}
		return streamReadyMsg(ch)
	}

	return m, tea.Batch(m.commitLine(approvalMsg), cmd)
}

func deniedToolResultsFromTUI(results []transport.ApprovalResult, pending []*agent.PendingAction) map[string]protocol.ToolResult {
	byID := make(map[string]*agent.PendingAction, len(pending))
	for _, action := range pending {
		byID[action.ToolCallID] = action
	}
	denied := make(map[string]protocol.ToolResult)
	for _, result := range results {
		if result.Approved {
			continue
		}
		action, ok := byID[result.ToolCallID]
		if !ok {
			continue
		}
		reason := result.Reason
		if reason == "" {
			reason = "User denied in TUI"
		}
		denied[result.ToolCallID] = protocol.ToolResult{
			ToolCallID: result.ToolCallID,
			ToolName:   action.Name,
			Type:       protocol.ToolResultTypeExecutionDenied,
			Reason:     reason,
			IsError:    true,
		}
	}
	return denied
}

func (m *Model) denyAndReturn(feedback string) (*Model, tea.Cmd) {
	m.approving = false

	reason := "User denied"
	if feedback != "" {
		reason = feedback
	}

	status := "🚫 Denied"
	if feedback != "" {
		status = fmt.Sprintf("🚫 Denied (feedback: %s)", feedback)
	}
	approvalMsg := fmt.Sprintf("\n%s\n%s", m.pendingToolName, status)

	snap := m.agent.Snapshot()
	var outputs []protocol.ToolResult
	for _, act := range snap.PendingActions {
		outputs = append(outputs, protocol.ToolResult{
			ToolCallID: act.ToolCallID,
			ToolName:   act.Name,
			Type:       protocol.ToolResultTypeExecutionDenied,
			Reason:     reason,
			IsError:    true,
		})
	}

	if feedback == "" {
		// Simple deny: back to input mode
		m.thinking = false
		m.appendDeniedToolResults(outputs)
		m.appendTurnInterrupted(reason)
		cmd := func() tea.Msg {
			ch, err := m.agent.SubmitToolOutputs(m.ctx, outputs)
			if err != nil {
				return errMsg(err)
			}
			// Drain the channel so agent settles, but don't continue the loop
			for range ch {
			}
			return denialSettledMsg{}
		}
		return m, tea.Batch(m.commitLine(approvalMsg), cmd)
	}

	// Deny with feedback: keep thinking, agent continues with user feedback
	m.thinking = true
	m.turnStartTime = time.Now()
	cmd := func() tea.Msg {
		ch, err := m.agent.SubmitToolOutputs(m.ctx, outputs)
		if err != nil {
			return errMsg(err)
		}
		return streamReadyMsg(ch)
	}
	return m, tea.Batch(m.commitLine(approvalMsg), cmd)
}
