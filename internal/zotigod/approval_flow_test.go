package zotigod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/internal/testutil/sessionlogtest"

	"github.com/gorilla/websocket"
	"github.com/jayyao97/zotigo/core/config"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
	"github.com/jayyao97/zotigo/internal/zotigod/workerbase"
)

func TestApprovalRequestFlowCreatesItemsAndAcceptsDecision(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	source := sessionlog.StoredItemSource{Store: store}
	registry := newSessionRegistry()
	handler := newHandler(registry, source, handlerOptions{store: store})
	server := httptest.NewServer(handler)
	defer server.Close()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()

	approval := createApprovalRequestForTest(t, registry, source, created.ID)
	if approval.Status != sessionlog.ApprovalStatusPending {
		t.Fatalf("expected status %q, got %q", sessionlog.ApprovalStatusPending, approval.Status)
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
	workerDone := respondToApprovalDecision(worker, source, created.ID, approval)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/approvals/"+approval.ID, strings.NewReader(`{
		"decisions": [{"tool_call_id":"call-1","approved":true}]
	}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if err := <-workerDone; err != nil {
		t.Fatalf("worker approval response: %v", err)
	}
	var resolved wire.ApprovalRequest
	if err := decodeAPIData(t, rec.Body.Bytes(), &resolved); err != nil {
		t.Fatalf("decode approval response: %v", err)
	}
	if resolved.Status != sessionlog.ApprovalStatusResolved || len(resolved.Decisions) != 1 || resolved.Decisions[0].Approved != approved {
		t.Fatalf("unexpected resolved approval: %#v", resolved)
	}
	if got := getSession(t, handler, created.ID); got.State != SessionStateRunning {
		t.Fatalf("expected state %q, got %q", SessionStateRunning, got.State)
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

func TestInteractionRequestFlowAcceptsAnswersAndResumesSession(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	source := sessionlog.StoredItemSource{Store: store}
	registry := newSessionRegistry()
	handler := newHandler(registry, source, handlerOptions{store: store})
	server := httptest.NewServer(handler)
	defer server.Close()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()

	interaction, err := sessionlog.NewUserInputInteraction(created.ID, "turn-1", "item-1", wire.InteractionRequester{Agent: "codex", Name: "Subagent"}, []wire.InteractionQuestion{{ID: "mode", Question: "Choose", Options: []wire.InteractionOption{{Label: "Safe"}}}}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.AppendItem(context.Background(), created.ID, sessionlog.InteractionItem(interaction, false)); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Pause(created.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		var message wire.Message
		if err := worker.ReadJSON(&message); err != nil {
			done <- err
			return
		}
		if message.InteractionResponse == nil || message.InteractionResponse.Answers["mode"][0] != "Safe" {
			done <- fmt.Errorf("unexpected interaction response: %#v", message)
			return
		}
		resolvedAt := time.Now().UTC()
		resolved := interaction
		resolved.Status = sessionlog.InteractionStatusResolved
		resolved.Answers = message.InteractionResponse.Answers
		resolved.ResolvedAt = &resolvedAt
		if _, err := source.AppendItem(context.Background(), created.ID, sessionlog.InteractionItem(resolved, true)); err != nil {
			done <- err
			return
		}
		done <- worker.WriteJSON(wire.Message{Type: wire.MessageInteractionResult, InteractionResult: &wire.InteractionResult{RequestID: message.InteractionResponse.RequestID, Interaction: &resolved}})
	}()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/interactions/"+interaction.ID, strings.NewReader(`{"answers":{"mode":["Safe"]}}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := getSession(t, handler, created.ID); got.State != SessionStateRunning {
		t.Fatalf("session state = %q", got.State)
	}
}

func TestWorkerApprovalRequestMessagePausesSession(t *testing.T) {
	registry := newSessionRegistry()
	handler := newHandler(registry, &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}})
	server := httptest.NewServer(handler)
	defer server.Close()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()

	if err := worker.WriteJSON(wire.Message{
		Type: wire.MessageApprovalRequest,
		ApprovalRequest: &wire.ApprovalRequest{
			ID:     "apr-worker-owned",
			Status: sessionlog.ApprovalStatusPending,
		},
	}); err != nil {
		t.Fatalf("send approval request message: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := getSession(t, handler, created.ID); got.State == SessionStatePaused {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected state %q, got %q", SessionStatePaused, getSession(t, handler, created.ID).State)
}

func TestWorkerApprovalRequestBeforeReadyKeepsSessionPaused(t *testing.T) {
	source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}
	registry := newSessionRegistry()
	handler := newHandler(registry, source)
	server := httptest.NewServer(handler)
	defer server.Close()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker, generation := connectWorker(t, server, created.ID)
	defer worker.Close()
	approval := sessionlog.PublicApprovalRequest(sessionlog.ApprovalRequest{
		ID:        "apr-before-ready",
		SessionID: created.ID,
		TurnID:    "turn-1",
		Status:    sessionlog.ApprovalStatusPending,
		Pending:   []zotigosession.DisplayPendingApproval{{ToolCallID: "call-1", ToolName: "shell"}},
	})
	if _, err := source.AppendItem(context.Background(), created.ID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemApprovalRequest,
		Approval: &zotigosession.DisplayApproval{
			ID:      approval.ID,
			TurnID:  approval.TurnID,
			Pending: []zotigosession.DisplayPendingApproval{{ToolCallID: "call-1", ToolName: "shell"}},
		},
	}); err != nil {
		t.Fatalf("append approval request before ready: %v", err)
	}
	if _, err := source.AppendItem(context.Background(), created.ID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnPaused,
		Turn: &zotigosession.DisplayTurn{ID: approval.TurnID, Reason: "need_approval"},
	}); err != nil {
		t.Fatalf("append turn paused before ready: %v", err)
	}
	if err := worker.WriteJSON(wire.Message{
		Type:            wire.MessageApprovalRequest,
		ApprovalRequest: &approval,
	}); err != nil {
		t.Fatalf("send approval request before ready: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := getSession(t, handler, created.ID); got.State == SessionStatePaused {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	resp, err := postWorkerReady(server, created.ID, generation)
	if err != nil {
		t.Fatalf("report worker ready: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("worker ready status = %d", resp.StatusCode)
	}
	if got := getSession(t, handler, created.ID); got.State != SessionStatePaused {
		t.Fatalf("expected state %q, got %q", SessionStatePaused, got.State)
	}
}

func TestPausedApprovalCanStartReplacementWorker(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	projectConfig := "default_profile: test\nprofiles:\n  test:\n    provider: openai\n    model: test\n"
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}
	registry := newSessionRegistry()
	workers := newWorkerRegistry()
	var server *httptest.Server
	var launches atomic.Int32
	connections := make(chan *websocket.Conn, 2)
	launcher := workerLauncherFunc(func(_ context.Context, sessionID string, _ string) error {
		if launches.Add(1) == 2 {
			display := sessionlog.NewLog(sessionID, source)
			if _, err := display.ResolvePendingApprovalsForOpenTurn(context.Background(), workerbase.ApprovalWorkerRestartedReason); err != nil {
				return err
			}
			if err := display.InterruptOpenTurn(context.Background(), sessionlog.WorkerRestartedReason); err != nil {
				return err
			}
		}
		worker, err := connectReadyWorker(server, sessionID)
		if err != nil {
			return err
		}
		connections <- worker
		return nil
	})
	handler := newHandler(registry, source, handlerOptions{workers: workers, launcher: launcher, workerConnectTimeout: time.Second})
	server = httptest.NewServer(handler)
	defer server.Close()
	created := createSessionWithWorkingDirectory(t, handler, workDir)
	started := startSession(t, handler, created.ID)
	if started.State != SessionStateRunning {
		t.Fatalf("initial start state = %q", started.State)
	}
	firstWorker := <-connections
	createApprovalRequestForTest(t, registry, source, created.ID)
	if err := firstWorker.Close(); err != nil {
		t.Fatalf("close first worker: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := getSession(t, handler, created.ID); got.State == SessionStatePaused && !workers.Has(created.ID) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/start", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("restart paused session: status %d: %s", rec.Code, rec.Body.String())
	}
	var restarted Session
	if err := decodeAPIData(t, rec.Body.Bytes(), &restarted); err != nil {
		t.Fatalf("decode restarted session: %v", err)
	}
	if restarted.State != SessionStateRunning {
		t.Fatalf("replacement start state = %q", restarted.State)
	}
	secondWorker := <-connections
	defer secondWorker.Close()
	items, _, err := source.LoadItems(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("load recovered display items: %v", err)
	}
	if sessionlog.HasPendingApproval(items) {
		t.Fatal("replacement worker left approval pending")
	}
}

func TestWorkerApprovalRequestSurvivesTurnPausedHintAppendFailure(t *testing.T) {
	source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}
	source.AppendErr = func(_ string, item zotigosession.DisplayItem) error {
		if item.Type == zotigosession.DisplayItemTurnPaused {
			return errors.New("write failed")
		}
		return nil
	}
	display := sessionlog.NewLog("sess-approval-write-failure", source)
	approval := sessionlog.ApprovalRequest{
		ID:        "apr-write-failure",
		SessionID: "sess-approval-write-failure",
		TurnID:    "turn-1",
		Status:    sessionlog.ApprovalStatusPending,
		Pending: []zotigosession.DisplayPendingApproval{{
			ToolCallID: "call-1",
			ToolName:   "shell",
		}},
	}
	if _, err := display.ApprovalRequested(context.Background(), approval); err != nil {
		t.Fatalf("approval request should remain committed: %v", err)
	}
}

func TestApprovalWaitDoesNotBlockAnotherSessionFinish(t *testing.T) {
	source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}
	registry := newSessionRegistry()
	workers := newWorkerRegistry()
	handler := newHandler(registry, source, handlerOptions{workers: workers})
	server := httptest.NewServer(handler)
	defer server.Close()

	first := createSession(t, handler)
	startSession(t, handler, first.ID)
	firstWorker := dialWorker(t, server, first.ID)
	defer firstWorker.Close()
	approval := createApprovalRequestForTest(t, registry, source, first.ID)

	second := createSession(t, handler)
	startSession(t, handler, second.ID)
	attachWorker(t, handler, second.ID)

	decisionCtx, cancelDecision := context.WithCancel(context.Background())
	decisionDone := make(chan struct{})
	go func() {
		defer close(decisionDone)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/sessions/"+first.ID+"/approvals/"+approval.ID, strings.NewReader(`{
			"decisions": [{"tool_call_id":"call-1","approved":true}]
		}`)).WithContext(decisionCtx)
		handler.ServeHTTP(rec, req)
	}()
	var decision wire.Message
	if err := firstWorker.ReadJSON(&decision); err != nil {
		t.Fatalf("read first session approval decision: %v", err)
	}
	if decision.Type != wire.MessageApprovalDecision {
		t.Fatalf("unexpected first worker message: %#v", decision)
	}

	finishDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/internal/sessions/"+second.ID+"/worker/finish", nil))
		finishDone <- rec
	}()
	select {
	case rec := <-finishDone:
		if rec.Code != http.StatusOK {
			t.Fatalf("finish second session: status %d: %s", rec.Code, rec.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("second session finish blocked behind first session approval")
	}

	cancelDecision()
	select {
	case <-decisionDone:
	case <-time.After(time.Second):
		t.Fatal("canceled approval request did not return")
	}
}

func TestApprovalDecisionTimesOutWaitingForWorkerAcknowledgement(t *testing.T) {
	source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}
	registry := newSessionRegistry()
	workers := newWorkerRegistry()
	workers.approvalWait = 20 * time.Millisecond
	handler := newHandler(registry, source, handlerOptions{workers: workers})
	server := httptest.NewServer(handler)
	defer server.Close()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	approval := createApprovalRequestForTest(t, registry, source, created.ID)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/approvals/"+approval.ID, strings.NewReader(`{
		"decisions": [{"tool_call_id":"call-1","approved":true}]
	}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status %d, got %d: %s", http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	}
}

