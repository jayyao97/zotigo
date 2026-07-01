package zotigod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

type sessionListResponse struct {
	Sessions []Session `json:"sessions"`
}

type fakeDisplayItemSource struct {
	items      map[string][]zotigosession.DisplayItem
	err        error
	appendErr  func(sessionID string, item zotigosession.DisplayItem) error
	appendHook func(sessionID string, item zotigosession.DisplayItem)
}

func (s *fakeDisplayItemSource) LoadItems(_ context.Context, sessionID string) ([]zotigosession.DisplayItem, bool, error) {
	if s.err != nil {
		return nil, false, s.err
	}
	items, ok := s.items[sessionID]
	return items, ok, nil
}

func (s *fakeDisplayItemSource) AppendItem(_ context.Context, sessionID string, item zotigosession.DisplayItem) (zotigosession.DisplayItem, error) {
	if s.err != nil {
		return zotigosession.DisplayItem{}, s.err
	}
	if s.items == nil {
		s.items = make(map[string][]zotigosession.DisplayItem)
	}
	if s.appendErr != nil {
		if err := s.appendErr(sessionID, item); err != nil {
			return zotigosession.DisplayItem{}, err
		}
	}
	next := uint64(len(s.items[sessionID]) + 1)
	item.Sequence = next
	if item.ID == "" {
		item.ID = fmt.Sprintf("item_%s_%d", sessionID, next)
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = time.Now().UTC()
	}
	s.items[sessionID] = append(s.items[sessionID], item)
	if s.appendHook != nil {
		s.appendHook(sessionID, item)
	}
	return item, nil
}

func createSession(t *testing.T, handler http.Handler) Session {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d", http.StatusCreated, rec.Code)
	}
	var session Session
	if err := sonic.Unmarshal(rec.Body.Bytes(), &session); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	return session
}

func TestHealth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	NewHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("expected JSON content type, got %q", got)
	}

	var body map[string]string
	if err := sonic.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("unexpected health response: %#v", body)
	}
}

func TestSessionsCreateAndList(t *testing.T) {
	handler := NewHandler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	var list sessionListResponse
	if err := sonic.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode initial list response: %v", err)
	}
	if len(list.Sessions) != 0 {
		t.Fatalf("expected no sessions, got %#v", list.Sessions)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions", nil))

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d", http.StatusCreated, rec.Code)
	}
	var created Session
	if err := sonic.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.ID == "" {
		t.Fatal("expected session id")
	}
	if created.State != SessionStateCreated {
		t.Fatalf("expected state %q, got %q", SessionStateCreated, created.State)
	}
	if created.CreatedAt.IsZero() {
		t.Fatal("expected created_at")
	}
	if created.StartedAt != nil {
		t.Fatalf("expected no started_at, got %v", created.StartedAt)
	}
	if created.EndedAt != nil {
		t.Fatalf("expected no ended_at, got %v", created.EndedAt)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	if err := sonic.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].ID != created.ID {
		t.Fatalf("unexpected sessions: %#v", list.Sessions)
	}
}

func TestSessionsGetByID(t *testing.T) {
	handler := NewHandler()
	created := createSession(t, handler)

	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/sessions/"+created.ID, nil))

	if getRec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, getRec.Code)
	}
	var got Session
	if err := sonic.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if got.ID != created.ID || got.State != SessionStateCreated {
		t.Fatalf("unexpected session: %#v", got)
	}
}

func TestSessionItemsRejectMissingSession(t *testing.T) {
	handler := newHandler(newSessionRegistry(), &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/missing/items", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d", http.StatusNotFound, rec.Code)
	}
}

func TestSessionItemsDefaultLimitReturnsRecentItemsAscending(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	created := createSession(t, handler)
	source.items[created.ID] = makeDisplayItems(60)

	resp := getItems(t, handler, "/sessions/"+created.ID+"/items")

	if len(resp.Items) != defaultItemsLimit {
		t.Fatalf("expected %d items, got %d", defaultItemsLimit, len(resp.Items))
	}
	if resp.Items[0].Sequence != 11 || resp.Items[49].Sequence != 60 {
		t.Fatalf("expected recent 11..60, got %d..%d", resp.Items[0].Sequence, resp.Items[49].Sequence)
	}
	if resp.PrevCursor != "11" || resp.NextCursor != "" || !resp.HasMore {
		t.Fatalf("unexpected cursors: %#v", resp)
	}
}

