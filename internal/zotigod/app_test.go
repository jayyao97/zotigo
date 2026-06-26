package zotigod

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
)

type sessionListResponse struct {
	Sessions []Session `json:"sessions"`
}

func createSession(t *testing.T, handler http.Handler) Session {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions", nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d", http.StatusCreated, rec.Code)
	}
	var session Session
	if err := sonic.Unmarshal(rec.Body.Bytes(), &session); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	return session
}

func TestHealth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	NewHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("expected JSON content type, got %q", got)
	}

	var body map[string]string
	if err := sonic.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("unexpected health response: %#v", body)
	}
}

func TestSessionsCreateAndList(t *testing.T) {
	handler := NewHandler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	var list sessionListResponse
	if err := sonic.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode initial list response: %v", err)
	}
	if len(list.Sessions) != 0 {
		t.Fatalf("expected no sessions, got %#v", list.Sessions)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions", nil))

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d", http.StatusCreated, rec.Code)
	}
	var created Session
	if err := sonic.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.ID == "" {
		t.Fatal("expected session id")
	}
	if created.State != SessionStateCreated {
		t.Fatalf("expected state %q, got %q", SessionStateCreated, created.State)
	}
	if created.CreatedAt.IsZero() {
		t.Fatal("expected created_at")
	}
	if created.StartedAt != nil {
		t.Fatalf("expected no started_at, got %v", created.StartedAt)
	}
	if created.EndedAt != nil {
		t.Fatalf("expected no ended_at, got %v", created.EndedAt)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	if err := sonic.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].ID != created.ID {
		t.Fatalf("unexpected sessions: %#v", list.Sessions)
	}
}

func TestSessionsGetByID(t *testing.T) {
	handler := NewHandler()
	created := createSession(t, handler)

	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/sessions/"+created.ID, nil))

	if getRec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, getRec.Code)
	}
	var got Session
	if err := sonic.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if got.ID != created.ID || got.State != SessionStateCreated {
		t.Fatalf("unexpected session: %#v", got)
	}
}

func TestSessionStartTransitionsToStarting(t *testing.T) {
	handler := NewHandler()
	created := createSession(t, handler)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/start", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	var started Session
	if err := sonic.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	if started.State != SessionStateStarting {
		t.Fatalf("expected state %q, got %q", SessionStateStarting, started.State)
	}
	if started.StartedAt == nil || started.StartedAt.IsZero() {
		t.Fatal("expected started_at")
	}
}

func TestWorkerAttachTransitionsToRunning(t *testing.T) {
	handler := NewHandler()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/internal/sessions/"+created.ID+"/worker/attach", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	var running Session
	if err := sonic.Unmarshal(rec.Body.Bytes(), &running); err != nil {
		t.Fatalf("decode attach response: %v", err)
	}
	if running.State != SessionStateRunning {
		t.Fatalf("expected state %q, got %q", SessionStateRunning, running.State)
	}
}

func TestWorkerFinishTransitionsToEnded(t *testing.T) {
	handler := NewHandler()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	attachWorker(t, handler, created.ID)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/internal/sessions/"+created.ID+"/worker/finish", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	var ended Session
	if err := sonic.Unmarshal(rec.Body.Bytes(), &ended); err != nil {
		t.Fatalf("decode finish response: %v", err)
	}
	if ended.State != SessionStateEnded {
		t.Fatalf("expected state %q, got %q", SessionStateEnded, ended.State)
	}
	if ended.EndedAt == nil || ended.EndedAt.IsZero() {
		t.Fatal("expected ended_at")
	}
}

func TestWorkerFinishWithErrorTransitionsToFailed(t *testing.T) {
	handler := NewHandler()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/internal/sessions/"+created.ID+"/worker/finish", strings.NewReader(`{"error":"worker exited"}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	var failed Session
	if err := sonic.Unmarshal(rec.Body.Bytes(), &failed); err != nil {
		t.Fatalf("decode finish response: %v", err)
	}
	if failed.State != SessionStateFailed {
		t.Fatalf("expected state %q, got %q", SessionStateFailed, failed.State)
	}
	if failed.Error != "worker exited" {
		t.Fatalf("expected error %q, got %q", "worker exited", failed.Error)
	}
}

