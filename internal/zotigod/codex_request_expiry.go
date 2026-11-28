package zotigod

import (
	"context"
	"strings"
	"time"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

func (h *handler) expireDisconnectedCodexRequests(ctx context.Context, sessionID string) error {
	items, exists, err := h.items.LoadItems(ctx, sessionID)
	if err != nil || !exists {
		return err
	}
	pendingApprovals := make(map[string]sessionlog.ApprovalRequest)
	pendingInteractions := make(map[string]wire.InteractionRequest)
	for _, item := range items {
		if item.Approval != nil && item.Approval.ID != "" {
			switch item.Type {
			case zotigosession.DisplayItemApprovalRequest:
				req, ok := sessionlog.ApprovalFromItems(sessionID, item.Approval.ID, []zotigosession.DisplayItem{item})
				if ok && isCodexApproval(req) {
					pendingApprovals[req.ID] = req
				}
			case zotigosession.DisplayItemApprovalDecision:
				delete(pendingApprovals, item.Approval.ID)
			}
		}
		if item.Interaction != nil && item.Interaction.ID != "" {
			switch item.Type {
			case zotigosession.DisplayItemInteractionRequest:
				req := sessionlog.InteractionRequestFromItem(sessionID, item)
				if req.Requester.Agent == "codex" && !req.ReplyAsMessage {
					pendingInteractions[req.ID] = req
				}
			case zotigosession.DisplayItemInteractionResponse:
				delete(pendingInteractions, item.Interaction.ID)
			}
		}
	}
	now := time.Now().UTC()
	for _, req := range pendingApprovals {
		decisions := make([]zotigosession.DisplayApprovalDecision, 0, len(req.Pending))
		for _, pending := range req.Pending {
			decisions = append(decisions, zotigosession.DisplayApprovalDecision{
				ToolCallID: pending.ToolCallID, Approved: false, Reason: "Codex request expired after worker disconnect",
			})
		}
		if _, err := h.items.AppendItem(ctx, sessionID, zotigosession.DisplayItem{
			Type: zotigosession.DisplayItemApprovalDecision, Turn: &zotigosession.DisplayTurn{ID: req.TurnID},
			Approval: &zotigosession.DisplayApproval{ID: req.ID, TurnID: req.TurnID, Decisions: decisions},
		}); err != nil {
			return err
		}
	}
	for _, req := range pendingInteractions {
		req.Status = sessionlog.InteractionStatusExpired
		req.ResolvedAt = &now
		if _, err := h.items.AppendItem(ctx, sessionID, sessionlog.InteractionItem(req, true)); err != nil {
			return err
		}
	}
	return nil
}

func isCodexApproval(req sessionlog.ApprovalRequest) bool {
	if len(req.Pending) == 0 {
		return false
	}
	for _, pending := range req.Pending {
		if !strings.HasPrefix(pending.Source, "codex") {
			return false
		}
	}
	return true
}
