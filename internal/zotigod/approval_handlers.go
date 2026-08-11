package zotigod

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	zotigosession "github.com/jayyao97/zotigo/core/session"
)

type submitApprovalDecisionRequest struct {
	Decisions []approvalDecisionRequestDTO `json:"decisions"`
}

type approvalDecisionRequestDTO struct {
	ToolCallID   string `json:"tool_call_id"`
	Approved     *bool  `json:"approved"`
	Reason       string `json:"reason,omitempty"`
	ModifiedArgs string `json:"modified_args,omitempty"`
}

type approvalRequestResponse struct {
	ID         string                         `json:"id"`
	SessionID  string                         `json:"session_id"`
	TurnID     string                         `json:"turn_id"`
	Status     string                         `json:"status"`
	Pending    []itemPendingApprovalResponse  `json:"pending"`
	Decisions  []itemApprovalDecisionResponse `json:"decisions,omitempty"`
	CreatedAt  time.Time                      `json:"created_at"`
	ResolvedAt *time.Time                     `json:"resolved_at,omitempty"`
}

func (h *handler) handleApprovalDecision(w http.ResponseWriter, r *http.Request, id string, approvalID string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req submitApprovalDecisionRequest
	if err := readRequiredJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}
	decisions, err := approvalDecisionRequests(req.Decisions)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	unlock := h.approvalOps.lock(id)
	defer unlock()

	approval, ok, err := h.loadApproval(r, id, approvalID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load approval: %v", err))
		return
	}
	if !ok {
		writeAPIError(w, http.StatusNotFound, "approval request not found")
		return
	}
	if _, inRegistry := h.registry.Get(id); !inRegistry {
		h.writeSessionNotLiveOrMissing(w, r.Context(), id, "approval decision requires a live session")
		return
	}
	if approval.Status != approvalStatusPending {
		writeAPIError(w, http.StatusConflict, "approval request already resolved")
		return
	}
	if err := validateApprovalDecisions(approval.Pending, decisions); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	resolved, err := h.workers.SubmitApproval(r.Context(), id, approval.ID, decisions)
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, fmt.Sprintf("submit approval decision: %v", err))
		return
	}

	writeAPIJSON(w, http.StatusOK, resolved)
}

func approvalDecisionRequests(items []approvalDecisionRequestDTO) ([]zotigosession.DisplayApprovalDecision, error) {
	decisions := make([]zotigosession.DisplayApprovalDecision, 0, len(items))
	for _, item := range items {
		if item.Approved == nil {
			return nil, fmt.Errorf("%w: approved is required", errInvalidApprovalRequest)
		}
		decisions = append(decisions, zotigosession.DisplayApprovalDecision{
			ToolCallID:   strings.TrimSpace(item.ToolCallID),
			Approved:     *item.Approved,
			Reason:       item.Reason,
			ModifiedArgs: item.ModifiedArgs,
		})
	}
	return decisions, nil
}

func publicApprovalRequest(approval approvalRequest) approvalRequestResponse {
	return approvalRequestResponse{
		ID:         approval.ID,
		SessionID:  approval.SessionID,
		TurnID:     approval.TurnID,
		Status:     approval.Status,
		Pending:    publicDisplayPendingApprovals(approval.Pending),
		Decisions:  publicDisplayApprovalDecisions(approval.Decisions),
		CreatedAt:  approval.CreatedAt,
		ResolvedAt: approval.ResolvedAt,
	}
}

func (h *handler) loadApproval(r *http.Request, sessionID string, approvalID string) (approvalRequest, bool, error) {
	_, inRegistry := h.registry.Get(sessionID)
	items, inStore, err := h.items.LoadItems(r.Context(), sessionID)
	if err != nil {
		return approvalRequest{}, false, err
	}
	if !inRegistry && !inStore {
		return approvalRequest{}, false, nil
	}
	approval, ok := approvalFromDisplayItems(sessionID, approvalID, items)
	return approval, ok, nil
}