func TestWorkerFinishRejectsCreatedSession(t *testing.T) {
	handler := NewHandler()
	created := createSession(t, handler)

	tests := []struct {
		name string
		body string
	}{
		{name: "without error", body: ""},
		{name: "with error", body: `{"error":"worker exited"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/internal/sessions/"+created.ID+"/worker/finish", strings.NewReader(tt.body))
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusConflict {
				t.Fatalf("expected status %d, got %d", http.StatusConflict, rec.Code)
			}
		})
	}
}

func TestSessionStartRejectsInvalidTransition(t *testing.T) {
	handler := NewHandler()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/start", nil))

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d", http.StatusConflict, rec.Code)
	}
}

func TestWorkerAttachRejectsCreatedSession(t *testing.T) {
	handler := NewHandler()
	created := createSession(t, handler)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/internal/sessions/"+created.ID+"/worker/attach", nil))

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d", http.StatusConflict, rec.Code)
	}
}

func TestPublicSessionRouteRejectsWorkerActions(t *testing.T) {
	handler := NewHandler()
	created := createSession(t, handler)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/worker/attach", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d", http.StatusNotFound, rec.Code)
	}
}

func TestSessionTransitionEndpointsRejectMissingSession(t *testing.T) {
	handler := NewHandler()

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
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tt.path, nil))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("expected status %d, got %d", http.StatusNotFound, rec.Code)
			}
		})
	}
}

func TestSessionRejectsUnknownAction(t *testing.T) {
	handler := NewHandler()
	created := createSession(t, handler)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/bogus", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d", http.StatusNotFound, rec.Code)
	}
}

func TestWorkerFinishRejectsBadJSON(t *testing.T) {
	handler := NewHandler()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/internal/sessions/"+created.ID+"/worker/finish", strings.NewReader(`{`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rec.Code)
	}
}

func TestSessionsGetByIDNotFound(t *testing.T) {
	rec := httptest.NewRecorder()

	NewHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/missing", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d", http.StatusNotFound, rec.Code)
	}
}

func TestSessionsListUsesCreationOrder(t *testing.T) {
	handler := NewHandler()
	createdIDs := make([]string, 0, 11)

	for i := 0; i < 11; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions", nil))
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %d: expected status %d, got %d", i, http.StatusCreated, rec.Code)
		}
		var created Session
		if err := sonic.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatalf("create %d: decode response: %v", i, err)
		}
		createdIDs = append(createdIDs, created.ID)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	var list sessionListResponse
	if err := sonic.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(list.Sessions) != len(createdIDs) {
		t.Fatalf("expected %d sessions, got %d", len(createdIDs), len(list.Sessions))
	}
	for i, session := range list.Sessions {
		if session.ID != createdIDs[i] {
			t.Fatalf("session %d: expected id %q, got %q", i, createdIDs[i], session.ID)
		}
	}
}

func TestSessionRegistryLifecycleTransitions(t *testing.T) {
	registry := newSessionRegistry()

	created := registry.Create()
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
			session := registry.Create()
			if err := tt.run(registry, session.ID); !errors.Is(err, errInvalidSessionTransition) {
				t.Fatalf("expected invalid transition error, got %v", err)
			}
		})
	}
}

func TestSessionRegistryRejectsMissingSession(t *testing.T) {
	registry := newSessionRegistry()

	if _, err := registry.Start("missing"); !errors.Is(err, errSessionNotFound) {
		t.Fatalf("expected not found error, got %v", err)
	}
}

func TestSessionsRejectsUnsupportedMethod(t *testing.T) {
	req := httptest.NewRequest(http.MethodDelete, "/sessions", nil)
	rec := httptest.NewRecorder()

	NewHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected status %d, got %d", http.StatusMethodNotAllowed, rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, POST" {
		t.Fatalf("expected Allow header %q, got %q", "GET, POST", got)
	}
}

func TestSessionRejectsUnsupportedMethod(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/sessions/sess-1", nil)
	rec := httptest.NewRecorder()

	NewHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected status %d, got %d", http.StatusMethodNotAllowed, rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != http.MethodGet {
		t.Fatalf("expected Allow header %q, got %q", http.MethodGet, got)
	}
}

func TestSessionStartRejectsUnsupportedMethod(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/sessions/sess-1/start", nil)
	rec := httptest.NewRecorder()

	NewHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected status %d, got %d", http.StatusMethodNotAllowed, rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != http.MethodPost {
		t.Fatalf("expected Allow header %q, got %q", http.MethodPost, got)
	}
}

func TestHealthRejectsNonGET(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/health", nil)
	rec := httptest.NewRecorder()

	NewHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected status %d, got %d", http.StatusMethodNotAllowed, rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != http.MethodGet {
		t.Fatalf("expected Allow header %q, got %q", http.MethodGet, got)
	}
}

func startSession(t *testing.T, handler http.Handler, id string) Session {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+id+"/start", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	var session Session
	if err := sonic.Unmarshal(rec.Body.Bytes(), &session); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	return session
}

func attachWorker(t *testing.T, handler http.Handler, id string) Session {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/internal/sessions/"+id+"/worker/attach", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	var session Session
	if err := sonic.Unmarshal(rec.Body.Bytes(), &session); err != nil {
		t.Fatalf("decode attach response: %v", err)
	}
	return session
}
