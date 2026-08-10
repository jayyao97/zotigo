package zotigod

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/jayyao97/zotigo/core/agent"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

type receivedDisplayEvent struct {
	id        string
	eventType string
	item      itemResponse
	delta     displayDeltaEvent
}

type deadlineResponseWriter struct {
	header    http.Header
	body      bytes.Buffer
	deadlines []time.Time
}

func (w *deadlineResponseWriter) Header() http.Header {
	return w.header
}

func (w *deadlineResponseWriter) Write(data []byte) (int, error) {
	return w.body.Write(data)
}

func (w *deadlineResponseWriter) WriteHeader(int) {}

func (w *deadlineResponseWriter) Flush() {}

func (w *deadlineResponseWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadlines = append(w.deadlines, deadline)
	return nil
}

func TestSessionEventsReplaysAndStreamsDurableItemsInOrder(t *testing.T) {
	store, broker, server, sessionID := newDisplayEventTestServer(t)
	appendDisplayEventTestItem(t, store, sessionID, zotigosession.DisplayItemUserMessage)
	appendDisplayEventTestItem(t, store, sessionID, zotigosession.DisplayItemAssistantMessage)

	resp, reader := openDisplayEventStream(t, server, "/sessions/"+sessionID+"/events?after=1", "")
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("content type = %q", got)
	}
	second := readDisplayEvent(t, reader)
	if second.id != "2" || second.item.Sequence != 2 {
		t.Fatalf("initial replay = %#v", second)
	}

	third := appendDisplayEventTestItem(t, store, sessionID, zotigosession.DisplayItemTurnStarted)
	broker.Wake(sessionID)
	broker.Wake(sessionID)
	gotThird := readDisplayEvent(t, reader)
	if gotThird.id != "3" || gotThird.item.Sequence != third.Sequence {
		t.Fatalf("real-time event = %#v", gotThird)
	}

	fourth := appendDisplayEventTestItem(t, store, sessionID, zotigosession.DisplayItemAssistantMessage)
	fifth := appendDisplayEventTestItem(t, store, sessionID, zotigosession.DisplayItemTurnCompleted)
	broker.Wake(sessionID)
	gotFourth := readDisplayEvent(t, reader)
	gotFifth := readDisplayEvent(t, reader)
	if gotFourth.item.Sequence != fourth.Sequence || gotFifth.item.Sequence != fifth.Sequence {
		t.Fatalf("events out of order: %d then %d", gotFourth.item.Sequence, gotFifth.item.Sequence)
	}

	_ = resp.Body.Close()
	waitForDisplayEventSubscribers(t, broker, sessionID, 0)
}

func TestSessionEventsStreamsVolatileDeltaWithoutDurableCursor(t *testing.T) {
	store, broker, server, sessionID := newDisplayEventTestServer(t)
	resp, reader := openDisplayEventStream(t, server, "/sessions/"+sessionID+"/events", "")
	defer resp.Body.Close()

	broker.PublishDelta(sessionID, displayDeltaEvent{
		ItemID:   "item-volatile",
		Role:     "assistant",
		PartType: "text",
		Delta:    "hello",
	})
	event := readDisplayEvent(t, reader)
	if event.eventType != "delta" || event.id != "" {
		t.Fatalf("volatile event cursor contract = %#v", event)
	}
	if event.delta.ItemID != "item-volatile" || event.delta.Delta != "hello" {
		t.Fatalf("volatile delta = %#v", event.delta)
	}
	durable, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		ID:      "item-volatile",
		Type:    zotigosession.DisplayItemAssistantMessage,
		Role:    "assistant",
		Content: []zotigosession.DisplayContentPart{{Type: "text", Text: "hello"}},
	})
	if err != nil {
		t.Fatalf("persist completed block: %v", err)
	}
	broker.Wake(sessionID)
	itemEvent := readDisplayEvent(t, reader)
	if itemEvent.eventType != "item" || itemEvent.id != fmt.Sprint(durable.Sequence) || itemEvent.item.ID != "item-volatile" {
		t.Fatalf("durable reconciliation event = %#v", itemEvent)
	}
}

