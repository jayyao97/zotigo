package zotigod

import (
	"bufio"
	"bytes"
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

func TestDisplayBarrierDropsQueuedPreviewsAndCannotBeOvertakenByLaterDelta(t *testing.T) {
	broker := newDisplayEventBroker()
	events, unsubscribe := broker.Subscribe("session-order")
	defer unsubscribe()
	for i := 0; i < cap(events); i++ {
		broker.PublishDelta("session-order", displayDeltaEvent{ItemID: fmt.Sprintf("old-%d", i), Delta: "old"})
	}

	broker.WakeBarrier("session-order")
	if event := <-events; event.delta != nil {
		t.Fatalf("barrier did not replace queued previews: %#v", event)
	}
	if len(events) != 0 {
		t.Fatalf("queued previews remained behind barrier: %d", len(events))
	}
	broker.PublishDelta("session-order", displayDeltaEvent{ItemID: "new-generation", Delta: "new"})
	if event := <-events; event.delta == nil || event.delta.ItemID != "new-generation" {
		t.Fatalf("new generation delta was not queued after barrier: %#v", event)
	}
}

func TestDisplayBarrierCoalescesExistingWakeAndFollowingPreview(t *testing.T) {
	broker := newDisplayEventBroker()
	events, unsubscribe := broker.Subscribe("session-existing-wake")
	defer unsubscribe()
	broker.Wake("session-existing-wake")
	broker.PublishDelta("session-existing-wake", displayDeltaEvent{ItemID: "old-generation", Delta: "old"})
	broker.WakeBarrier("session-existing-wake")
	if event := <-events; event.delta != nil {
		t.Fatalf("barrier event = %#v", event)
	}
	if len(events) != 0 {
		t.Fatalf("old wake or preview remained after barrier: %d", len(events))
	}
}

func TestOrderedDisplayWakeDoesNotAffectOtherSubscribers(t *testing.T) {
	broker := newDisplayEventBroker()
	full, unsubscribeFull := broker.Subscribe("full-session")
	other, unsubscribeOther := broker.Subscribe("other-session")
	for i := 0; i < cap(full); i++ {
		broker.PublishDelta("full-session", displayDeltaEvent{ItemID: fmt.Sprintf("old-%d", i), Delta: "old"})
	}
	second, unsubscribeSecond := broker.Subscribe("full-session")
	broker.WakeBarrier("full-session")
	if event := <-second; event.delta != nil {
		t.Fatalf("second subscriber did not receive ordered wake: %#v", event)
	}
	broker.PublishDelta("other-session", displayDeltaEvent{ItemID: "other", Delta: "ready"})
	if event := <-other; event.delta == nil || event.delta.ItemID != "other" {
		t.Fatalf("unrelated subscriber was stalled: %#v", event)
	}
	unsubscribeFull()
	broker.WakeBarrier("full-session")
	unsubscribeSecond()
	unsubscribeOther()
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

func TestRuntimeUpdatesRequireOptIn(t *testing.T) {
	items := []zotigosession.DisplayItem{
		{ID: "text", Sequence: 1, Type: zotigosession.DisplayItemAssistantMessage},
		{ID: "usage", Sequence: 2, Type: zotigosession.DisplayItemModelUsageUpdated, ContextUsage: &zotigosession.DisplayContextUsage{Tokens: 42, Window: 100}},
		{ID: "retry", Sequence: 3, Type: zotigosession.DisplayItemRequestRetry},
	}
	if page := buildItemsResponse(items, zotigosession.DisplayPageQuery{Limit: 10}); len(page.Items) != 1 {
		t.Fatalf("legacy page: %+v", page)
	}
	if page := buildItemsResponse(items, zotigosession.DisplayPageQuery{Limit: 10}, true); len(page.Items) != 3 || page.Items[1].ContextUsage.Tokens != 42 {
		t.Fatalf("runtime page: %+v", page)
	}
}

func TestWorkerWebSocketForwardsVolatileDeltaToSessionEvents(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
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
	writer.SendDelta(displayDeltaEvent{ItemID: "item-from-worker", Role: "assistant", PartType: "reasoning", BeforeInputID: "steering-input"})
	boundary := readDisplayEvent(t, reader)
	if boundary.eventType != "delta" || boundary.delta.BeforeInputID != "steering-input" || boundary.delta.Delta != "" {
		t.Fatalf("metadata-only ordering delta lost: %#v", boundary)
	}
}

func TestWorkerWebSocketBarrierOrdersBufferedDisplayEvents(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()
	const sessionID = "sess-worker-display-barrier"
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
	events, unsubscribe := broker.Subscribe(sessionID)
	defer unsubscribe()
	writer := &workerClientWriter{
		conn:   workerConn,
		sendCh: make(chan workerMessage, workerDeltaBufferSize),
		done:   make(chan struct{}),
	}
	barrierClient := newWorkerDisplayBarrierClient(writer)
	_, _, _, _, readErrCh := readWorkerMessages(workerConn, barrierClient.Acknowledge)
	writer.SendDelta(displayDeltaEvent{ItemID: "old", Role: "assistant", PartType: "text", Delta: "old"})
	writer.SendDisplayWake()
	barrierResult := make(chan error, 1)
	barrierCtx, cancelBarrier := context.WithTimeout(context.Background(), time.Second)
	defer cancelBarrier()
	go func() { barrierResult <- barrierClient.Wait(barrierCtx) }()
	deadline := time.Now().Add(time.Second)
	for len(writer.sendCh) < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(writer.sendCh) < 3 {
		t.Fatal("display barrier was not buffered behind old events")
	}
	go writer.run()
	defer writer.Close()
	if err := <-barrierResult; err != nil {
		t.Fatalf("wait for display barrier acknowledgement: %v", err)
	}
	writer.SendDelta(displayDeltaEvent{ItemID: "new", Role: "assistant", PartType: "text", Delta: "new"})

	sawWake := false
	eventDeadline := time.After(time.Second)
	for {
		select {
		case event := <-events:
			if event.delta == nil {
				sawWake = true
				continue
			}
			if event.delta.ItemID == "old" && sawWake {
				t.Fatal("old generation delta arrived after display barrier")
			}
			if event.delta.ItemID == "new" {
				if !sawWake {
					t.Fatal("new generation delta arrived before display barrier")
				}
				return
			}
		case err := <-readErrCh:
			t.Fatalf("read worker acknowledgement: %v", err)
		case <-eventDeadline:
			t.Fatal("timed out waiting for ordered display events")
		}
	}
}

func TestReliableDisplayWakeWaitsForQueueSpace(t *testing.T) {
	writer := &workerClientWriter{
		sendCh: make(chan workerMessage, 2),
		done:   make(chan struct{}),
	}
	writer.sendCh <- workerMessage{Type: workerMessageDelta}
	writer.sendCh <- workerMessage{Type: workerMessageDelta}
	result := make(chan error, 1)
	go func() {
		result <- writer.SendDisplayWakeReliable(context.Background())
	}()
	select {
	case err := <-result:
		t.Fatalf("reliable wake returned while queue was full: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	<-writer.sendCh
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("reliable wake: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("reliable wake did not use released queue space")
	}
	<-writer.sendCh
	if message := <-writer.sendCh; message.Type != workerMessageDisplayWake {
		t.Fatalf("last queued message = %q, want reliable wake", message.Type)
	}
}

func TestReliableDisplayWakeTimesOutWhileQueueIsFull(t *testing.T) {
	writer := &workerClientWriter{
		displayControlTimeout: 20 * time.Millisecond,
		sendCh:                make(chan workerMessage, 1),
		done:                  make(chan struct{}),
	}
	writer.sendCh <- workerMessage{Type: workerMessageDelta}
	err := writer.SendDisplayWakeReliable(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reliable wake error = %v, want deadline exceeded", err)
	}
	if len(writer.sendCh) != 1 {
		t.Fatalf("timed-out reliable wake altered full queue: %d", len(writer.sendCh))
	}
}

func TestDisplayBarrierQueueWaitStopsOnCancel(t *testing.T) {
	writer := &workerClientWriter{
		sendCh: make(chan workerMessage, 1),
		done:   make(chan struct{}),
	}
	writer.sendCh <- workerMessage{Type: workerMessageDelta}
	barrier := newWorkerDisplayBarrierClient(writer)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- barrier.Wait(ctx) }()
	select {
	case err := <-result:
		t.Fatalf("barrier returned before cancellation: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("barrier error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("barrier remained blocked after context cancellation")
	}
	barrier.mu.Lock()
	waiterCount := len(barrier.waiters)
	barrier.mu.Unlock()
	if waiterCount != 0 {
		t.Fatalf("barrier waiters leaked after cancellation: %d", waiterCount)
	}
}

func TestDisplayBarrierWaitTimesOutWithoutAcknowledgement(t *testing.T) {
	writer := &workerClientWriter{
		sendCh: make(chan workerMessage, 1),
		done:   make(chan struct{}),
	}
	barrier := newWorkerDisplayBarrierClient(writer)
	barrier.timeout = 20 * time.Millisecond
	started := time.Now()
	err := barrier.Wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "acknowledgement timed out") {
		t.Fatalf("barrier error = %v, want acknowledgement timeout", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("barrier acknowledgement timeout was not bounded")
	}
	barrier.mu.Lock()
	waiterCount := len(barrier.waiters)
	barrier.mu.Unlock()
	if waiterCount != 0 {
		t.Fatalf("barrier waiters leaked after timeout: %d", waiterCount)
	}
}

func TestDisplayBarrierWaitTimesOutWhileQueueIsFull(t *testing.T) {
	writer := &workerClientWriter{
		sendCh: make(chan workerMessage, 1),
		done:   make(chan struct{}),
	}
	writer.sendCh <- workerMessage{Type: workerMessageDelta}
	barrier := newWorkerDisplayBarrierClient(writer)
	barrier.timeout = 20 * time.Millisecond
	err := barrier.Wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "acknowledgement timed out") {
		t.Fatalf("barrier error = %v, want full-handshake timeout", err)
	}
	if len(writer.sendCh) != 1 {
		t.Fatalf("timed-out barrier altered full queue: %d", len(writer.sendCh))
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
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
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

func newDisplayEventTestServer(t *testing.T) (*zotigosession.FileStore, *displayEventBroker, *httptest.Server, string) {
	t.Helper()
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
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

func TestSessionEventsRestoresUnfinishedPrefixAfterSwitch(t *testing.T) {
	store, broker, server, sessionID := newDisplayEventTestServer(t)
	started := appendDisplayEventTestItem(t, store, sessionID, zotigosession.DisplayItemTurnStarted)
	path := fmt.Sprintf("/sessions/%s/events?after=%d", sessionID, started.Sequence)
	first, reader := openDisplayEventStream(t, server, path, "")
	delta := displayDeltaEvent{ItemID: "streaming-body", Role: "assistant", PartType: "text", Delta: "前半段 **"}
	broker.PublishDelta(sessionID, delta)
	if got := readDisplayEvent(t, reader); got.delta.Delta != delta.Delta {
		t.Fatalf("first delta = %#v", got)
	}
	_ = first.Body.Close()
	waitForDisplayEventSubscribers(t, broker, sessionID, 0)
	delta.Delta = "重点**，切走时继续输出。"
	broker.PublishDelta(sessionID, delta)
	broker.PublishDelta(sessionID, displayDeltaEvent{ItemID: delta.ItemID, Role: "assistant", PartType: "text", BeforeInputID: "steering-1"})
	second, reader := openDisplayEventStream(t, server, path, "")
	defer second.Body.Close()
	replay := readDisplayEvent(t, reader)
	prefix := "前半段 **重点**，切走时继续输出。"
	if replay.eventType != "delta" || replay.id != "" || replay.delta.Delta != prefix || replay.delta.BeforeInputID != "steering-1" {
		t.Fatalf("restored prefix = %#v", replay)
	}
	delta.Delta = "最后一段。"
	broker.PublishDelta(sessionID, delta)
	if got := readDisplayEvent(t, reader); got.delta.Delta != delta.Delta {
		t.Fatalf("new tail = %#v", got)
	}
	stored, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		ID: delta.ItemID, Type: zotigosession.DisplayItemAssistantMessage, Role: "assistant",
		Content: []zotigosession.DisplayContentPart{{Type: "text", Text: prefix + delta.Delta}},
	})
	if err != nil {
		t.Fatal(err)
	}
	broker.Wake(sessionID)
	if got := readDisplayEvent(t, reader); got.eventType != "item" || got.item.ID != stored.ID {
		t.Fatalf("completed block = %#v", got)
	}
	if broker.hasPreview(sessionID, delta.ItemID) {
		t.Fatal("completed prefix retained")
	}
}

func TestDisplayPrefixSubscriptionBoundaryAndRetirement(t *testing.T) {
	broker := newDisplayEventBroker()
	prefix := strings.Repeat("正文🙂", 5000)
	broker.PublishDelta("one", displayDeltaEvent{ItemID: "body", Role: "assistant", PartType: "text", Delta: prefix})
	broker.PublishDelta("one", displayDeltaEvent{ItemID: "child", Role: "assistant", PartType: "reasoning", Delta: "child prefix", Subagent: &zotigosession.DisplaySubagent{ToolCallID: "spawn-1"}})
	broker.PublishDelta("two", displayDeltaEvent{ItemID: "other", Role: "assistant", PartType: "text", Delta: "private"})
	events, snapshot, cancel := broker.subscribeWithPreviews("one")
	defer cancel()
	broker.PublishDelta("one", displayDeltaEvent{ItemID: "body", Role: "assistant", PartType: "text", Delta: "tail"})
	if len(snapshot) != 2 || snapshot[0].Delta != prefix || snapshot[1].Delta != "child prefix" {
		t.Fatal("snapshot is incomplete or contaminated by another session/later delta")
	}
	if event := <-events; event.delta == nil || event.delta.Delta != "tail" {
		t.Fatalf("subscription boundary = %#v", event)
	}
	broker.Retire("one", previewRetirement(zotigosession.DisplayItem{ID: "result", Content: []zotigosession.DisplayContentPart{{Type: "tool_result", ToolResult: &zotigosession.DisplayToolResult{ToolCallID: "spawn-1"}}}}))
	if broker.hasPreview("one", "child") || !broker.hasPreview("one", "body") {
		t.Fatal("tool retirement removed wrong blocks")
	}
	broker.Retire("one", displayPreviewRetirement{Terminal: true})
	if broker.hasPreview("one", "body") || !broker.hasPreview("two", "other") {
		t.Fatal("terminal retirement leaked across sessions")
	}
}

func TestSessionEventsDoesNotReplayAlreadyDurablePrefix(t *testing.T) {
	store, broker, server, sessionID := newDisplayEventTestServer(t)
	broker.PublishDelta(sessionID, displayDeltaEvent{ItemID: "completed", Role: "assistant", PartType: "text", Delta: "already saved"})
	stored, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{ID: "completed", Type: zotigosession.DisplayItemAssistantMessage})
	if err != nil {
		t.Fatal(err)
	}
	// Even if a completion notification is dropped, catch-up must discard the
	// prefix when the durable item is at/before the client's existing cursor.
	response, reader := openDisplayEventStream(t, server, fmt.Sprintf("/sessions/%s/events?after=%d", sessionID, stored.Sequence), "")
	defer response.Body.Close()
	broker.PublishDelta(sessionID, displayDeltaEvent{ItemID: "next", Role: "assistant", PartType: "text", Delta: "new block"})
	event := readDisplayEvent(t, reader)
	if event.delta.ItemID != "next" {
		t.Fatalf("replayed completed preview: %#v", event)
	}
}

func TestWorkersNotifyPreviewRetirementAfterPersistence(t *testing.T) {
	for _, kind := range []string{"native", "codex"} {
		t.Run(kind, func(t *testing.T) {
			store, _, _, sessionID := newDisplayEventTestServer(t)
			writer := &workerClientWriter{sendCh: make(chan workerMessage, 4), done: make(chan struct{})}
			item := zotigosession.DisplayItem{ID: "finished-block", Type: zotigosession.DisplayItemAssistantMessage, Content: []zotigosession.DisplayContentPart{{Type: "text", Text: "body"}}}
			if kind == "native" {
				log := newWorkerDisplayLog(sessionID, storedDisplayItemSource{store: store})
				log.persisted = writer.SendDisplayItem
				if _, err := log.appendItem(context.Background(), item); err != nil {
					t.Fatal(err)
				}
			} else {
				runtime := &codexWorkerRuntime{cfg: codexWorkerConfig{workerClientConfig: workerClientConfig{SessionID: sessionID}}, store: store, writer: writer}
				if err := runtime.append(item); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case message := <-writer.sendCh:
				if message.Type != workerMessageDisplayWake || message.PreviewRetirement == nil || message.PreviewRetirement.ItemID != item.ID {
					t.Fatalf("completion notification = %#v", message)
				}
				items, _, err := store.ListDisplayItems(context.Background(), sessionID)
				if err != nil || len(items) != 1 || items[0].ID != item.ID {
					t.Fatalf("not durable before notification: %#v, %v", items, err)
				}
			default:
				t.Fatal("missing completion notification")
			}
		})
	}
}

func TestDisplayRetirementSurvivesFullQueueWithoutSubscribers(t *testing.T) {
	const sessionID = "sess-retirement-backpressure"
	registry := newSessionRegistry()
	registry.Add(Session{ID: sessionID, State: SessionStateRunning, Live: true})
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	broker := newDisplayEventBroker()
	server := httptest.NewServer(newHandler(registry, source, handlerOptions{events: broker}))
	defer server.Close()
	conn, _ := connectWorker(t, server, sessionID)
	defer conn.Close()
	writer := &workerClientWriter{conn: conn, sendCh: make(chan workerMessage, 1), done: make(chan struct{})}
	defer writer.Close()
	broker.PublishDelta(sessionID, displayDeltaEvent{ItemID: "finished", Role: "assistant", PartType: "text", Delta: "completed body"})
	stored, err := source.AppendItem(context.Background(), sessionID, zotigosession.DisplayItem{ID: "finished", Type: zotigosession.DisplayItemAssistantMessage})
	if err != nil {
		t.Fatal(err)
	}
	writer.SendDisplayWake() // Fill the queue before the writer starts draining.
	sent := make(chan struct{})
	go func() { writer.SendDisplayItem(stored); close(sent) }()
	select {
	case <-sent:
		t.Fatal("completion was dropped when the queue was full")
	case <-time.After(20 * time.Millisecond):
	}
	go writer.run()
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("completion did not enqueue after queue space became available")
	}
	// No SSE subscription or history catch-up is involved in this cleanup.
	// Allow the first WebSocket JSON decode to initialize under race instrumentation.
	for deadline := time.Now().Add(5 * time.Second); broker.hasPreview(sessionID, stored.ID) && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if broker.hasPreview(sessionID, stored.ID) {
		t.Fatal("completed prefix retained without subscribers")
	}
}

func TestDisplayRetirementWaitStopsWhenWriterCloses(t *testing.T) {
	writer := &workerClientWriter{sendCh: make(chan workerMessage, 1), done: make(chan struct{})}
	defer writer.Close()
	writer.SendDisplayWake()
	sent := make(chan struct{})
	go func() { writer.SendDisplayItem(zotigosession.DisplayItem{ID: "finished"}); close(sent) }()
	select {
	case <-sent:
		t.Fatal("completion was dropped when the queue was full")
	case <-time.After(20 * time.Millisecond):
	}
	writer.Close()
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("closed writer left completion sender blocked")
	}
}