func TestSessionItemsLimitMax(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	created := createSession(t, handler)
	source.items[created.ID] = makeDisplayItems(210)

	resp := getItems(t, handler, "/sessions/"+created.ID+"/items?limit=200")

	if len(resp.Items) != maxItemsLimit {
		t.Fatalf("expected %d items, got %d", maxItemsLimit, len(resp.Items))
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/"+created.ID+"/items?limit=201", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rec.Code)
	}
}

func TestSessionItemsCursorPagination(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	created := createSession(t, handler)
	source.items[created.ID] = makeDisplayItems(5)

	after := getItems(t, handler, "/sessions/"+created.ID+"/items?after=2&limit=2")
	assertItemSequences(t, after.Items, []uint64{3, 4})

	before := getItems(t, handler, "/sessions/"+created.ID+"/items?before=5&limit=2")
	assertItemSequences(t, before.Items, []uint64{3, 4})
}

func TestSessionItemsRejectInvalidCursor(t *testing.T) {
	handler := newHandler(newSessionRegistry(), &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}})

	tests := []string{
		"/sessions/missing/items?after=abc",
		"/sessions/missing/items?before=0",
		"/sessions/missing/items?after=1&before=2",
	}
	for _, path := range tests {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rec.Code)
			}
		})
	}
}

func TestSessionItemsPublicResponseDoesNotExposeInternalSession(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	created := createSession(t, handler)
	source.items[created.ID] = makeDisplayItems(1)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/"+created.ID+"/items", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	body := rec.Body.String()
	for _, internalField := range []string{"agent_snapshot", "display_log", "turns"} {
		if strings.Contains(body, internalField) {
			t.Fatalf("response leaked internal field %q: %s", internalField, body)
		}
	}
}

func TestSessionItemsReturnsStructuredToolResult(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	created := createSession(t, handler)
	source.items[created.ID] = []zotigosession.DisplayItem{{
		ID:       "item_tool_result",
		Sequence: 1,
		Type:     zotigosession.DisplayItemAssistantMessage,
		Role:     string(protocol.RoleAssistant),
		Content: []zotigosession.DisplayContentPart{{
			Type: string(protocol.ContentTypeToolResult),
			ToolResult: &zotigosession.DisplayToolResult{
				ToolCallID: "call-1",
				ToolName:   "screenshot",
				ResultType: string(protocol.ToolResultTypeContent),
				Content: []zotigosession.DisplayToolResultContentPart{{
					Type: string(protocol.ContentTypeText),
					Text: "captured",
				}, {
					Type: string(protocol.ContentTypeImage),
					Image: &zotigosession.DisplayMediaPart{
						URL:       "file:///tmp/screenshot.png",
						MediaType: "image/png",
					},
				}},
			},
		}},
		CreatedAt: time.Now().UTC(),
	}}

	resp := getItems(t, handler, "/sessions/"+created.ID+"/items")

	if len(resp.Items) != 1 || len(resp.Items[0].Content) != 1 {
		t.Fatalf("unexpected items response: %#v", resp)
	}
	result := resp.Items[0].Content[0].ToolResult
	if result == nil {
		t.Fatalf("expected structured tool result, got %#v", resp.Items[0].Content[0])
	}
	if result.ToolCallID != "call-1" || result.ToolName != "screenshot" || result.ResultType != string(protocol.ToolResultTypeContent) {
		t.Fatalf("unexpected tool result: %#v", result)
	}
	if len(result.Content) != 2 {
		t.Fatalf("expected structured content parts, got %#v", result.Content)
	}
	if result.Content[0].Type != string(protocol.ContentTypeText) || result.Content[0].Text != "captured" {
		t.Fatalf("unexpected text content part: %#v", result.Content[0])
	}
	if result.Content[1].Image == nil || result.Content[1].Image.URL != "file:///tmp/screenshot.png" {
		t.Fatalf("unexpected image content part: %#v", result.Content[1])
	}
}

