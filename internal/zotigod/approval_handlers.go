package zotigod

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	zotigosession "github.com/jayyao97/zotigo/core/session"
)

type createApprovalRequest struct {
	TurnID  string                      `json:"turn_id"`
	Pending []pendingApprovalRequestDTO `json:"pending"`
}

type pendingApprovalRequestDTO struct {
	ToolCallID       string `json:"tool_call_id"`
	ToolName         string `json:"tool_name"`
	Arguments        string `json:"arguments,omitempty"`
	Description      string `json:"description,omitempty"`
	Reason           string `json:"reason,omitempty"`
	RiskLevel        string `json:"risk_level,omitempty"`
	Source           string `json:"source,omitempty"`
	RequiresSnapshot bool   `json:"requires_snapshot,omitempty"`
}

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

func (h *handler) handleApprovalCreate(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	h.approvals.mu.Lock()
	defer h.approvals.mu.Unlock()

	session, ok := h.registry.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if session.State != SessionStateRunning {
		http.Error(w, "approval request requires a running session", http.StatusConflict)
		return
	}

	var req createApprovalRequest
	if err := readRequiredJSON(r, &req); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}

	approval, err := h.approvals.Create(id, req.TurnID, pendingApprovalRequests(req.Pending))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	item, err := h.items.AppendItem(r.Context(), id, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemApprovalRequest,
		Approval: &zotigosession.DisplayApproval{
			ID:      approval.ID,
			TurnID:  approval.TurnID,
			Pending: approval.Pending,
		},
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("append approval request item: %v", err), http.StatusInternalServerError)
		return
	}
	approval.CreatedAt = item.CreatedAt

	_, _ = h.items.AppendItem(r.Context(), id, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnPaused,
		Turn: &zotigosession.DisplayTurn{
			ID:     approval.TurnID,
			Reason: "need_approval",
		},
	})

	if session, err = h.registry.Pause(id); err != nil {
		writeJSON(w, http.StatusCreated, publicApprovalRequest(approval))
		return
	}
	writeJSON(w, http.StatusCreated, publicApprovalRequest(approval))
}

func (h *handler) handleApprovalGet(w http.ResponseWriter, r *http.Request, id string, approvalID string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	approval, ok, err := h.loadApproval(r, id, approvalID)
	if err != nil {
		http.Error(w, fmt.Sprintf("load approval: %v", err), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, publicApprovalRequest(approval))
}

func (h *handler) handleApprovalDecision(w http.ResponseWriter, r *http.Request, id string, approvalID string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req submitApprovalDecisionRequest
	if err := readRequiredJSON(r, &req); err != nil {
		http.Error(w, fmt.Sprintf("decode request: %v", err), http.StatusBadRequest)
		return
	}
	decisions, err := approvalDecisionRequests(req.Decisions)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	h.approvals.mu.Lock()
	defer h.approvals.mu.Unlock()

	session, inRegistry := h.registry.Get(id)
	if inRegistry && session.State != SessionStatePaused {
		http.Error(w, "approval decision requires a paused session", http.StatusConflict)
		return
	}

	approval, ok, err := h.loadApproval(r, id, approvalID)
	if err != nil {
		http.Error(w, fmt.Sprintf("load approval: %v", err), http.StatusInternalServerError)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	if approval.Status != approvalStatusPending {
		http.Error(w, "approval request already resolved", http.StatusConflict)
		return
	}
	if err := validateApprovalDecisions(approval.Pending, decisions); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	item, err := h.items.AppendItem(r.Context(), id, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemApprovalDecision,
		Approval: &zotigosession.DisplayApproval{
			ID:        approval.ID,
			TurnID:    approval.TurnID,
			Decisions: decisions,
		},
	})
	if err != nil {
		http.Error(w, fmt.Sprintf("append approval decision item: %v", err), http.StatusInternalServerError)
		return
	}
	approval = resolvedApprovalFromDecision(approval, decisions, item.CreatedAt)

	if inRegistry {
		_, _ = h.registry.ResumeAfterApproval(id)
	}
	writeJSON(w, http.StatusOK, publicApprovalRequest(approval))
}

func pendingApprovalRequests(items []pendingApprovalRequestDTO) []zotigosession.DisplayPendingApproval {
	pending := make([]zotigosession.DisplayPendingApproval, 0, len(items))
	for _, item := range items {
		pending = append(pending, zotigosession.DisplayPendingApproval{
			ToolCallID:       strings.TrimSpace(item.ToolCallID),
			ToolName:         strings.TrimSpace(item.ToolName),
			Arguments:        item.Arguments,
			Description:      item.Description,
			Reason:           item.Reason,
			RiskLevel:        item.RiskLevel,
			Source:           item.Source,
			RequiresSnapshot: item.RequiresSnapshot,
		})
	}
	return pending
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
