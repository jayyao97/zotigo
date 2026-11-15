package zotigod

import (
	"fmt"
	"net/http"
	"strings"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
)

type submitApprovalDecisionRequest struct {
	Decisions []approvalDecisionRequestDTO `json:"decisions"`
	// StopTurn denies every pending call and interrupts the turn in the
	// same worker step, so the agent cannot continue past the denial. It
	// is the approval-time equivalent of pause, which is rejected while an
	// approval is pending.
	StopTurn bool `json:"stop_turn,omitempty"`
}

type approvalDecisionRequestDTO struct {
	ToolCallID   string `json:"tool_call_id"`
	Approved     *bool  `json:"approved"`
	Reason       string `json:"reason,omitempty"`
	ModifiedArgs string `json:"modified_args,omitempty"`
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
	if approval.Status != sessionlog.ApprovalStatusPending {
		writeAPIError(w, http.StatusConflict, "approval request already resolved")
		return
	}
	if err := sessionlog.ValidateApprovalDecisions(approval.Pending, decisions); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.StopTurn {
		for _, decision := range decisions {
			if decision.Approved {
				writeAPIError(w, http.StatusBadRequest, "stop_turn requires every decision to deny")
				return
			}
		}
	}

	resolved, err := h.workers.SubmitApproval(r.Context(), id, approval.ID, decisions, req.StopTurn)
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
			return nil, fmt.Errorf("%w: approved is required", sessionlog.ErrInvalidApprovalRequest)
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

func (h *handler) loadApproval(r *http.Request, sessionID string, approvalID string) (sessionlog.ApprovalRequest, bool, error) {
	_, inRegistry := h.registry.Get(sessionID)
	items, inStore, err := h.items.LoadItems(r.Context(), sessionID)
	if err != nil {
		return sessionlog.ApprovalRequest{}, false, err
	}
	if !inRegistry && !inStore {
		return sessionlog.ApprovalRequest{}, false, nil
	}
	approval, ok := sessionlog.ApprovalFromItems(sessionID, approvalID, items)
	return approval, ok, nil
}
