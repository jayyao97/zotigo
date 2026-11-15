package sessionlog

import (
	"testing"

	zotigosession "github.com/jayyao97/zotigo/core/session"
)

func TestPendingHumanRequestsResolveOnlyAfterLastBlockingRequest(t *testing.T) {
	requestItems := []zotigosession.DisplayItem{
		{Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-root"}},
		{Type: zotigosession.DisplayItemApprovalRequest, Approval: &zotigosession.DisplayApproval{ID: "approval", TurnID: "turn-child", Pending: []zotigosession.DisplayPendingApproval{{ToolCallID: "call", ToolName: "shell"}}}},
		{Type: zotigosession.DisplayItemInteractionRequest, Interaction: &zotigosession.DisplayInteraction{ID: "interaction", TurnID: "turn-child-2", IsBlocking: true}},
	}
	approvalResolved := zotigosession.DisplayItem{Type: zotigosession.DisplayItemApprovalDecision, Approval: &zotigosession.DisplayApproval{ID: "approval", TurnID: "turn-child"}}
	interactionResolved := zotigosession.DisplayItem{Type: zotigosession.DisplayItemInteractionResponse, Interaction: &zotigosession.DisplayInteraction{ID: "interaction", TurnID: "turn-child-2", IsBlocking: true, Status: InteractionStatusResolved}}
	for _, order := range [][]zotigosession.DisplayItem{{approvalResolved, interactionResolved}, {interactionResolved, approvalResolved}} {
		items := append([]zotigosession.DisplayItem(nil), requestItems...)
		if !HasPendingHumanRequest(items) {
			t.Fatal("two blocking requests were not detected")
		}
		items = append(items, order[0])
		if !HasPendingHumanRequest(items) {
			t.Fatal("resolving the first request resumed too early")
		}
		items = append(items, order[1])
		if HasPendingHumanRequest(items) {
			t.Fatal("final request did not release the pause")
		}
	}
}

func TestCompletedTurnDoesNotRetainStaleHumanRequest(t *testing.T) {
	items := []zotigosession.DisplayItem{
		{Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "old"}},
		{Type: zotigosession.DisplayItemApprovalRequest, Approval: &zotigosession.DisplayApproval{ID: "stale", TurnID: "old"}},
		{Type: zotigosession.DisplayItemTurnInterrupted, Turn: &zotigosession.DisplayTurn{ID: "old"}},
		{Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "current"}},
	}
	if HasPendingHumanRequest(items) {
		t.Fatal("stale request from completed turn blocked the current turn")
	}
}
