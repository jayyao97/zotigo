package zotigod

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jayyao97/zotigo/internal/zotigod/workerbase"
)

func TestSessionRegistryActivityClearsWhenIdleOrTerminal(t *testing.T) {
	registry := newSessionRegistry()
	registry.Add(Session{ID: "sess-activity", State: SessionStateRunning})
	registry.MarkWorking("sess-activity", "shell")
	working, _ := registry.Get("sess-activity")
	if !working.Working || working.ActiveTool != "shell" {
		t.Fatalf("working activity = %#v", working)
	}
	registry.MarkWorking("sess-activity", "")
	continued, _ := registry.Get("sess-activity")
	if !continued.Working || continued.ActiveTool != "" {
		t.Fatalf("continued activity = %#v", continued)
	}
	registry.MarkWorking("sess-activity", "shell")
	registry.MarkIdle("sess-activity")
	idle, _ := registry.Get("sess-activity")
	if idle.Working || idle.ActiveTool != "" {
		t.Fatalf("idle activity = %#v", idle)
	}
	registry.MarkWorking("sess-activity", "read_file")
	if _, err := registry.End("sess-activity"); err != nil {
		t.Fatalf("end session: %v", err)
	}
	ended, _ := registry.Get("sess-activity")
	if ended.Working || ended.ActiveTool != "" {
		t.Fatalf("ended activity = %#v", ended)
	}
}

func TestSessionTransitionEndpointsRejectMissingSession(t *testing.T) {
	handler := newTestHandler()

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
			var body io.Reader
			if tt.name == "worker attach" {
				body = workerReadyBody("stale")
			}
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tt.path, body))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("expected status %d, got %d", http.StatusNotFound, rec.Code)
			}
		})
	}
}

func TestSessionRegistryLifecycleTransitions(t *testing.T) {
	registry := newSessionRegistry()

	created := registry.Add(newSession("", ""))
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
			session := registry.Add(newSession("", ""))
			if err := tt.run(registry, session.ID); !errors.Is(err, errInvalidSessionTransition) {
				t.Fatalf("expected invalid transition error, got %v", err)
			}
		})
	}
}

func TestSessionRegistryRejectsMissingSession(t *testing.T) {
	registry := newSessionRegistry()

	if _, err := registry.Start("missing"); !errors.Is(err, workerbase.ErrSessionNotFound) {
		t.Fatalf("expected not found error, got %v", err)
	}
}
