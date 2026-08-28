package zotigod

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

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
