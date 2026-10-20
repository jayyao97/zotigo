package zotigod

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/core/workspace"
)

func TestEmptyQueueDoesNotLoadHistory(t *testing.T) {
	catalog, err := workspace.Open(newTestStoreRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	// A nil item source makes any accidental history read fail immediately.
	h := &handler{catalog: catalog, queueOps: newSessionOperationLocks()}
	if err := h.dispatchQueuedInputs("no-queue", false, true); err != nil {
		t.Fatal(err)
	}
}

type slowQueueHistory struct {
	storedDisplayItemSource
	delay atomic.Bool
}

func (s *slowQueueHistory) LoadItems(ctx context.Context, id string) ([]zotigosession.DisplayItem, bool, error) {
	if s.delay.Load() {
		time.Sleep(350 * time.Millisecond)
	}
	return s.storedDisplayItemSource.LoadItems(ctx, id)
}

func TestQueuedInputsWaitForTurnAndMerge(t *testing.T) {
	for _, agent := range []string{"zotigo", "codex"} {
		t.Run(agent, func(t *testing.T) { testQueuedInputsWaitForTurnAndMerge(t, agent, false, false) })
		t.Run(agent+" send now", func(t *testing.T) { testQueuedInputsWaitForTurnAndMerge(t, agent, true, false) })
		t.Run(agent+" error after admission", func(t *testing.T) { testQueuedInputsWaitForTurnAndMerge(t, agent, true, true) })
	}
}

func testQueuedInputsWaitForTurnAndMerge(t *testing.T, agent string, sendNow, failAfterAdmission bool) {
	ctx := context.Background()
	root := newTestStoreRoot(t)
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	catalog, err := workspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	sessionID := "queue-test"
	now := time.Now().UTC()
	dir := t.TempDir()
	if err := store.Put(ctx, &zotigosession.Session{Metadata: zotigosession.Metadata{ID: sessionID, Agent: agent, WorkingDirectory: dir, CreatedAt: now, UpdatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	registry := newSessionRegistry()
	registry.Add(Session{ID: sessionID, State: SessionStateRunning, Agent: agent, WorkingDirectory: dir, CreatedAt: now})
	source := &slowQueueHistory{storedDisplayItemSource: storedDisplayItemSource{store: store}}
	if _, err := source.AppendItem(ctx, sessionID, zotigosession.DisplayItem{ID: "turn-start", Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "old"}, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	handler := newHandler(registry, source, handlerOptions{store: store, catalog: catalog})
	server := httptest.NewServer(handler)
	defer server.Close()
	worker, generation := connectWorkerForActivation(t, server, sessionID, "0")
	defer worker.Close()
	markWorkerReady(t, server, sessionID, generation)
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(method, "/sessions/"+sessionID+"/queue"+path, strings.NewReader(body)))
		return rec
	}
	for _, id := range []string{"a", "b", "c"} {
		rec := request("POST", "", `{"client_message_id":"`+id+`","text":"`+id+`"}`)
		if rec.Code != 201 {
			t.Fatalf("enqueue: %d %s", rec.Code, rec.Body.String())
		}
	}
	if rec := request("POST", "", `{"client_message_id":"a","text":"a"}`); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if rec := request("POST", "", `{"client_message_id":"a","text":"different"}`); rec.Code != 409 {
		t.Fatalf("idempotency: %d", rec.Code)
	}
	if rec := request("PATCH", "/a", `{"text":"edited","revision":1}`); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if rec := request("DELETE", "/c", `{"revision":1}`); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	var listed struct {
		Items []queuedInputResponse `json:"items"`
	}
	rec := request("GET", "", "")
	if err := decodeAPIData(t, rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Items) != 2 || listed.Items[0].Text != "edited" || listed.Items[0].State != "queued" {
		t.Fatalf("queue=%+v response=%d %s", listed, rec.Code, rec.Body.String())
	}
	var idleSequence uint64
	if sendNow {
		if rec := request("POST", "/b/send", `{"revision":99}`); rec.Code != 409 {
			t.Fatalf("stale send = %d", rec.Code)
		}
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- request("POST", "/b/send", `{"revision":1}`) }()
		if err := worker.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		var input workerMessage
		if err := worker.ReadJSON(&input); err != nil {
			t.Fatal(err)
		}
		if input.InputRequest == nil || input.InputRequest.StartOnly || input.InputRequest.Command.Message.Text != "b" {
			t.Fatalf("immediate input = %+v", input)
		}
		// A definite rejection must preserve the item and permit a fresh retry.
		if err := worker.WriteJSON(workerMessage{Type: workerMessageInputResult, InputResult: &workerInputResult{RequestID: input.InputRequest.RequestID, ErrorCode: "input_failed", Error: "image decoding failed before admission"}}); err != nil {
			t.Fatal(err)
		}
		select {
		case rec := <-done:
			if rec.Code != 409 {
				t.Fatalf("rejection = %d", rec.Code)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("rejection timed out")
		}
		row, err := catalog.QueuedInputByID(ctx, sessionID, "b")
		if err != nil || row == nil || row.BatchID != "" || row.State != "queued" {
			t.Fatalf("rejected item lost: %+v %v", row, err)
		}
		go func() { done <- request("POST", "/b/send", fmt.Sprintf(`{"revision":%d}`, row.Revision)) }()
		if err := worker.ReadJSON(&input); err != nil {
			t.Fatal(err)
		}
		if input.InputRequest == nil {
			t.Fatal("retry input missing")
		}
		command := steeringCommandForRequest(*input.InputRequest, "old")
		accepted, err := appendAcceptedSessionInput(ctx, source, sessionID, command)
		if err != nil {
			t.Fatal(err)
		}
		idleSequence = accepted.Sequence
		result := &workerInputResult{RequestID: input.InputRequest.RequestID, Command: &accepted}
		wantStatus := 200
		if failAfterAdmission {
			result.Command = nil
			result.ErrorCode, result.Error = "input_failed", "approval interruption failed after admission"
		}
		if err := worker.WriteJSON(workerMessage{Type: workerMessageInputResult, InputResult: result}); err != nil {
			t.Fatal(err)
		}
		select {
		case rec := <-done:
			if rec.Code != wantStatus {
				t.Fatalf("send now: %d %s", rec.Code, rec.Body.String())
			}
		case <-time.After(3 * time.Second):
			t.Fatal("send now timed out")
		}
		if failAfterAdmission {
			row, err := catalog.QueuedInputByID(ctx, sessionID, "b")
			if err != nil || row == nil || row.BatchID != accepted.ID || row.State != "sent" {
				t.Fatalf("durable admission must reconcile immediately: %+v %v", row, err)
			}
			body := fmt.Sprintf(`{"revision":%d}`, row.Revision)
			if rec := request("POST", "/b/send", body); rec.Code != 200 {
				t.Fatalf("reconciled retry = %d", rec.Code)
			}
			rows, err := catalog.QueuedInputs(ctx, sessionID)
			if err != nil || len(rows) != 1 || rows[0].ID != "a" {
				t.Fatalf("reconciliation lost remaining queue: %+v %v", rows, err)
			}
			return
		}
		if rec := request("POST", "/b/send", `{"revision":1}`); rec.Code != 200 {
			t.Fatalf("repeat send = %d", rec.Code)
		}
		rows, err := catalog.QueuedInputs(ctx, sessionID)
		if err != nil || len(rows) != 1 || rows[0].ID != "a" || rows[0].State != "queued" {
			t.Fatalf("remaining queue: %+v %v", rows, err)
		}
	}
	if _, err := source.AppendItem(ctx, sessionID, zotigosession.DisplayItem{ID: "turn-end", Type: zotigosession.DisplayItemTurnCompleted, Turn: &zotigosession.DisplayTurn{ID: "old", Status: "completed"}, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	// Preparation deliberately exceeds Codex's 250ms idle eviction timeout.
	source.delay.Store(true)
	if err := worker.WriteJSON(workerMessage{Type: workerMessageIdle, Idle: &workerIdle{CommandSequence: idleSequence}}); err != nil {
		t.Fatal(err)
	}
	if err := worker.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var input workerMessage
	if err := worker.ReadJSON(&input); err != nil {
		rows, _ := catalog.QueuedInputs(ctx, sessionID)
		current, _ := registry.Get(sessionID)
		t.Fatalf("read input: %v queue=%+v session=%+v", err, rows, current)
	}
	wantText := "edited\n\nb"
	if sendNow {
		wantText = "edited"
	}
	if input.InputRequest == nil || !input.InputRequest.StartOnly || input.InputRequest.Command.Message.Text != wantText {
		t.Fatalf("input=%+v", input)
	}
	command := input.InputRequest.Command
	accepted, err := appendAcceptedSessionInput(ctx, source, sessionID, command)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.WriteJSON(workerMessage{Type: workerMessageInputResult, InputResult: &workerInputResult{RequestID: input.InputRequest.RequestID, Command: &accepted}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		rows, err := catalog.QueuedInputs(ctx, sessionID)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("acknowledged batch still queued")
		}
		time.Sleep(time.Millisecond)
	}
	if rec := request(http.MethodPost, "", `{"client_message_id":"b","text":"b"}`); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
}

func TestQueuedInputImagesAndLostAcknowledgement(t *testing.T) {
	t.Run("message", func(t *testing.T) { testQueuedInputImagesAndLostAcknowledgement(t, false) })
	t.Run("steering", func(t *testing.T) { testQueuedInputImagesAndLostAcknowledgement(t, true) })
}

func testQueuedInputImagesAndLostAcknowledgement(t *testing.T, steering bool) {
	ctx := context.Background()
	root := newTestStoreRoot(t)
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	catalog, err := workspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	source := storedDisplayItemSource{store: store}
	h := &handler{catalog: catalog, store: store, items: source, workers: newWorkerRegistry(), queueOps: newSessionOperationLocks()}
	images, err := validateMessageImages([]submitMessageImageRequest{{MimeType: "image/png", DataBase64: tinyPNGBase64()}})
	if err != nil {
		t.Fatal(err)
	}
	first, _, _, err := h.prepareSessionInputCommand(ctx, "s", "a", "first", images, []string{"review"}, false)
	if err != nil {
		t.Fatal(err)
	}
	first.Message.Images[0].DataBase64 = ""
	second := commandResponse{ID: "b", Type: sessionCommandMessage, Message: &messageCommandPayload{Text: "second", Skills: []string{"review", "writing"}}}
	for _, command := range []commandResponse{first, second} {
		data, err := json.Marshal(command)
		if err != nil {
			t.Fatal(err)
		}
		if err := catalog.QueueInput(ctx, "s", command.ID, data); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := catalog.ClaimQueuedInputs(ctx, "s", "batch", false)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := mergeQueuedInputs(rows)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Message.Text != "first\n\nsecond" || !equalStrings(merged.Message.Skills, []string{"review", "writing"}) {
		t.Fatalf("merge=%+v", merged.Message)
	}
	if err := h.hydrateQueuedImages(&merged); err != nil {
		t.Fatal(err)
	}
	if len(merged.Message.Images) != 1 || merged.Message.Images[0].DataBase64 != tinyPNGBase64() {
		t.Fatal("image was not rehydrated")
	}
	if steering {
		merged = steeringCommandForRequest(workerInputRequest{Command: merged}, "active")
	}
	if _, err := appendAcceptedSessionInput(ctx, source, "s", merged); err != nil {
		t.Fatal(err)
	}
	if err := catalog.RecoverQueuedInputs(ctx); err != nil {
		t.Fatal(err)
	}
	// No worker is connected: recovering an already-admitted batch must not
	// attempt another admission, even when its original turn is still pending.
	if err := h.dispatchQueuedInputs("s", false, false); err != nil {
		t.Fatal(err)
	}
	rows, err = catalog.QueuedInputs(ctx, "s")
	if err != nil || len(rows) != 0 {
		t.Fatalf("duplicate pending batch: %+v %v", rows, err)
	}
}

func TestQueuePausedAfterFailure(t *testing.T) {
	ctx := context.Background()
	root := newTestStoreRoot(t)
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	catalog, err := workspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	source := storedDisplayItemSource{store: store}
	h := &handler{catalog: catalog, store: store, items: source, workers: newWorkerRegistry(), queueOps: newSessionOperationLocks()}
	for _, kind := range []zotigosession.DisplayItemType{zotigosession.DisplayItemTurnFailed, zotigosession.DisplayItemTurnInterrupted} {
		id := string(kind)
		if err := catalog.QueueInput(ctx, id, "a", []byte(`{"message":{"text":"pending"}}`)); err != nil {
			t.Fatal(err)
		}
		if _, err := source.AppendItem(ctx, id, zotigosession.DisplayItem{ID: "end", Type: kind, Turn: &zotigosession.DisplayTurn{ID: "t"}, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if err := h.dispatchQueuedInputs(id, false, true); err != nil {
			t.Fatal(err)
		}
		rows, err := catalog.QueuedInputs(ctx, id)
		if err != nil || len(rows) != 1 || rows[0].State != "paused" {
			t.Fatalf("queue=%+v %v", rows, err)
		}
	}
}