func TestSessionItemsReturnsStructuredToolCall(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	created := createSession(t, handler)
	source.items[created.ID] = []zotigosession.DisplayItem{{
		ID:       "item_tool_call",
		Sequence: 1,
		Type:     zotigosession.DisplayItemAssistantMessage,
		Role:     string(protocol.RoleAssistant),
		Content: []zotigosession.DisplayContentPart{{
			Type: string(protocol.ContentTypeToolCall),
			ToolCall: &zotigosession.DisplayToolCall{
				ID:        "call-1",
				Name:      "shell",
				Arguments: `{"command":"git status"}`,
			},
		}},
		CreatedAt: time.Now().UTC(),
	}}

	resp := getItems(t, handler, "/sessions/"+created.ID+"/items")

	if len(resp.Items) != 1 || len(resp.Items[0].Content) != 1 {
		t.Fatalf("unexpected items response: %#v", resp)
	}
	call := resp.Items[0].Content[0].ToolCall
	if call == nil {
		t.Fatalf("expected structured tool call, got %#v", resp.Items[0].Content[0])
	}
	if call.ID != "call-1" || call.Name != "shell" || call.Arguments != `{"command":"git status"}` {
		t.Fatalf("unexpected tool call: %#v", call)
	}
}

func TestSessionItemsReadsStoredSessionWithoutRegistryEntry(t *testing.T) {
	source := &fakeDisplayItemSource{
		items: map[string][]zotigosession.DisplayItem{
			"sess-stored": makeDisplayItems(2),
		},
	}
	handler := newHandler(newSessionRegistry(), source)

	resp := getItems(t, handler, "/sessions/sess-stored/items")

	assertItemSequences(t, resp.Items, []uint64{1, 2})
}

func TestStoredDisplayItemSourceReadsDisplayLog(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	sess := &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:        "sess-store",
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		},
		AgentSnapshot: agent.Snapshot{
			History: []protocol.Message{protocol.NewUserMessage("runtime summary")},
		},
	}
	if err := store.Put(context.Background(), sess); err != nil {
		t.Fatalf("put session: %v", err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), sess.ID, zotigosession.DisplayItem{
		Type:    zotigosession.DisplayItemUserMessage,
		Role:    string(protocol.RoleUser),
		Content: []zotigosession.DisplayContentPart{{Type: string(protocol.ContentTypeText), Text: "display prompt"}},
	}); err != nil {
		t.Fatalf("append display item: %v", err)
	}

	source := storedDisplayItemSource{store: store}
	items, ok, err := source.LoadItems(context.Background(), "sess-store")
	if err != nil {
		t.Fatalf("load items: %v", err)
	}
	if !ok {
		t.Fatal("expected session to exist")
	}
	if len(items) != 1 || items[0].Content[0].Text != "display prompt" {
		t.Fatalf("unexpected items: %#v", items)
	}
}

func TestSessionStartTransitionsToStarting(t *testing.T) {
	handler := NewHandler()
	created := createSession(t, handler)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/start", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	var started Session
	if err := sonic.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	if started.State != SessionStateStarting {
		t.Fatalf("expected state %q, got %q", SessionStateStarting, started.State)
	}
	if started.StartedAt == nil || started.StartedAt.IsZero() {
		t.Fatal("expected started_at")
	}
}

func TestWorkerAttachTransitionsToRunning(t *testing.T) {
	handler := NewHandler()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/internal/sessions/"+created.ID+"/worker/attach", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	var running Session
	if err := sonic.Unmarshal(rec.Body.Bytes(), &running); err != nil {
		t.Fatalf("decode attach response: %v", err)
	}
	if running.State != SessionStateRunning {
		t.Fatalf("expected state %q, got %q", SessionStateRunning, running.State)
	}
}

