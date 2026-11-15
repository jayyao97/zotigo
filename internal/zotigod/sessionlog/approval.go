package sessionlog

import (
	"errors"
	"fmt"
	"strings"
	"time"

	zotigosession "github.com/jayyao97/zotigo/core/session"
)

const (
	ApprovalStatusPending  = "pending"
	ApprovalStatusResolved = "resolved"
)

var ErrInvalidApprovalRequest = errors.New("invalid approval request")

type ApprovalRequest struct {
	ID         string
	SessionID  string
	TurnID     string
	Status     string
	Pending    []zotigosession.DisplayPendingApproval
	Decisions  []zotigosession.DisplayApprovalDecision
	CreatedAt  time.Time
	ResolvedAt *time.Time
}

func NewApprovalRequest(sessionID string, turnID string, pending []zotigosession.DisplayPendingApproval) (ApprovalRequest, error) {
	if err := validatePendingApprovals(turnID, pending); err != nil {
		return ApprovalRequest{}, err
	}

	return ApprovalRequest{
		ID:        NewID("apr"),
		SessionID: sessionID,
		TurnID:    strings.TrimSpace(turnID),
		Status:    ApprovalStatusPending,
		Pending:   CopyPendingApprovals(pending),
		CreatedAt: time.Now().UTC(),
	}, nil
}

func HasPendingApproval(items []zotigosession.DisplayItem) bool {
	turnID := LastOpenTurnID(items)
	return turnID != "" && HasPendingApprovalForTurn(items, turnID)
}

func HasPendingHumanRequest(items []zotigosession.DisplayItem) bool {
	turnID := LastOpenTurnID(items)
	if turnID == "" {
		return false
	}
	start := 0
	for index, item := range items {
		switch item.Type {
		case zotigosession.DisplayItemTurnCompleted, zotigosession.DisplayItemTurnFailed, zotigosession.DisplayItemTurnInterrupted:
			start = index + 1
		}
	}
	for index := len(items) - 1; index >= 0; index-- {
		if items[index].Type == zotigosession.DisplayItemTurnStarted && items[index].Turn != nil && items[index].Turn.ID == turnID {
			if index > start {
				start = index
			}
			break
		}
	}
	pendingApprovals := make(map[string]struct{})
	pendingInteractions := make(map[string]struct{})
	for _, item := range items[start:] {
		if item.Approval != nil {
			key := item.Approval.ID
			if key == "" {
				key = "turn:" + item.Approval.TurnID
			}
			switch item.Type {
			case zotigosession.DisplayItemApprovalRequest:
				pendingApprovals[key] = struct{}{}
			case zotigosession.DisplayItemApprovalDecision:
				delete(pendingApprovals, key)
			}
		}
		if item.Interaction == nil || !item.Interaction.IsBlocking || item.Interaction.ID == "" {
			continue
		}
		switch item.Type {
		case zotigosession.DisplayItemInteractionRequest:
			pendingInteractions[item.Interaction.ID] = struct{}{}
		case zotigosession.DisplayItemInteractionResponse:
			delete(pendingInteractions, item.Interaction.ID)
		}
	}
	return len(pendingApprovals) > 0 || len(pendingInteractions) > 0
}

func ApprovalFromItems(sessionID string, approvalID string, items []zotigosession.DisplayItem) (ApprovalRequest, bool) {
	var req ApprovalRequest
	for _, item := range items {
		if item.Approval == nil || item.Approval.ID != approvalID {
			continue
		}

		switch item.Type {
		case zotigosession.DisplayItemApprovalRequest:
			req = ApprovalRequest{
				ID:        item.Approval.ID,
				SessionID: sessionID,
				TurnID:    item.Approval.TurnID,
				Status:    ApprovalStatusPending,
				Pending:   CopyPendingApprovals(item.Approval.Pending),
				CreatedAt: item.CreatedAt,
			}
		case zotigosession.DisplayItemApprovalDecision:
			if req.ID == "" {
				continue
			}
			resolvedAt := item.CreatedAt
			req.Status = ApprovalStatusResolved
			req.Decisions = CopyApprovalDecisions(item.Approval.Decisions)
			req.ResolvedAt = &resolvedAt
		}
	}
	return req, req.ID != ""
}

