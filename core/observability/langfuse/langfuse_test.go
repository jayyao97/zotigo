package langfuse

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/observability"
	"github.com/jayyao97/zotigo/core/protocol"
)

// captureServer is a minimal stand-in for Langfuse's ingestion endpoint.
// It records every batch posted so tests can assert event shape, count,
// and ordering without hitting the network. Handler is concurrency-safe
// because the client's flush goroutine may post during test teardown.
type captureServer struct {
	mu      sync.Mutex
	batches [][]event
	auth    string
}

func newCaptureServer(t *testing.T) (*httptest.Server, *captureServer) {
	t.Helper()
	cs := &captureServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Batch []event `json:"batch"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("server: decode body: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		cs.mu.Lock()
		cs.batches = append(cs.batches, payload.Batch)
		cs.auth = r.Header.Get("Authorization")
		cs.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	return srv, cs
}

func (c *captureServer) allEvents() []event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []event
	for _, b := range c.batches {
		out = append(out, b...)
	}
	return out
}

func TestObserver_FullTurnLifecycle_EmitsExpectedEvents(t *testing.T) {
	srv, cs := newCaptureServer(t)
	defer srv.Close()

	obs := New(Config{
		Host:          srv.URL,
		PublicKey:     "pk-test",
		SecretKey:     "sk-test",
		FlushInterval: 50 * time.Millisecond,
	})

	ctx := context.Background()
	user := protocol.NewUserMessage("hello")

	ctx = obs.StartTurn(ctx, user)
	genCtx := obs.StartGeneration(ctx, observability.GenerationMain, "claude-test", []protocol.Message{user})
	obs.EndGeneration(genCtx, observability.GenerationOutput{
		Text:         "hi back",
		FinishReason: protocol.FinishReasonStop,
	}, &protocol.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}, nil)

	toolCtx := obs.StartTool(ctx, "read_file", `{"path":"foo"}`)
	obs.EndTool(toolCtx, "file contents", nil)

	obs.EndTurn(ctx, nil)

	// Force a flush by closing — Close drains the queue synchronously.
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := obs.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	events := cs.allEvents()

	wantTypes := []string{
		"trace-create",      // StartTurn
		"generation-create", // StartGeneration
		"generation-update", // EndGeneration
		"span-create",       // StartTool
		"span-update",       // EndTool
		"trace-create",      // EndTurn (Langfuse merges by id)
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("got %d events, want %d: %+v", len(events), len(wantTypes), events)
	}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Errorf("event[%d].Type = %q, want %q", i, events[i].Type, want)
		}
	}

	// Auth header should be HTTP Basic with our test creds, base64-encoded.
	if !strings.HasPrefix(cs.auth, "Basic ") {
		t.Errorf("Authorization = %q, want Basic ...", cs.auth)
	}
}

func TestObserver_GenerationAndToolAreSiblings(t *testing.T) {
	// Generations and tool spans must hang off the trace, not nest under
	// each other — our agent runs them sequentially, not nested. If the
	// observer accidentally pushed parentObservationId to "the previous
	// span", every tool span would appear as a child of the prior
	// generation in the trace UI.
	srv, cs := newCaptureServer(t)
	defer srv.Close()

	obs := New(Config{
		Host:          srv.URL,
		PublicKey:     "pk",
		SecretKey:     "sk",
		FlushInterval: 50 * time.Millisecond,
	})

	ctx := context.Background()
	ctx = obs.StartTurn(ctx, protocol.NewUserMessage("u"))

	genCtx := obs.StartGeneration(ctx, observability.GenerationMain, "m", nil)
	obs.EndGeneration(genCtx, observability.GenerationOutput{}, nil, nil)

	// Critical: pass the trace ctx (not genCtx) to StartTool so the tool
	// span sits beside the generation, not inside it.
	toolCtx := obs.StartTool(ctx, "t", "{}")
	obs.EndTool(toolCtx, nil, nil)

	obs.EndTurn(ctx, nil)

	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := obs.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for _, ev := range cs.allEvents() {
		body, ok := ev.Body.(map[string]any)
		if !ok {
			continue
		}
		// Neither generation-create nor span-create should set
		// parentObservationId — both are direct children of the trace.
		if _, hasParent := body["parentObservationId"]; hasParent {
			t.Errorf("event %s set parentObservationId; expected siblings under trace", ev.Type)
		}
	}
}

func TestNewClient_NilOnMissingCredentials(t *testing.T) {
	if c := NewClient(Config{PublicKey: ""}); c != nil {
		t.Error("NewClient should return nil when PublicKey is empty")
	}
	if c := NewClient(Config{PublicKey: "pk", SecretKey: ""}); c != nil {
		t.Error("NewClient should return nil when SecretKey is empty")
	}
	if got := New(Config{PublicKey: ""}); got == nil {
		t.Error("New must always return a non-nil Observer (Noop fallback)")
	}
}

func TestObserver_DropsOnFullBuffer(t *testing.T) {
	// We don't want telemetry to back-pressure the agent. With a 1-slot
	// buffer and a server that never returns, the second emit must drop
	// instead of blocking.
	blockedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer blockedSrv.Close()

	obs := New(Config{
		Host:          blockedSrv.URL,
		PublicKey:     "pk",
		SecretKey:     "sk",
		BufferSize:    1,
		FlushInterval: 24 * time.Hour, // never tick on its own
	})

	done := make(chan struct{})
	go func() {
		ctx := context.Background()
		for i := 0; i < 100; i++ {
			obs.StartTool(ctx, "t", "{}")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("emit blocked when buffer was full; should drop instead")
	}
}