func TestApprovalRequestFlowCreatesItemsAndAcceptsDecision(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	attachWorker(t, handler, created.ID)

	approval := createApprovalRequestForTest(t, handler, created.ID)
	if approval.Status != approvalStatusPending {
		t.Fatalf("expected status %q, got %q", approvalStatusPending, approval.Status)
	}
	if approval.TurnID != "turn-1" || len(approval.Pending) != 1 {
		t.Fatalf("unexpected approval response: %#v", approval)
	}
	if got := getSession(t, handler, created.ID); got.State != SessionStatePaused {
		t.Fatalf("expected state %q, got %q", SessionStatePaused, got.State)
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 2 {
		t.Fatalf("expected approval request and turn paused items, got %d", len(items.Items))
	}
	if items.Items[0].Type != string(zotigosession.DisplayItemApprovalRequest) {
		t.Fatalf("expected approval_request item, got %s", items.Items[0].Type)
	}
	if items.Items[0].Approval == nil || items.Items[0].Approval.ID != approval.ID {
		t.Fatalf("unexpected approval item: %#v", items.Items[0].Approval)
	}
	if got := items.Items[0].Approval.Pending[0]; got.ToolCallID != "call-1" || got.ToolName != "shell" || got.Reason != "writes files" {
		t.Fatalf("unexpected pending approval: %#v", got)
	}
	if items.Items[1].Type != string(zotigosession.DisplayItemTurnPaused) {
		t.Fatalf("expected turn_paused item, got %s", items.Items[1].Type)
	}
	if items.Items[1].Turn == nil || items.Items[1].Turn.ID != "turn-1" || items.Items[1].Turn.Reason != "need_approval" {
		t.Fatalf("unexpected turn paused item: %#v", items.Items[1].Turn)
	}

	approved := true
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/approvals/"+approval.ID, strings.NewReader(`{
		"decisions": [{"tool_call_id":"call-1","approved":true}]
	}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var resolved approvalRequestResponse
	if err := sonic.Unmarshal(rec.Body.Bytes(), &resolved); err != nil {
		t.Fatalf("decode approval response: %v", err)
	}
	if resolved.Status != approvalStatusResolved || len(resolved.Decisions) != 1 || resolved.Decisions[0].Approved != approved {
		t.Fatalf("unexpected resolved approval: %#v", resolved)
	}
	if got := getSession(t, handler, created.ID); got.State != SessionStateRunning {
		t.Fatalf("expected state %q, got %q", SessionStateRunning, got.State)
	}

	workerView := getApprovalRequest(t, handler, created.ID, approval.ID)
	if workerView.Status != approvalStatusResolved || len(workerView.Decisions) != 1 {
		t.Fatalf("worker did not observe resolved approval: %#v", workerView)
	}

	items = getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 3 {
		t.Fatalf("expected approval request, turn paused, and decision items, got %d", len(items.Items))
	}
	if items.Items[2].Type != string(zotigosession.DisplayItemApprovalDecision) {
		t.Fatalf("expected approval_decision item, got %s", items.Items[2].Type)
	}
	if items.Items[2].Approval == nil || len(items.Items[2].Approval.Decisions) != 1 || !items.Items[2].Approval.Decisions[0].Approved {
		t.Fatalf("unexpected approval decision item: %#v", items.Items[2].Approval)
	}
}

func TestWorkerAttachDoesNotResumePausedApproval(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	attachWorker(t, handler, created.ID)
	approval := createApprovalRequestForTest(t, handler, created.ID)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/internal/sessions/"+created.ID+"/worker/attach", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d", http.StatusConflict, rec.Code)
	}
	if got := getSession(t, handler, created.ID); got.State != SessionStatePaused {
		t.Fatalf("expected state %q, got %q", SessionStatePaused, got.State)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/approvals/"+approval.ID, strings.NewReader(`{
		"decisions": [{"tool_call_id":"call-1","approved":true}]
	}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
}

func TestApprovalCreateStillPausesWhenTurnPausedItemAppendFails(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	source.appendErr = func(_ string, item zotigosession.DisplayItem) error {
		if item.Type == zotigosession.DisplayItemTurnPaused {
			return errors.New("write failed")
		}
		return nil
	}
	handler := newHandler(newSessionRegistry(), source)
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	attachWorker(t, handler, created.ID)

	approval := createApprovalRequestForTest(t, handler, created.ID)
	if got := getSession(t, handler, created.ID); got.State != SessionStatePaused {
		t.Fatalf("expected state %q, got %q", SessionStatePaused, got.State)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/approvals/"+approval.ID, strings.NewReader(`{
		"decisions": [{"tool_call_id":"call-1","approved":true}]
	}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
}

func TestApprovalDecisionSucceedsWhenRegistryResumeLosesRace(t *testing.T) {
	registry := newSessionRegistry()
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(registry, source)
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	attachWorker(t, handler, created.ID)
	approval := createApprovalRequestForTest(t, handler, created.ID)

	source.appendHook = func(sessionID string, item zotigosession.DisplayItem) {
		if sessionID == created.ID && item.Type == zotigosession.DisplayItemApprovalDecision {
			_, _ = registry.End(created.ID)
		}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/approvals/"+approval.ID, strings.NewReader(`{
		"decisions": [{"tool_call_id":"call-1","approved":true}]
	}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if items.Items[len(items.Items)-1].Type != string(zotigosession.DisplayItemApprovalDecision) {
		t.Fatalf("expected durable approval_decision, got %#v", items.Items[len(items.Items)-1])
	}
}

