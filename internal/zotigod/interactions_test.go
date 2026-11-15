package zotigod

import (
	"context"
	"testing"
	"time"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
)

// newCodexRequestExpiryTestStore seeds the stored Codex session the expiry
// handler reads; it mirrors the store half of the Codex worker tests'
// newCodexInputTestRuntime without building a worker runtime.
func newCodexRequestExpiryTestStore(t *testing.T, sessionID string) *zotigosession.FileStore {
	t.Helper()
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: sessionID, Agent: "codex", ConversationID: "thread-1", Model: "gpt-test", CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestCodexReconnectExpiresUnroutableApprovalAndInteraction(t *testing.T) {
	store := newCodexRequestExpiryTestStore(t, "session-reconnect")
	for _, item := range []zotigosession.DisplayItem{
		{Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn"}},
		{Type: zotigosession.DisplayItemApprovalRequest, Approval: &zotigosession.DisplayApproval{ID: "approval", TurnID: "child", Pending: []zotigosession.DisplayPendingApproval{{ToolCallID: "call", ToolName: "shell", Source: "codex_subagent:child"}}}},
		{Type: zotigosession.DisplayItemInteractionRequest, Interaction: &zotigosession.DisplayInteraction{ID: "interaction", TurnID: "child-2", IsBlocking: true, Requester: &zotigosession.DisplayInteractionRequester{Agent: "codex"}}},
	} {
		if _, err := store.AppendDisplayItem(context.Background(), "session-reconnect", item); err != nil {
			t.Fatal(err)
		}
	}
	h := handler{items: sessionlog.StoredItemSource{Store: store}}
	if err := h.expireDisconnectedCodexRequests(context.Background(), "session-reconnect"); err != nil {
		t.Fatal(err)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-reconnect")
	if err != nil || len(items) != 5 || items[3].Type != zotigosession.DisplayItemApprovalDecision || items[4].Type != zotigosession.DisplayItemInteractionResponse || sessionlog.HasPendingHumanRequest(items) {
		t.Fatalf("reconnected items = %#v, err=%v", items, err)
	}
}
