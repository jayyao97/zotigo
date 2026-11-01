package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigodclient"
)

// approvalFailedMsg reports a decision the service did not record, so the
// prompt becomes answerable again.
type approvalFailedMsg string

func (m *Model) showApproval(approval *session.DisplayApproval) tea.Cmd {
	copied := *approval
	m.approval = &copied
	m.approving = true
	m.approvalSubmitted = false
	m.approvalChoice = 0
	m.approvalItemChoice = 0
	m.approvalDecisions = make(map[string]bool, len(copied.Pending))
	m.pendingToolName = formatPendingApprovals(copied.Pending)
	return nil
}

func (m *Model) clearApproval() {
	m.approving = false
	m.approval = nil
	m.approvalDecisions = nil
	m.approvalSubmitted = false
	m.pendingToolName = ""
}

// handleApprovalDecision closes the approval prompt once the service has
// durably recorded a decision, whichever client made it.
func (m *Model) handleApprovalDecision(item session.DisplayItem) tea.Cmd {
	if item.Approval == nil || m.approval == nil || item.Approval.ID != m.approval.ID {
		return nil
	}
	ours := m.approvalSubmitted
	header := m.pendingToolName
	m.clearApproval()
	status := describeDecisions(item.Approval.Decisions)
	if !ours {
		status += " (decided by another client)"
	}
	return m.commitLine(fmt.Sprintf("\n%s\n%s", header, status))
}

func (m *Model) currentPending() *session.DisplayPendingApproval {
	if m.approval == nil || m.approvalItemChoice < 0 || m.approvalItemChoice >= len(m.approval.Pending) {
		return nil
	}
	return &m.approval.Pending[m.approvalItemChoice]
}

func (m *Model) acceptApproval() (*Model, tea.Cmd) {
	current := m.currentPending()
	if current == nil || m.approvalSubmitted {
		return m, nil
	}
	m.approvalDecisions[current.ToolCallID] = true
	if m.approvalItemChoice < len(m.approval.Pending)-1 {
		m.approvalItemChoice++
		m.approvalChoice = 0
		return m, nil
	}
	return m, m.submitApprovalDecisions("", false)
}

// denyApproval denies the current call and every remaining one. Without
// feedback the service stops the turn together with the denial, matching
// the previous "deny returns to the prompt" behavior; with feedback the
// agent continues and sees the reason. A stop is only possible when nothing
// in the batch was approved.
func (m *Model) denyApproval(feedback string) (*Model, tea.Cmd) {
	current := m.currentPending()
	if current == nil || m.approvalSubmitted {
		return m, nil
	}
	for _, pending := range m.approval.Pending[m.approvalItemChoice:] {
		m.approvalDecisions[pending.ToolCallID] = false
	}
	stop := strings.TrimSpace(feedback) == ""
	for _, approved := range m.approvalDecisions {
		if approved {
			stop = false
		}
	}
	return m, m.submitApprovalDecisions(feedback, stop)
}

func (m *Model) submitApprovalDecisions(feedback string, stopTurn bool) tea.Cmd {
	reason := strings.TrimSpace(feedback)
	if reason == "" {
		reason = "User denied in TUI"
	}
	decisions := make([]zotigodclient.ApprovalDecision, 0, len(m.approval.Pending))
	for _, pending := range m.approval.Pending {
		approved := m.approvalDecisions[pending.ToolCallID]
		decision := zotigodclient.ApprovalDecision{ToolCallID: pending.ToolCallID, Approved: approved}
		if !approved {
			decision.Reason = reason
		}
		decisions = append(decisions, decision)
	}
	m.approvalSubmitted = true
	service, sessionID, approvalID, ctx := m.service, m.session.ID, m.approval.ID, m.ctx
	return func() tea.Msg {
		if err := service.DecideApproval(ctx, sessionID, approvalID, decisions, stopTurn); err != nil {
			if zotigodclient.IsStatus(err, 409) {
				return serviceNoticeMsg(timingStyle.Render("Approval was already resolved by another client."))
			}
			return approvalFailedMsg(errorStyle.Render("✗ ") + "Approval not recorded: " + err.Error())
		}
		return nil
	}
}