func TestApprovalDecisionSucceedsWhenSessionEndsBeforePause(t *testing.T) {
	registry := newSessionRegistry()
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(registry, source)
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	attachWorker(t, handler, created.ID)

	source.appendHook = func(sessionID string, item zotigosession.DisplayItem) {
		if sessionID == created.ID && item.Type == zotigosession.DisplayItemApprovalRequest {
			_, _ = registry.End(created.ID)
		}
	}

	approval := createApprovalRequestForTest(t, handler, created.ID)
	if got := getSession(t, handler, created.ID); got.State != SessionStateEnded {
		t.Fatalf("expected state %q, got %q", SessionStateEnded, got.State)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/approvals/"+approval.ID, strings.NewReader(`{
		"decisions": [{"tool_call_id":"call-1","approved":false,"reason":"session ended"}]
	}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	resolved := getApprovalRequest(t, handler, created.ID, approval.ID)
	if resolved.Status != approvalStatusResolved || len(resolved.Decisions) != 1 {
		t.Fatalf("expected resolved approval, got %#v", resolved)
	}
}

func TestApprovalRequestFlowCreatesStoredDisplayLogForDaemonSession(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store})
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	attachWorker(t, handler, created.ID)

	approval := createApprovalRequestForTest(t, handler, created.ID)

	items, ok, err := store.ListDisplayItems(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("list display items: %v", err)
	}
	if !ok {
		t.Fatal("expected daemon session to be materialized in store")
	}
	if len(items) != 2 {
		t.Fatalf("expected approval request and turn paused items, got %d", len(items))
	}
	if items[0].Type != zotigosession.DisplayItemApprovalRequest || items[0].Approval == nil || items[0].Approval.ID != approval.ID {
		t.Fatalf("unexpected stored approval request item: %#v", items[0])
	}
	if items[1].Type != zotigosession.DisplayItemTurnPaused {
		t.Fatalf("expected turn_paused item, got %s", items[1].Type)
	}
}

func TestApprovalDecisionRecoversPendingRequestFromDisplayLog(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store})
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	attachWorker(t, handler, created.ID)
	approval := createApprovalRequestForTest(t, handler, created.ID)

	restarted := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store})
	newSession := createSession(t, restarted)
	if newSession.ID == created.ID {
		t.Fatalf("expected restart-created session id not to reuse %q", created.ID)
	}

	workerView := getApprovalRequest(t, restarted, created.ID, approval.ID)
	if workerView.Status != approvalStatusPending || len(workerView.Pending) != 1 {
		t.Fatalf("expected pending approval after restart, got %#v", workerView)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/approvals/"+approval.ID, strings.NewReader(`{
		"decisions": [{"tool_call_id":"call-1","approved":false,"reason":"not now"}]
	}`))
	restarted.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var resolved approvalRequestResponse
	if err := sonic.Unmarshal(rec.Body.Bytes(), &resolved); err != nil {
		t.Fatalf("decode approval response: %v", err)
	}
	if resolved.Status != approvalStatusResolved || len(resolved.Decisions) != 1 || resolved.Decisions[0].Approved {
		t.Fatalf("unexpected resolved approval: %#v", resolved)
	}
	if resolved.Decisions[0].Reason != "not now" {
		t.Fatalf("expected denial reason to persist, got %#v", resolved.Decisions[0])
	}

	workerView = getApprovalRequest(t, restarted, created.ID, approval.ID)
	if workerView.Status != approvalStatusResolved || len(workerView.Decisions) != 1 {
		t.Fatalf("expected resolved approval from display log, got %#v", workerView)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/approvals/"+approval.ID, strings.NewReader(`{
		"decisions": [{"tool_call_id":"call-1","approved":true}]
	}`))
	restarted.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d: %s", http.StatusConflict, rec.Code, rec.Body.String())
	}
}

func TestApprovalDecisionRejectsIncompleteOrUnknownDecisions(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	attachWorker(t, handler, created.ID)
	approval := createApprovalRequestForTest(t, handler, created.ID)

	tests := []struct {
		name string
		body string
	}{
		{name: "missing decision", body: `{"decisions":[]}`},
		{name: "unknown tool call", body: `{"decisions":[{"tool_call_id":"call-missing","approved":true}]}`},
		{name: "missing approved", body: `{"decisions":[{"tool_call_id":"call-1"}]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/approvals/"+approval.ID, strings.NewReader(tt.body))
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestApprovalCreateRejectsNonRunningSession(t *testing.T) {
	handler := NewHandler()
	created := createSession(t, handler)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/internal/sessions/"+created.ID+"/approvals", strings.NewReader(`{
		"turn_id":"turn-1",
		"pending":[{"tool_call_id":"call-1","tool_name":"shell"}]
	}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d", http.StatusConflict, rec.Code)
	}
}

