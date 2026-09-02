package zotigod

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/hooks"
)

type capturingHookDispatcher struct {
	events []hooks.Event
}

func (d *capturingHookDispatcher) Dispatch(_ context.Context, event hooks.Event) hooks.DispatchResult {
	d.events = append(d.events, event)
	return hooks.DispatchResult{}
}

func TestSessionStartHookUsesActivationSource(t *testing.T) {
	dispatcher := &capturingHookDispatcher{}
	registry := newSessionRegistry()
	created := registry.Add(Session{ID: "sess-start", WorkingDirectory: t.TempDir(), Agent: "zotigo"})
	if _, err := registry.Start(created.ID); err != nil {
		t.Fatal(err)
	}
	running, err := registry.MarkRunning(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	handler := &handler{registry: registry, hooks: dispatcher}
	handler.dispatchSessionStart(running)

	if len(dispatcher.events) != 1 {
		t.Fatalf("expected one event, got %d", len(dispatcher.events))
	}
	event := dispatcher.events[0]
	if event.EventName != hooks.SessionStart || event.Session == nil || event.Session.Source != "start" {
		t.Fatalf("unexpected event: %#v", event)
	}
}

func TestReleasedIdleWorkerResumesSession(t *testing.T) {
	registry := newSessionRegistry()
	registry.Add(Session{ID: "sess-resume", State: SessionStateRunning})
	if _, err := registry.ReleaseIdleWorker("sess-resume"); err != nil {
		t.Fatal(err)
	}
	starting, err := registry.Start("sess-resume")
	if err != nil {
		t.Fatal(err)
	}
	if starting.activationSource != "resume" {
		t.Fatalf("activation source = %q, want resume", starting.activationSource)
	}
}

func TestWorkerFinishDispatchesSessionEndAfterTransition(t *testing.T) {
	dispatcher := &capturingHookDispatcher{}
	registry := newSessionRegistry()
	registry.Add(Session{
		ID: "sess-end", State: SessionStateRunning, WorkingDirectory: t.TempDir(), Agent: "zotigo",
	})
	handler := newHandler(registry, &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{
		"sess-end": {},
	}}, handlerOptions{hooks: dispatcher})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/internal/sessions/sess-end/worker/finish", nil)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if len(dispatcher.events) != 1 {
		t.Fatalf("expected one event, got %d", len(dispatcher.events))
	}
	event := dispatcher.events[0]
	if event.EventName != hooks.SessionEnd || event.Session == nil || event.Session.Result != string(SessionStateEnded) {
		t.Fatalf("unexpected event: %#v", event)
	}
}

func TestSessionEndHookIncludesModelAndUsage(t *testing.T) {
	workDir := t.TempDir()
	writeTestProfileConfig(t, workDir)
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	first := protocol.NewAssistantMessage("first")
	first.Metadata = &protocol.MessageMetadata{Usage: &protocol.Usage{
		InputTokens: 100, OutputTokens: 20, CacheReadInputTokens: 40,
	}}
	second := protocol.NewAssistantMessage("second")
	second.Metadata = &protocol.MessageMetadata{
		Usage:     &protocol.Usage{InputTokens: 80, OutputTokens: 10, CacheCreationInputTokens: 30},
		ToolUsage: &protocol.Usage{InputTokens: 5, OutputTokens: 2},
	}
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID: "sess-usage", WorkingDirectory: workDir, Agent: "zotigo", ProfileName: "test",
			CreatedAt: now, UpdatedAt: now,
		},
		AgentSnapshot: agent.Snapshot{
			// Compaction may remove older assistant messages from History. The
			// cumulative ledger must remain authoritative for lifecycle hooks.
			History:         []protocol.Message{second},
			CumulativeUsage: protocol.SessionUsage([]protocol.Message{first, second}).Normalized(),
		},
	}); err != nil {
		t.Fatal(err)
	}

	dispatcher := &capturingHookDispatcher{}
	handler := &handler{store: store, hooks: dispatcher}
	handler.dispatchSessionEnd(Session{
		ID: "sess-usage", State: SessionStateEnded, WorkingDirectory: workDir, Agent: "zotigo", ProfileName: "test",
	})

	if len(dispatcher.events) != 1 {
		t.Fatalf("expected one event, got %d", len(dispatcher.events))
	}
	payload := dispatcher.events[0].Session
	if payload == nil || payload.Model != "test" || payload.Usage == nil {
		t.Fatalf("unexpected session payload: %#v", payload)
	}
	if got, want := *payload.Usage, (hooks.UsagePayload{
		InputTokens: 185, OutputTokens: 32, TotalTokens: 287,
		CacheCreationInputTokens: 30, CacheReadInputTokens: 40,
	}); got != want {
		t.Fatalf("usage = %#v, want %#v", got, want)
	}
}