func TestApprovalDecisionSucceedsWhenRegistryResumeLosesRace(t *testing.T) {
	registry := newSessionRegistry()
	source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(registry, source)
	server := httptest.NewServer(handler)
	defer server.Close()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	approval := createApprovalRequestForTest(t, registry, source, created.ID)

	source.AppendHook = func(sessionID string, item zotigosession.DisplayItem) {
		if sessionID == created.ID && item.Type == zotigosession.DisplayItemApprovalDecision {
			_, _ = registry.End(created.ID)
		}
	}

	workerDone := respondToApprovalDecision(worker, source, created.ID, approval)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/approvals/"+approval.ID, strings.NewReader(`{
		"decisions": [{"tool_call_id":"call-1","approved":true}]
	}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if err := <-workerDone; err != nil {
		t.Fatalf("worker approval response: %v", err)
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if items.Items[len(items.Items)-1].Type != string(zotigosession.DisplayItemApprovalDecision) {
		t.Fatalf("expected durable approval_decision, got %#v", items.Items[len(items.Items)-1])
	}
}

func TestPersistedApprovalRemainsReadableWhenSessionEnds(t *testing.T) {
	registry := newSessionRegistry()
	source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(registry, source)
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	approvalID := sessionlog.NewID("apr")
	source.AppendHook = func(sessionID string, item zotigosession.DisplayItem) {
		if sessionID == created.ID && item.Type == zotigosession.DisplayItemApprovalRequest {
			_, _ = registry.End(created.ID)
		}
	}
	if _, err := source.AppendItem(context.Background(), created.ID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemApprovalRequest,
		Approval: &zotigosession.DisplayApproval{
			ID:      approvalID,
			TurnID:  "turn-1",
			Pending: []zotigosession.DisplayPendingApproval{{ToolCallID: "call-1", ToolName: "shell"}},
		},
	}); err != nil {
		t.Fatalf("append approval request: %v", err)
	}
	if got := getSession(t, handler, created.ID); got.State != SessionStateEnded {
		t.Fatalf("expected state %q, got %q", SessionStateEnded, got.State)
	}

	items, _, err := source.LoadItems(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("load approval: %v", err)
	}
	approval, ok := sessionlog.ApprovalFromItems(created.ID, approvalID, items)
	if !ok || approval.Status != sessionlog.ApprovalStatusPending {
		t.Fatalf("expected pending persisted approval, got %#v", approval)
	}
}