func TestInternalToolExecutionMarkerIsHiddenFromPublicItemsAndEvents(t *testing.T) {
	items := []zotigosession.DisplayItem{
		{ID: "public-1", Sequence: 1, Type: zotigosession.DisplayItemAssistantMessage},
		{ID: "internal-2", Sequence: 2, Type: zotigosession.DisplayItemToolExecutionStarted},
		{ID: "public-3", Sequence: 3, Type: zotigosession.DisplayItemAssistantMessage},
	}
	page := buildItemsResponse(items, zotigosession.DisplayPageQuery{Limit: 2})
	if len(page.Items) != 2 || page.Items[0].ID != "public-1" || page.Items[1].ID != "public-3" {
		t.Fatalf("internal marker affected public pagination: %#v", page.Items)
	}

	store, _, server, sessionID := newDisplayEventTestServer(t)
	if _, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type:          zotigosession.DisplayItemToolExecutionStarted,
		ToolExecution: &zotigosession.DisplayToolExecution{TurnID: "turn-1", ToolCallID: "call-1", ToolName: "shell"},
	}); err != nil {
		t.Fatalf("append internal marker: %v", err)
	}
	public, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{Type: zotigosession.DisplayItemAssistantMessage})
	if err != nil {
		t.Fatalf("append public item: %v", err)
	}
	resp, reader := openDisplayEventStream(t, server, "/sessions/"+sessionID+"/events", "")
	defer resp.Body.Close()
	event := readDisplayEvent(t, reader)
	if event.item.ID != public.ID || event.item.Sequence != public.Sequence {
		t.Fatalf("SSE exposed internal marker: %#v", event)
	}
}

func TestWorkerWebSocketForwardsVolatileDeltaToSessionEvents(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()
	const sessionID = "sess-worker-delta"
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata:      zotigosession.Metadata{ID: sessionID, CreatedAt: now, UpdatedAt: now},
		AgentSnapshot: agent.Snapshot{State: agent.StateIdle, CreatedAt: now},
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	registry := newSessionRegistry()
	registry.Add(Session{ID: sessionID, State: SessionStateRunning, Live: true})
	broker := newDisplayEventBroker()
	handler := newHandler(registry, storedDisplayItemSource{store: store}, handlerOptions{
		store:   store,
		workers: newWorkerRegistry(),
		events:  broker,
	})
	server := httptest.NewServer(handler)
	defer server.Close()

	workerConn, _ := connectWorker(t, server, sessionID)
	defer workerConn.Close()
	writer := newWorkerClientWriter(workerConn, 0, 0)
	defer writer.Close()
	resp, reader := openDisplayEventStream(t, server, "/sessions/"+sessionID+"/events", "")
	defer resp.Body.Close()

	writer.SendDelta(displayDeltaEvent{ItemID: "item-from-worker", Role: "assistant", PartType: "reasoning", Delta: "thinking"})
	event := readDisplayEvent(t, reader)
	if event.eventType != "delta" || event.id != "" || event.delta.ItemID != "item-from-worker" || event.delta.Delta != "thinking" {
		t.Fatalf("worker delta event = %#v", event)
	}
}

func TestDisplayEventWriteClearsConnectionDeadline(t *testing.T) {
	writer := &deadlineResponseWriter{header: make(http.Header)}
	if err := writeDisplayEventComment(writer, writer, "heartbeat"); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}
	if len(writer.deadlines) != 2 {
		t.Fatalf("write deadlines = %#v, want set and clear", writer.deadlines)
	}
	if writer.deadlines[0].IsZero() || !writer.deadlines[1].IsZero() {
		t.Fatalf("write deadlines = %#v, want non-zero then zero", writer.deadlines)
	}
}

func TestSessionEventsReconnectUsesAfterAndLastEventID(t *testing.T) {
	store, _, server, sessionID := newDisplayEventTestServer(t)
	for range 3 {
		appendDisplayEventTestItem(t, store, sessionID, zotigosession.DisplayItemAssistantMessage)
	}

	resp, reader := openDisplayEventStream(t, server, "/sessions/"+sessionID+"/events", "2")
	if event := readDisplayEvent(t, reader); event.item.Sequence != 3 {
		t.Fatalf("Last-Event-ID replayed sequence %d, want 3", event.item.Sequence)
	}
	_ = resp.Body.Close()

	resp, reader = openDisplayEventStream(t, server, "/sessions/"+sessionID+"/events?after=1", "2")
	if event := readDisplayEvent(t, reader); event.item.Sequence != 2 {
		t.Fatalf("after query did not take precedence: sequence %d", event.item.Sequence)
	}
	_ = resp.Body.Close()
}

func TestSessionEventsCatchesUpWhenWakeIsLost(t *testing.T) {
	store, _, server, sessionID := newDisplayEventTestServer(t)
	first := appendDisplayEventTestItem(t, store, sessionID, zotigosession.DisplayItemUserMessage)
	resp, reader := openDisplayEventStream(t, server, "/sessions/"+sessionID+"/events?after="+fmt.Sprint(first.Sequence), "")
	defer resp.Body.Close()

	second := appendDisplayEventTestItem(t, store, sessionID, zotigosession.DisplayItemAssistantMessage)
	got := readDisplayEvent(t, reader)
	if got.item.Sequence != second.Sequence {
		t.Fatalf("catch-up sequence = %d, want %d", got.item.Sequence, second.Sequence)
	}
}