func TestWorkerFinishTransitionsToEnded(t *testing.T) {
	handler := NewHandler()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	attachWorker(t, handler, created.ID)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/internal/sessions/"+created.ID+"/worker/finish", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	var ended Session
	if err := sonic.Unmarshal(rec.Body.Bytes(), &ended); err != nil {
		t.Fatalf("decode finish response: %v", err)
	}
	if ended.State != SessionStateEnded {
		t.Fatalf("expected state %q, got %q", SessionStateEnded, ended.State)
	}
	if ended.EndedAt == nil || ended.EndedAt.IsZero() {
		t.Fatal("expected ended_at")
	}
}

func TestWorkerFinishWithErrorTransitionsToFailed(t *testing.T) {
	handler := NewHandler()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/internal/sessions/"+created.ID+"/worker/finish", strings.NewReader(`{"error":"worker exited"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	var failed Session
	if err := sonic.Unmarshal(rec.Body.Bytes(), &failed); err != nil {
		t.Fatalf("decode finish response: %v", err)
	}
	if failed.State != SessionStateFailed {
		t.Fatalf("expected state %q, got %q", SessionStateFailed, failed.State)
	}
	if failed.Error != "worker exited" {
		t.Fatalf("expected error %q, got %q", "worker exited", failed.Error)
	}
}

