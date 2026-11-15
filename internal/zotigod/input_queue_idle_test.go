package zotigod

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/core/workspace"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

type blockedQueueHistory struct {
	sessionlog.ItemSource
	entered chan struct{}
	release chan struct{}
}

func (s *blockedQueueHistory) LoadItems(context.Context, string) ([]zotigosession.DisplayItem, bool, error) {
	close(s.entered)
	<-s.release
	// Another ordinary input has already started its turn while draining waited.
	return []zotigosession.DisplayItem{{Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "next-turn"}}}, true, nil
}

func TestDelayedQueueDrainDoesNotOverwriteNewWorkingState(t *testing.T) {
	catalog, err := workspace.Open(newTestStoreRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	defer catalog.Close()
	const sessionID = "idle-order"
	payload, err := json.Marshal(wire.Command{ID: "queued", Type: wire.CommandMessage, Message: &wire.MessageCommandPayload{Text: "later"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.QueueInput(context.Background(), sessionID, "queued", payload); err != nil {
		t.Fatal(err)
	}
	registry := newSessionRegistry()
	registry.Add(Session{ID: sessionID, State: SessionStateRunning, Agent: "zotigo", Working: true})
	workers := newWorkerRegistry()
	worker := newWorkerConnection(sessionID, "generation", nil, workers)
	workers.workers[sessionID] = worker
	source := &blockedQueueHistory{entered: make(chan struct{}), release: make(chan struct{})}
	_ = newHandler(registry, source, handlerOptions{catalog: catalog, workers: workers})
	workers.onMessage(sessionID, "generation", wire.Message{Type: wire.MessageIdle, Idle: &wire.Idle{CommandSequence: 0}})
	// State transitions must retain receive-loop order, even when draining is slow.
	if session, _ := registry.Get(sessionID); session.Working {
		t.Error("idle state update was deferred behind asynchronous queue draining")
	}
	select {
	case <-source.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("queue drain did not start")
	}
	submission, err := workers.BeginInput(sessionID, wire.InputRequest{Command: wire.Command{ID: "ordinary", Type: wire.CommandMessage, Message: &wire.MessageCommandPayload{Text: "new input"}}})
	if err != nil {
		close(source.release)
		t.Fatal(err)
	}
	// The real worker emits Working from startMessageTurn before its input ack.
	workers.onMessage(sessionID, "generation", wire.Message{Type: wire.MessageWorking})
	close(source.release)
	deadline := time.Now().Add(3 * time.Second)
	for {
		workers.mu.Lock()
		idlePending := worker.idlePending
		workers.mu.Unlock()
		if idlePending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("old idle callback did not finish scheduling eviction")
		}
		time.Sleep(time.Millisecond)
	}
	if session, _ := registry.Get(sessionID); !session.Working {
		t.Error("old idle notification overwrote the new turn's Working state")
	}
	// Acknowledgement advances the sequence and cancels the deferred eviction.
	workers.finishInput(worker, submission.requestID, &wire.Command{Sequence: 1})
}