func TestSessionEventsRejectsInvalidRequests(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{store: store})

	tests := []struct {
		name       string
		method     string
		path       string
		lastID     string
		wantStatus int
	}{
		{name: "unknown session", method: http.MethodGet, path: "/sessions/missing/events", wantStatus: http.StatusNotFound},
		{name: "invalid after", method: http.MethodGet, path: "/sessions/missing/events?after=abc", wantStatus: http.StatusBadRequest},
		{name: "zero after", method: http.MethodGet, path: "/sessions/missing/events?after=0", wantStatus: http.StatusBadRequest},
		{name: "invalid last id", method: http.MethodGet, path: "/sessions/missing/events", lastID: "abc", wantStatus: http.StatusBadRequest},
		{name: "wrong method", method: http.MethodPost, path: "/sessions/missing/events", wantStatus: http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tt.method, tt.path, nil)
			req.Header.Set("Last-Event-ID", tt.lastID)
			handler.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestWorkerDisplayWakeOnlySignalsDurableCatchUp(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()
	const sessionID = "sess-worker-wake"
	registry := newSessionRegistry()
	registry.Add(Session{ID: sessionID, State: SessionStateRunning, Live: true})
	workers := newWorkerRegistry()
	workers.workers[sessionID] = &workerConnection{generation: "generation-1"}
	broker := newDisplayEventBroker()
	wake, unsubscribe := broker.Subscribe(sessionID)
	defer unsubscribe()
	handler := newHandler(registry, storedDisplayItemSource{store: store}, handlerOptions{
		store:   store,
		workers: workers,
		events:  broker,
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/internal/sessions/"+sessionID+"/events/wake", strings.NewReader(`{"generation":"generation-1"}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("wake status = %d: %s", rec.Code, rec.Body.String())
	}
	select {
	case <-wake:
	case <-time.After(time.Second):
		t.Fatal("worker wake did not signal SSE catch-up")
	}
}

func TestDisplayWakeNotifierDoesNotBlockWorkerEventHandling(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-releaseRequest
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	notifier := newDisplayWakeNotifier(server.Client(), server.URL, "sess-wake-notifier", "generation-1")
	wakeReturned := make(chan struct{})
	go func() {
		notifier.Wake(context.Background())
		close(wakeReturned)
	}()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("wake sender did not issue request")
	}
	select {
	case <-wakeReturned:
	case <-time.After(time.Second):
		t.Fatal("wake blocked while the HTTP notification was in flight")
	}
	close(releaseRequest)
	notifier.Close()
}

func newDisplayEventTestServer(t *testing.T) (*zotigosession.FileStore, *displayEventBroker, *httptest.Server, string) {
	t.Helper()
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	broker := newDisplayEventBroker()
	sessionID := "sess-events"
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata:      zotigosession.Metadata{ID: sessionID, CreatedAt: now, UpdatedAt: now},
		AgentSnapshot: agent.Snapshot{State: agent.StateIdle, CreatedAt: now},
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{store: store, events: broker})
	server := httptest.NewServer(handler)
	t.Cleanup(func() {
		server.Close()
		_ = store.Close()
	})
	return store, broker, server, sessionID
}

func appendDisplayEventTestItem(t *testing.T, store *zotigosession.FileStore, sessionID string, itemType zotigosession.DisplayItemType) zotigosession.DisplayItem {
	t.Helper()
	item, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{Type: itemType})
	if err != nil {
		t.Fatalf("append display item: %v", err)
	}
	return item
}

func openDisplayEventStream(t *testing.T, server *httptest.Server, path string, lastEventID string) (*http.Response, *bufio.Reader) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
	if err != nil {
		t.Fatalf("create SSE request: %v", err)
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("open SSE stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		t.Fatalf("SSE status = %d", resp.StatusCode)
	}
	return resp, bufio.NewReader(resp.Body)
}

func readDisplayEvent(t *testing.T, reader *bufio.Reader) receivedDisplayEvent {
	t.Helper()
	result := make(chan receivedDisplayEvent, 1)
	errCh := make(chan error, 1)
	go func() {
		var event receivedDisplayEvent
		var data string
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				errCh <- err
				return
			}
			line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
			switch {
			case strings.HasPrefix(line, "id: "):
				event.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "event: "):
				event.eventType = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			case line == "" && data != "":
				var target any = &event.item
				if event.eventType == "delta" {
					target = &event.delta
				}
				if err := sonic.Unmarshal([]byte(data), target); err != nil {
					errCh <- err
					return
				}
				result <- event
				return
			case line == "":
				event = receivedDisplayEvent{}
			}
		}
	}()
	select {
	case event := <-result:
		return event
	case err := <-errCh:
		t.Fatalf("read SSE event: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SSE event")
	}
	return receivedDisplayEvent{}
}

func waitForDisplayEventSubscribers(t *testing.T, broker *displayEventBroker, sessionID string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		broker.mu.Lock()
		got := len(broker.subscribers[sessionID])
		broker.mu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("subscriber count = %d, want %d", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