func TestWorkerFinishRejectsCreatedSession(t *testing.T) {
	handler := NewHandler()
	created := createSession(t, handler)

	tests := []struct {
		name string
		body string
	}{
		{name: "without error", body: ""},
		{name: "with error", body: `{"error":"worker exited"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/internal/sessions/"+created.ID+"/worker/finish", strings.NewReader(tt.body))
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusConflict {
				t.Fatalf("expected status %d, got %d", http.StatusConflict, rec.Code)
			}
		})
	}
}

func TestSessionStartRejectsInvalidTransition(t *testing.T) {
	handler := NewHandler()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/start", nil))

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d", http.StatusConflict, rec.Code)
	}
}

func TestWorkerAttachRejectsCreatedSession(t *testing.T) {
	handler := NewHandler()
	created := createSession(t, handler)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/internal/sessions/"+created.ID+"/worker/attach", nil))

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d", http.StatusConflict, rec.Code)
	}
}

func TestPublicSessionRouteRejectsWorkerActions(t *testing.T) {
	handler := NewHandler()
	created := createSession(t, handler)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/worker/attach", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d", http.StatusNotFound, rec.Code)
	}
}

func TestSessionTransitionEndpointsRejectMissingSession(t *testing.T) {
	handler := NewHandler()

	tests := []struct {
		name string
		path string
	}{
		{name: "start", path: "/sessions/missing/start"},
		{name: "worker attach", path: "/internal/sessions/missing/worker/attach"},
		{name: "worker finish", path: "/internal/sessions/missing/worker/finish"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tt.path, nil))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("expected status %d, got %d", http.StatusNotFound, rec.Code)
			}
		})
	}
}

func TestSessionRejectsUnknownAction(t *testing.T) {
	handler := NewHandler()
	created := createSession(t, handler)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/bogus", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d", http.StatusNotFound, rec.Code)
	}
}

func TestWorkerFinishRejectsBadJSON(t *testing.T) {
	handler := NewHandler()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/internal/sessions/"+created.ID+"/worker/finish", strings.NewReader(`{`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rec.Code)
	}
}

func TestSessionsGetByIDNotFound(t *testing.T) {
	rec := httptest.NewRecorder()

	NewHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/missing", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d", http.StatusNotFound, rec.Code)
	}
}

func TestSessionsListUsesCreationOrder(t *testing.T) {
	handler := NewHandler()
	createdIDs := make([]string, 0, 11)

	for i := 0; i < 11; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions", nil))
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %d: expected status %d, got %d", i, http.StatusCreated, rec.Code)
		}
		var created Session
		if err := sonic.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatalf("create %d: decode response: %v", i, err)
		}
		createdIDs = append(createdIDs, created.ID)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	var list sessionListResponse
	if err := sonic.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(list.Sessions) != len(createdIDs) {
		t.Fatalf("expected %d sessions, got %d", len(createdIDs), len(list.Sessions))
	}
	for i, session := range list.Sessions {
		if session.ID != createdIDs[i] {
			t.Fatalf("session %d: expected id %q, got %q", i, createdIDs[i], session.ID)
		}
	}
}

func TestSessionRegistryLifecycleTransitions(t *testing.T) {
	registry := newSessionRegistry()

	created := registry.Create()
	if created.State != SessionStateCreated {
		t.Fatalf("expected state %q, got %q", SessionStateCreated, created.State)
	}

	starting, err := registry.Start(created.ID)
	if err != nil {
		t.Fatalf("start session: %v", err)
	}
	if starting.State != SessionStateStarting {
		t.Fatalf("expected state %q, got %q", SessionStateStarting, starting.State)
	}
	if starting.StartedAt == nil || starting.StartedAt.IsZero() {
		t.Fatal("expected started_at")
	}

	running, err := registry.MarkRunning(created.ID)
	if err != nil {
		t.Fatalf("mark session running: %v", err)
	}
	if running.State != SessionStateRunning {
		t.Fatalf("expected state %q, got %q", SessionStateRunning, running.State)
	}

	ended, err := registry.End(created.ID)
	if err != nil {
		t.Fatalf("end session: %v", err)
	}
	if ended.State != SessionStateEnded {
		t.Fatalf("expected state %q, got %q", SessionStateEnded, ended.State)
	}
	if ended.EndedAt == nil || ended.EndedAt.IsZero() {
		t.Fatal("expected ended_at")
	}

	if _, err := registry.MarkRunning(created.ID); !errors.Is(err, errInvalidSessionTransition) {
		t.Fatalf("expected invalid transition error, got %v", err)
	}
}

func TestSessionRegistryRejectsInvalidLifecycleTransitions(t *testing.T) {
	tests := []struct {
		name string
		run  func(*sessionRegistry, string) error
	}{
		{
			name: "start twice",
			run: func(registry *sessionRegistry, id string) error {
				if _, err := registry.Start(id); err != nil {
					return err
				}
				_, err := registry.Start(id)
				return err
			},
		},
		{
			name: "mark running from created",
			run: func(registry *sessionRegistry, id string) error {
				_, err := registry.MarkRunning(id)
				return err
			},
		},
		{
			name: "end from created",
			run: func(registry *sessionRegistry, id string) error {
				_, err := registry.End(id)
				return err
			},
		},
		{
			name: "start after ended",
			run: func(registry *sessionRegistry, id string) error {
				if _, err := registry.Start(id); err != nil {
					return err
				}
				if _, err := registry.MarkRunning(id); err != nil {
					return err
				}
				if _, err := registry.End(id); err != nil {
					return err
				}
				_, err := registry.Start(id)
				return err
			},
		},
		{
			name: "end after failed",
			run: func(registry *sessionRegistry, id string) error {
				if _, err := registry.Start(id); err != nil {
					return err
				}
				if _, err := registry.Fail(id, "failed"); err != nil {
					return err
				}
				_, err := registry.End(id)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := newSessionRegistry()
			session := registry.Create()
			if err := tt.run(registry, session.ID); !errors.Is(err, errInvalidSessionTransition) {
				t.Fatalf("expected invalid transition error, got %v", err)
			}
		})
	}
}

func TestSessionRegistryRejectsMissingSession(t *testing.T) {
	registry := newSessionRegistry()

	if _, err := registry.Start("missing"); !errors.Is(err, errSessionNotFound) {
		t.Fatalf("expected not found error, got %v", err)
	}
}

func TestSessionsRejectsUnsupportedMethod(t *testing.T) {
	req := httptest.NewRequest(http.MethodDelete, "/sessions", nil)
	rec := httptest.NewRecorder()

	NewHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected status %d, got %d", http.StatusMethodNotAllowed, rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, POST" {
		t.Fatalf("expected Allow header %q, got %q", "GET, POST", got)
	}
}

func TestSessionRejectsUnsupportedMethod(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/sessions/sess-1", nil)
	rec := httptest.NewRecorder()

	NewHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected status %d, got %d", http.StatusMethodNotAllowed, rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != http.MethodGet {
		t.Fatalf("expected Allow header %q, got %q", http.MethodGet, got)
	}
}

func TestSessionStartRejectsUnsupportedMethod(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/sessions/sess-1/start", nil)
	rec := httptest.NewRecorder()

	NewHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected status %d, got %d", http.StatusMethodNotAllowed, rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != http.MethodPost {
		t.Fatalf("expected Allow header %q, got %q", http.MethodPost, got)
	}
}

func TestHealthRejectsNonGET(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/health", nil)
	rec := httptest.NewRecorder()

	NewHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected status %d, got %d", http.StatusMethodNotAllowed, rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != http.MethodGet {
		t.Fatalf("expected Allow header %q, got %q", http.MethodGet, got)
	}
}

func startSession(t *testing.T, handler http.Handler, id string) Session {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+id+"/start", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	var session Session
	if err := sonic.Unmarshal(rec.Body.Bytes(), &session); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	return session
}

func attachWorker(t *testing.T, handler http.Handler, id string) Session {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/internal/sessions/"+id+"/worker/attach", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	var session Session
	if err := sonic.Unmarshal(rec.Body.Bytes(), &session); err != nil {
		t.Fatalf("decode attach response: %v", err)
	}
	return session
}

func getSession(t *testing.T, handler http.Handler, id string) Session {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/"+id, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var session Session
	if err := sonic.Unmarshal(rec.Body.Bytes(), &session); err != nil {
		t.Fatalf("decode session response: %v", err)
	}
	return session
}

func createApprovalRequestForTest(t *testing.T, handler http.Handler, id string) approvalRequestResponse {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/internal/sessions/"+id+"/approvals", strings.NewReader(`{
		"turn_id": "turn-1",
		"pending": [{
			"tool_call_id": "call-1",
			"tool_name": "shell",
			"arguments": "{\"command\":\"touch file\"}",
			"description": "Run shell command",
			"reason": "writes files",
			"risk_level": "medium",
			"source": "classifier",
			"requires_snapshot": true
		}]
	}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	var approval approvalRequestResponse
	if err := sonic.Unmarshal(rec.Body.Bytes(), &approval); err != nil {
		t.Fatalf("decode approval response: %v", err)
	}
	return approval
}

func getApprovalRequest(t *testing.T, handler http.Handler, sessionID string, approvalID string) approvalRequestResponse {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/internal/sessions/"+sessionID+"/approvals/"+approvalID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var approval approvalRequestResponse
	if err := sonic.Unmarshal(rec.Body.Bytes(), &approval); err != nil {
		t.Fatalf("decode approval response: %v", err)
	}
	return approval
}

func getItems(t *testing.T, handler http.Handler, path string) itemsResponse {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var resp itemsResponse
	if err := sonic.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode items response: %v", err)
	}
	return resp
}

func makeDisplayItems(count int) []zotigosession.DisplayItem {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	items := make([]zotigosession.DisplayItem, 0, count)
	for i := 1; i <= count; i++ {
		items = append(items, zotigosession.DisplayItem{
			ID:        fmt.Sprintf("item_sess-test_%d", i),
			Sequence:  uint64(i),
			Type:      zotigosession.DisplayItemUserMessage,
			Role:      string(protocol.RoleUser),
			Content:   []zotigosession.DisplayContentPart{{Type: string(protocol.ContentTypeText), Text: "message"}},
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		})
	}
	return items
}

func assertItemSequences(t *testing.T, items []itemResponse, expected []uint64) {
	t.Helper()
	if len(items) != len(expected) {
		t.Fatalf("expected %d items, got %d", len(expected), len(items))
	}
	for idx, sequence := range expected {
		if items[idx].Sequence != sequence {
			t.Fatalf("item %d: expected sequence %d, got %d", idx, sequence, items[idx].Sequence)
		}
	}
}
