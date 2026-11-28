package zotigod

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/testutil/sessionlogtest"
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
		{Type: zotigosession.DisplayItemInteractionRequest, Interaction: &zotigosession.DisplayInteraction{ID: "followup", ReplyAsMessage: true, Requester: &zotigosession.DisplayInteractionRequester{Agent: "codex"}}},
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
	if err != nil || len(items) != 6 || items[4].Type != zotigosession.DisplayItemApprovalDecision || items[5].Type != zotigosession.DisplayItemInteractionResponse || items[5].Interaction.ID != "interaction" || sessionlog.HasPendingHumanRequest(items) {
		t.Fatalf("reconnected items = %#v, err=%v", items, err)
	}
}

func TestFollowupQuestionAnswerSteersOnlyOriginatingTurn(t *testing.T) {
	source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()
	id := createSession(t, handler).ID
	startSession(t, handler, id)
	worker := dialWorker(t, server, id)
	defer worker.Close()
	inputs := serveTestWorkerInputs(t, worker, source, id)
	appendTurnStarted(t, source, id, "first")
	questionID := sessionlog.FollowupQuestionPrefix + "call-1"
	for _, item := range []zotigosession.DisplayItem{
		{ID: "call-1", Type: zotigosession.DisplayItemInteractionRequest, Interaction: &zotigosession.DisplayInteraction{ID: questionID, ItemID: "call-1", TurnID: "first", Requester: &zotigosession.DisplayInteractionRequester{Agent: "codex"}, Kind: "user_input", ReplyAsMessage: true, Questions: []zotigosession.DisplayInteractionQuestion{{ID: "place", Question: "Where?", Options: []zotigosession.DisplayInteractionOption{{Label: "Sea"}}}, {ID: "why", Question: "Why?"}}}},
		{ID: "call-late", Type: zotigosession.DisplayItemInteractionRequest, Interaction: &zotigosession.DisplayInteraction{ID: "codex-question-late", TurnID: "first", ItemID: "call-late", ReplyAsMessage: true, Questions: []zotigosession.DisplayInteractionQuestion{{ID: "place", Question: "Where?"}}}},
	} {
		if _, err := source.AppendItem(t.Context(), id, item); err != nil {
			t.Fatal(err)
		}
	}
	body := `{"answers":{"place":["Sea"],"why":["user_note: Waves"]}}`
	post := func(body string, want int) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+id+"/interactions/"+questionID, strings.NewReader(body)))
		if rec.Code != want {
			t.Fatalf("answer: %d %s", rec.Code, rec.Body.String())
		}
	}
	post(`{"answers":{"place":["Train"],"why":[]}}`, 400)
	post(body, 200)
	input := <-inputs
	if !input.SteeringOnly || input.ExpectedTurnID != "first" || input.Command.Steering == nil || input.Command.Steering.Text != "Where?\nSea\n\nWhy?\nWaves" {
		t.Fatalf("follow-up = %+v", input)
	}
	if _, err := source.AppendItem(t.Context(), id, zotigosession.DisplayItem{Type: zotigosession.DisplayItemTurnCompleted, Turn: &zotigosession.DisplayTurn{ID: "first"}}); err != nil {
		t.Fatal(err)
	}
	post(body, 200)
	post(`{"answers":{"place":["Sea"],"why":[]}}`, 409)
	items, _, err := source.LoadItems(t.Context(), id)
	if err != nil || len(items) != 6 || !items[4].Interaction.ReplyAsMessage || items[4].Interaction.Status != "resolved" {
		t.Fatalf("after retry: %+v, %v", items, err)
	}
	questionID = "codex-question-late"
	post(`{"answers":{"place":["user_note: Too late"]}}`, 409)
	appendTurnStarted(t, source, id, "another")
	post(`{"answers":{"place":["user_note: Wrong turn"]}}`, 409)
	select {
	case extra := <-inputs:
		t.Fatalf("unexpected late input: %+v", extra)
	default:
	}

}