func TestApprovalRequestFlowCreatesStoredDisplayLogForDaemonSession(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	source := sessionlog.StoredItemSource{Store: store}
	registry := newSessionRegistry()
	handler := newHandler(registry, source)
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	attachWorker(t, handler, created.ID)

	approval := createApprovalRequestForTest(t, registry, source, created.ID)

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

func TestApprovalDecisionRejectsOfflineSession(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	source := sessionlog.StoredItemSource{Store: store}
	registry := newSessionRegistry()
	handler := newHandler(registry, source)
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	attachWorker(t, handler, created.ID)
	approval := createApprovalRequestForTest(t, registry, source, created.ID)

	restarted := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store})
	newSession := createSession(t, restarted)
	if newSession.ID == created.ID {
		t.Fatalf("expected restart-created session id not to reuse %q", created.ID)
	}

	items, _, err := store.ListDisplayItems(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("load persisted approval: %v", err)
	}
	workerView, ok := sessionlog.ApprovalFromItems(created.ID, approval.ID, items)
	if !ok || workerView.Status != sessionlog.ApprovalStatusPending || len(workerView.Pending) != 1 {
		t.Fatalf("expected pending approval after restart, got %#v", workerView)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/approvals/"+approval.ID, strings.NewReader(`{
		"decisions": [{"tool_call_id":"call-1","approved":false,"reason":"not now"}]
	}`))
	restarted.ServeHTTP(rec, req)
	assertAPIError(t, rec, http.StatusConflict, "session_not_live", "approval decision requires a live session")
}

func TestApprovalDecisionRejectsIncompleteOrUnknownDecisions(t *testing.T) {
	source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}
	registry := newSessionRegistry()
	handler := newHandler(registry, source)
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	attachWorker(t, handler, created.ID)
	approval := createApprovalRequestForTest(t, registry, source, created.ID)

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