func ResolvedApprovalFromDecision(req ApprovalRequest, decisions []zotigosession.DisplayApprovalDecision, resolvedAt time.Time) ApprovalRequest {
	req.Status = ApprovalStatusResolved
	req.Decisions = CopyApprovalDecisions(decisions)
	req.ResolvedAt = &resolvedAt
	return req
}

func validatePendingApprovals(turnID string, pending []zotigosession.DisplayPendingApproval) error {
	if strings.TrimSpace(turnID) == "" {
		return fmt.Errorf("%w: turn_id is required", ErrInvalidApprovalRequest)
	}
	if len(pending) == 0 {
		return fmt.Errorf("%w: pending approvals are required", ErrInvalidApprovalRequest)
	}
	seen := make(map[string]struct{}, len(pending))
	for _, item := range pending {
		toolCallID := strings.TrimSpace(item.ToolCallID)
		if toolCallID == "" {
			return fmt.Errorf("%w: tool_call_id is required", ErrInvalidApprovalRequest)
		}
		if strings.TrimSpace(item.ToolName) == "" {
			return fmt.Errorf("%w: tool_name is required", ErrInvalidApprovalRequest)
		}
		if _, ok := seen[toolCallID]; ok {
			return fmt.Errorf("%w: duplicate tool_call_id %q", ErrInvalidApprovalRequest, toolCallID)
		}
		seen[toolCallID] = struct{}{}
	}
	return nil
}

func ValidateApprovalDecisions(pending []zotigosession.DisplayPendingApproval, decisions []zotigosession.DisplayApprovalDecision) error {
	if len(decisions) != len(pending) {
		return fmt.Errorf("%w: expected %d decisions, got %d", ErrInvalidApprovalRequest, len(pending), len(decisions))
	}

	expected := make(map[string]struct{}, len(pending))
	for _, item := range pending {
		expected[item.ToolCallID] = struct{}{}
	}

	seen := make(map[string]struct{}, len(decisions))
	for _, decision := range decisions {
		toolCallID := strings.TrimSpace(decision.ToolCallID)
		if toolCallID == "" {
			return fmt.Errorf("%w: tool_call_id is required", ErrInvalidApprovalRequest)
		}
		if _, ok := expected[toolCallID]; !ok {
			return fmt.Errorf("%w: unknown tool_call_id %q", ErrInvalidApprovalRequest, toolCallID)
		}
		if _, ok := seen[toolCallID]; ok {
			return fmt.Errorf("%w: duplicate tool_call_id %q", ErrInvalidApprovalRequest, toolCallID)
		}
		seen[toolCallID] = struct{}{}
	}
	return nil
}

func CopyPendingApprovals(items []zotigosession.DisplayPendingApproval) []zotigosession.DisplayPendingApproval {
	if len(items) == 0 {
		return nil
	}
	copied := make([]zotigosession.DisplayPendingApproval, len(items))
	copy(copied, items)
	for idx := range copied {
		copied[idx].ToolCallID = strings.TrimSpace(copied[idx].ToolCallID)
		copied[idx].ToolName = strings.TrimSpace(copied[idx].ToolName)
	}
	return copied
}

func CopyApprovalDecisions(items []zotigosession.DisplayApprovalDecision) []zotigosession.DisplayApprovalDecision {
	if len(items) == 0 {
		return nil
	}
	copied := make([]zotigosession.DisplayApprovalDecision, len(items))
	copy(copied, items)
	for idx := range copied {
		copied[idx].ToolCallID = strings.TrimSpace(copied[idx].ToolCallID)
	}
	return copied
}
