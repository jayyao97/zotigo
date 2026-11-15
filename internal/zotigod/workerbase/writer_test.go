package workerbase

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

func TestReliableDisplayWakeWaitsForQueueSpace(t *testing.T) {
	writer := &Writer{
		sendCh: make(chan wire.Message, 2),
		done:   make(chan struct{}),
	}
	writer.sendCh <- wire.Message{Type: wire.MessageDelta}
	writer.sendCh <- wire.Message{Type: wire.MessageDelta}
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
	if message := <-writer.sendCh; message.Type != wire.MessageDisplayWake {
		t.Fatalf("last queued message = %q, want reliable wake", message.Type)
	}
}

func TestReliableDisplayWakeTimesOutWhileQueueIsFull(t *testing.T) {
	writer := &Writer{
		displayControlTimeout: 20 * time.Millisecond,
		sendCh:                make(chan wire.Message, 1),
		done:                  make(chan struct{}),
	}
	writer.sendCh <- wire.Message{Type: wire.MessageDelta}
	err := writer.SendDisplayWakeReliable(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reliable wake error = %v, want deadline exceeded", err)
	}
	if len(writer.sendCh) != 1 {
		t.Fatalf("timed-out reliable wake altered full queue: %d", len(writer.sendCh))
	}
}

func TestDisplayBarrierQueueWaitStopsOnCancel(t *testing.T) {
	writer := &Writer{
		sendCh: make(chan wire.Message, 1),
		done:   make(chan struct{}),
	}
	writer.sendCh <- wire.Message{Type: wire.MessageDelta}
	barrier := NewDisplayBarrierClient(writer)
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
	writer := &Writer{
		sendCh: make(chan wire.Message, 1),
		done:   make(chan struct{}),
	}
	barrier := NewDisplayBarrierClient(writer)
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
	writer := &Writer{
		sendCh: make(chan wire.Message, 1),
		done:   make(chan struct{}),
	}
	writer.sendCh <- wire.Message{Type: wire.MessageDelta}
	barrier := NewDisplayBarrierClient(writer)
	barrier.timeout = 20 * time.Millisecond
	err := barrier.Wait(context.Background())
	if err == nil || !strings.Contains(err.Error(), "acknowledgement timed out") {
		t.Fatalf("barrier error = %v, want full-handshake timeout", err)
	}
	if len(writer.sendCh) != 1 {
		t.Fatalf("timed-out barrier altered full queue: %d", len(writer.sendCh))
	}
}

func TestWorkerClientWriterFailureClosesDoneAndUnblocksApproval(t *testing.T) {
	releaseServerConn := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		<-releaseServerConn
		_ = conn.Close()
	}))
	defer server.Close()
	defer close(releaseServerConn)

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	clientConn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial worker websocket: %v", err)
	}
	writer := NewWriter(clientConn, 0, 0)
	if err := clientConn.Close(); err != nil {
		t.Fatalf("close worker websocket: %v", err)
	}
	writer.sendCh <- wire.Message{Type: wire.MessageDelta, Delta: &wire.DisplayDelta{}}
	select {
	case <-writer.done:
	case <-time.After(time.Second):
		t.Fatal("writer failure did not close done")
	}
	for len(writer.sendCh) < cap(writer.sendCh) {
		writer.sendCh <- wire.Message{Type: wire.MessageDelta, Delta: &wire.DisplayDelta{}}
	}
	notified := make(chan struct{})
	go func() {
		writer.SendApprovalRequest(context.Background(), wire.ApprovalRequest{})
		close(notified)
	}()
	select {
	case <-notified:
	case <-time.After(time.Second):
		t.Fatal("approval notification remained blocked after writer failure")
	}
}

func TestDisplayRetirementWaitStopsWhenWriterCloses(t *testing.T) {
	writer := NewUnstartedWriter(nil, 1)
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
