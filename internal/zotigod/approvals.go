package zotigod

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	zotigosession "github.com/jayyao97/zotigo/core/session"
)

const (
	approvalStatusPending  = "pending"
	approvalStatusResolved = "resolved"
)

var errInvalidApprovalRequest = errors.New("invalid approval request")

type approvalRegistry struct {
	mu sync.Mutex
}

type approvalRequest struct {
	ID         string
	SessionID  string
	TurnID     string
	Status     string
	Pending    []zotigosession.DisplayPendingApproval
	Decisions  []zotigosession.DisplayApprovalDecision
	CreatedAt  time.Time
	ResolvedAt *time.Time
}

func newApprovalRegistry() *approvalRegistry {
	return &approvalRegistry{}
}

func (r *approvalRegistry) Create(sessionID string, turnID string, pending []zotigosession.DisplayPendingApproval) (approvalRequest, error) {
	if err := validatePendingApprovals(turnID, pending); err != nil {
		return approvalRequest{}, err
	}

	return approvalRequest{
		ID:        newApprovalID(),
		SessionID: sessionID,
		TurnID:    strings.TrimSpace(turnID),
		Status:    approvalStatusPending,
		Pending:   copyPendingApprovals(pending),
		CreatedAt: time.Now().UTC(),
	}, nil
}

func approvalFromDisplayItems(sessionID string, approvalID string, items []zotigosession.DisplayItem) (approvalRequest, bool) {
	var req approvalRequest
	for _, item := range items {
		if item.Approval == nil || item.Approval.ID != approvalID {
			continue
		}

		switch item.Type {
		case zotigosession.DisplayItemApprovalRequest:
			req = approvalRequest{
				ID:        item.Approval.ID,
				SessionID: sessionID,
				TurnID:    item.Approval.TurnID,
				Status:    approvalStatusPending,
				Pending:   copyPendingApprovals(item.Approval.Pending),
				CreatedAt: item.CreatedAt,
			}
		case zotigosession.DisplayItemApprovalDecision:
			if req.ID == "" {
				continue
			}
			resolvedAt := item.CreatedAt
			req.Status = approvalStatusResolved
			req.Decisions = copyApprovalDecisions(item.Approval.Decisions)
			req.ResolvedAt = &resolvedAt
		}
	}
	return req, req.ID != ""
}

func resolvedApprovalFromDecision(req approvalRequest, decisions []zotigosession.DisplayApprovalDecision, resolvedAt time.Time) approvalRequest {
	req.Status = approvalStatusResolved
	req.Decisions = copyApprovalDecisions(decisions)
	req.ResolvedAt = &resolvedAt
	return req
}

func validatePendingApprovals(turnID string, pending []zotigosession.DisplayPendingApproval) error {
	if strings.TrimSpace(turnID) == "" {
		return fmt.Errorf("%w: turn_id is required", errInvalidApprovalRequest)
	}
	if len(pending) == 0 {
		return fmt.Errorf("%w: pending approvals are required", errInvalidApprovalRequest)
	}
	seen := make(map[string]struct{}, len(pending))
	for _, item := range pending {
		toolCallID := strings.TrimSpace(item.ToolCallID)
		if toolCallID == "" {
			return fmt.Errorf("%w: tool_call_id is required", errInvalidApprovalRequest)
		}
		if strings.TrimSpace(item.ToolName) == "" {
			return fmt.Errorf("%w: tool_name is required", errInvalidApprovalRequest)
		}
		if _, ok := seen[toolCallID]; ok {
			return fmt.Errorf("%w: duplicate tool_call_id %q", errInvalidApprovalRequest, toolCallID)
		}
		seen[toolCallID] = struct{}{}
	}
	return nil
}

func validateApprovalDecisions(pending []zotigosession.DisplayPendingApproval, decisions []zotigosession.DisplayApprovalDecision) error {
	if len(decisions) != len(pending) {
		return fmt.Errorf("%w: expected %d decisions, got %d", errInvalidApprovalRequest, len(pending), len(decisions))
	}

	expected := make(map[string]struct{}, len(pending))
	for _, item := range pending {
		expected[item.ToolCallID] = struct{}{}
	}

	seen := make(map[string]struct{}, len(decisions))
	for _, decision := range decisions {
		toolCallID := strings.TrimSpace(decision.ToolCallID)
		if toolCallID == "" {
			return fmt.Errorf("%w: tool_call_id is required", errInvalidApprovalRequest)
		}
		if _, ok := expected[toolCallID]; !ok {
			return fmt.Errorf("%w: unknown tool_call_id %q", errInvalidApprovalRequest, toolCallID)
		}
		if _, ok := seen[toolCallID]; ok {
			return fmt.Errorf("%w: duplicate tool_call_id %q", errInvalidApprovalRequest, toolCallID)
		}
		seen[toolCallID] = struct{}{}
	}
	return nil
}

func copyPendingApprovals(items []zotigosession.DisplayPendingApproval) []zotigosession.DisplayPendingApproval {
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

func copyApprovalDecisions(items []zotigosession.DisplayApprovalDecision) []zotigosession.DisplayApprovalDecision {
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

func newApprovalID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err == nil {
		return "apr_" + hex.EncodeToString(buf[:])
	}
	return fmt.Sprintf("apr_%d", time.Now().UnixNano())
}
