package zotigod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/compaction"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/channels"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/workerbase"
)

func TestHealth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	newTestHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("expected JSON content type, got %q", got)
	}

	var body map[string]string
	if err := decodeAPIData(t, rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["status"] != "ok" {
		t.Fatalf("unexpected health response: %#v", body)
	}
	if body["version"] == "" || body["commit"] == "" {
		t.Fatalf("missing build identity: %#v", body)
	}
	if body["process_id"] != strconv.Itoa(os.Getpid()) {
		t.Fatalf("unexpected daemon process identity: %#v", body)
	}
	if body["protocol_version"] != apiProtocolVersion {
		t.Fatalf("protocol_version = %q, want %q", body["protocol_version"], apiProtocolVersion)
	}
}

func TestDecorateSessionDoesNotReportInactiveSessionWorking(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{
		"sess-failed": {
			{Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
			{Type: zotigosession.DisplayItemToolExecutionStarted, ToolExecution: &zotigosession.DisplayToolExecution{ToolCallID: "call-1", ToolName: "shell"}},
		},
	}}
	handler := &handler{items: source}

	for _, state := range []SessionState{SessionStateCreated, SessionStateOffline, SessionStateEnded, SessionStateFailed} {
		got := handler.decorateSession(context.Background(), Session{
			ID: "sess-failed", State: state, Working: true, ActiveTool: "stale-tool",
		}, nil)
		if got.Working {
			t.Fatalf("%s session must not be reported as working", state)
		}
		if got.ActiveTool != "" {
			t.Fatalf("%s session active tool = %q, want empty", state, got.ActiveTool)
		}
	}
}

func TestSessionsCreateAndList(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	handler := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{store: store})
	workDir := t.TempDir()
	writeTestProfileConfig(t, workDir)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	var list sessionListResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode initial list response: %v", err)
	}
	if len(list.Sessions) != 0 {
		t.Fatalf("expected no sessions, got %#v", list.Sessions)
	}

	rec = httptest.NewRecorder()
	body := fmt.Sprintf(`{"working_directory":%q}`, workDir)
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(body)))

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d", http.StatusCreated, rec.Code)
	}
	var created Session
	if err := decodeAPIData(t, rec.Body.Bytes(), &created); err != nil {
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
	if !created.UpdatedAt.Equal(created.CreatedAt) {
		t.Fatalf("updated_at = %s, want created_at %s", created.UpdatedAt, created.CreatedAt)
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
	if err := decodeAPIData(t, rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].ID != created.ID {
		t.Fatalf("unexpected sessions: %#v", list.Sessions)
	}
}

func TestSessionsCreatePersistsWorkingDirectory(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	handler := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{store: store})
	workDir := t.TempDir()
	writeTestProfileConfig(t, workDir)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(fmt.Sprintf(`{"working_directory":%q}`, workDir)))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	var created Session
	if err := decodeAPIData(t, rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.WorkingDirectory != workDir {
		t.Fatalf("expected working directory %q, got %q", workDir, created.WorkingDirectory)
	}

	stored, err := store.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("load stored session: %v", err)
	}
	if stored == nil || stored.WorkingDirectory != workDir {
		t.Fatalf("expected stored working directory %q, got %#v", workDir, stored)
	}
}

func TestSessionsCreatePersistsSelectedProfile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer store.Close()
	handler := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{store: store})
	workDir := t.TempDir()
	projectConfig := `
default_profile: project-default
profiles:
  project-default:
    provider: openai
    model: gpt-default
  project-high:
    provider: openai
    model: gpt-high
    thinking_level: high
`
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}

	body := fmt.Sprintf(`{"working_directory":%q,"profile":"project-high"}`, workDir)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	var created Session
	if err := decodeAPIData(t, rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.ProfileName != "project-high" {
		t.Fatalf("expected selected profile project-high, got %q", created.ProfileName)
	}
	stored, err := store.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("load stored session: %v", err)
	}
	if stored == nil || stored.ProfileName != "project-high" {
		t.Fatalf("expected stored profile project-high, got %#v", stored)
	}
	listed, err := store.List(context.Background(), zotigosession.ListFilter{})
	if err != nil {
		t.Fatalf("list stored sessions: %v", err)
	}
	if len(listed) != 1 || listed[0].ProfileName != "project-high" {
		t.Fatalf("expected indexed profile project-high, got %#v", listed)
	}
	restarted := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{store: store})
	getRec := httptest.NewRecorder()
	restarted.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/sessions/"+created.ID, nil))
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected restarted GET status %d, got %d: %s", http.StatusOK, getRec.Code, getRec.Body.String())
	}
	var offline Session
	if err := decodeAPIData(t, getRec.Body.Bytes(), &offline); err != nil {
		t.Fatalf("decode restarted session: %v", err)
	}
	if offline.ProfileName != "project-high" || offline.Live {
		t.Fatalf("expected offline session with persisted profile, got %#v", offline)
	}
}

func TestSessionsCreatePersistsProjectDefaultProfile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer store.Close()
	handler := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{store: store})
	workDir := t.TempDir()
	projectConfig := "default_profile: project-default\nprofiles:\n  project-default:\n    provider: openai\n    model: gpt-default\n"
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}

	body := fmt.Sprintf(`{"working_directory":%q}`, workDir)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	var created Session
	if err := decodeAPIData(t, rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.ProfileName != "project-default" {
		t.Fatalf("expected project default profile, got %q", created.ProfileName)
	}
}

func TestSessionsCreateRejectsUnknownProfile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer store.Close()
	registry := newSessionRegistry()
	handler := newHandler(registry, sessionlog.StoredItemSource{Store: store}, handlerOptions{store: store})
	workDir := t.TempDir()

	body := fmt.Sprintf(`{"working_directory":%q,"profile":"missing"}`, workDir)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(body)))

	assertAPIError(t, rec, http.StatusBadRequest, "invalid_request", `profile "missing" not found`)
	if got := registry.List(); len(got) != 0 {
		t.Fatalf("expected rejected profile not to create a session, got %#v", got)
	}
}

func TestSessionsCreateRejectsInvalidWorkingDirectory(t *testing.T) {
	handler := newTestHandler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(`{"working_directory":"/path/that/does/not/exist"}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(`{"working_directory":"relative/project"}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
}

func TestSessionsCreateDoesNotRegisterWhenPersistenceFails(t *testing.T) {
	registry := newSessionRegistry()
	handler := newHandler(registry, &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}, handlerOptions{
		store: unavailableSessionStore{err: errors.New("store unavailable")},
	})
	workDir := t.TempDir()
	writeTestProfileConfig(t, workDir)

	rec := httptest.NewRecorder()
	body := fmt.Sprintf(`{"working_directory":%q}`, workDir)
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(body)))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d: %s", http.StatusInternalServerError, rec.Code, rec.Body.String())
	}
	if got := registry.List(); len(got) != 0 {
		t.Fatalf("expected no registered sessions after persistence failure, got %#v", got)
	}
}

func TestDefaultHandlerRejectsCreateWhenStoreInitializationFails(t *testing.T) {
	homeFile := filepath.Join(t.TempDir(), "home-file")
	if err := os.WriteFile(homeFile, []byte("not a directory"), 0644); err != nil {
		t.Fatalf("write home file: %v", err)
	}
	t.Setenv("HOME", homeFile)
	handler := newTestHandler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d: %s", http.StatusInternalServerError, rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d: %s", http.StatusInternalServerError, rec.Code, rec.Body.String())
	}
}

func TestSessionsGetByID(t *testing.T) {
	handler := newTestHandler()
	created := createSession(t, handler)

	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/sessions/"+created.ID, nil))

	if getRec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, getRec.Code)
	}
	var got Session
	if err := decodeAPIData(t, getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if got.ID != created.ID || got.State != SessionStateCreated {
		t.Fatalf("unexpected session: %#v", got)
	}
	if got.ContextUsage == nil || got.ContextUsage.Status != "unavailable" {
		t.Fatalf("new session context usage = %#v", got.ContextUsage)
	}
	if strings.Contains(getRec.Body.String(), `"context_usage":null`) {
		t.Fatalf("session response returned null context usage: %s", getRec.Body.String())
	}
}

func TestSessionsGetLiveFallsBackToRegistryWhenStoreFails(t *testing.T) {
	registry := newSessionRegistry()
	registry.Add(Session{
		ID:          "sess-live-store-failure",
		State:       SessionStateRunning,
		ProfileName: "registry-profile",
		CreatedAt:   time.Now().UTC(),
	})
	handler := newHandler(registry, &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}, handlerOptions{
		store: unavailableSessionStore{err: errors.New("store unavailable")},
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/sess-live-store-failure", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected live registry fallback status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var session Session
	if err := decodeAPIData(t, rec.Body.Bytes(), &session); err != nil {
		t.Fatalf("decode live session: %v", err)
	}
	if !session.Live || session.State != SessionStateRunning || session.ProfileName != "registry-profile" {
		t.Fatalf("unexpected live registry fallback: %#v", session)
	}
}

func TestSessionsGetReturnsStoredSessionOffline(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	workDir := t.TempDir()
	putStoredSession(t, store, "sess-stored", workDir)
	handler := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{store: store})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/sess-stored", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var got Session
	if err := decodeAPIData(t, rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if got.ID != "sess-stored" || got.State != SessionStateOffline || got.Live {
		t.Fatalf("expected stored offline session, got %#v", got)
	}
	if got.WorkingDirectory != workDir {
		t.Fatalf("expected working directory %q, got %q", workDir, got.WorkingDirectory)
	}
}

func TestSessionsListMergesRegistryAndStoredSessions(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	workDir := t.TempDir()
	putStoredSession(t, store, "sess-stored", workDir)
	handler := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{store: store})
	created := createSession(t, handler)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var list sessionListResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	seen := map[string]Session{}
	for _, session := range list.Sessions {
		seen[session.ID] = session
	}
	if len(seen) != 2 {
		t.Fatalf("expected two unique sessions, got %#v", list.Sessions)
	}
	if !seen[created.ID].Live || seen[created.ID].State != SessionStateCreated {
		t.Fatalf("expected registry session to remain live, got %#v", seen[created.ID])
	}
	if seen["sess-stored"].Live || seen["sess-stored"].State != SessionStateOffline {
		t.Fatalf("expected stored session to be offline, got %#v", seen["sess-stored"])
	}
}

func TestSessionsListDoesNotLoadSessionHistories(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	putStoredSession(t, store, "sess-stored", t.TempDir())
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	registry := newSessionRegistry()
	registry.Add(Session{ID: "sess-live", State: SessionStateRunning, Working: true, ActiveTool: "shell"})
	handler := newHandler(registry, source, handlerOptions{store: store})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if got := source.loadCalls.Load(); got != 0 {
		t.Fatalf("session list loaded %d display histories, want 0", got)
	}
	var list sessionListResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	for _, session := range list.Sessions {
		if session.ID == "sess-live" && (!session.Working || session.ActiveTool != "shell") {
			t.Fatalf("live activity lost from list response: %#v", session)
		}
	}
}

func TestReadSessionAPIsDoNotLaunchWorker(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	putStoredSession(t, store, "sess-stored", t.TempDir())
	var launches atomic.Int32
	handler := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{
		store: store,
		launcher: workerLauncherFunc(func(context.Context, string, string) error {
			launches.Add(1)
			return nil
		}),
		workerConnectTimeout: time.Millisecond,
	})

	for _, path := range []string{"/sessions", "/sessions/sess-stored", "/sessions/sess-stored/items"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: expected status %d, got %d: %s", path, http.StatusOK, rec.Code, rec.Body.String())
		}
	}
	if got := launches.Load(); got != 0 {
		t.Fatalf("expected read APIs not to launch workers, got %d launches", got)
	}
}

func TestSessionItemsRejectMissingSession(t *testing.T) {
	handler := newHandler(newSessionRegistry(), &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/missing/items", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d", http.StatusNotFound, rec.Code)
	}
}

func TestSessionItemsDefaultLimitReturnsRecentItemsAscending(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	created := createSession(t, handler)
	source.items[created.ID] = makeDisplayItems(60)

	resp := getItems(t, handler, "/sessions/"+created.ID+"/items")

	if len(resp.Items) != defaultItemsLimit {
		t.Fatalf("expected %d items, got %d", defaultItemsLimit, len(resp.Items))
	}
	if resp.Items[0].Sequence != 11 || resp.Items[49].Sequence != 60 {
		t.Fatalf("expected recent 11..60, got %d..%d", resp.Items[0].Sequence, resp.Items[49].Sequence)
	}
	if resp.PrevCursor != "11" || resp.NextCursor != "" || !resp.HasMore {
		t.Fatalf("unexpected cursors: %#v", resp)
	}
}

func TestSessionItemsLimitMax(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	created := createSession(t, handler)
	source.items[created.ID] = makeDisplayItems(210)

	resp := getItems(t, handler, "/sessions/"+created.ID+"/items?limit=200")

	if len(resp.Items) != maxItemsLimit {
		t.Fatalf("expected %d items, got %d", maxItemsLimit, len(resp.Items))
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/"+created.ID+"/items?limit=201", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rec.Code)
	}
}

func TestSessionItemsCursorPagination(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	created := createSession(t, handler)
	source.items[created.ID] = makeDisplayItems(5)

	after := getItems(t, handler, "/sessions/"+created.ID+"/items?after=2&limit=2")
	assertItemSequences(t, after.Items, []uint64{3, 4})

	before := getItems(t, handler, "/sessions/"+created.ID+"/items?before=5&limit=2")
	assertItemSequences(t, before.Items, []uint64{3, 4})
}

func TestSessionItemsRejectInvalidCursor(t *testing.T) {
	handler := newHandler(newSessionRegistry(), &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}})

	tests := []string{
		"/sessions/missing/items?after=abc",
		"/sessions/missing/items?before=0",
		"/sessions/missing/items?after=1&before=2",
	}
	for _, path := range tests {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rec.Code)
			}
		})
	}
}

func TestSessionItemsPublicResponseDoesNotExposeInternalSession(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	created := createSession(t, handler)
	source.items[created.ID] = makeDisplayItems(1)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/"+created.ID+"/items", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	body := rec.Body.String()
	for _, internalField := range []string{"agent_snapshot", "display_log", "turns"} {
		if strings.Contains(body, internalField) {
			t.Fatalf("response leaked internal field %q: %s", internalField, body)
		}
	}
}

func TestSessionItemsReturnsStructuredToolResult(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	created := createSession(t, handler)
	source.items[created.ID] = []zotigosession.DisplayItem{{
		ID:       "item_tool_result",
		Sequence: 1,
		Type:     zotigosession.DisplayItemAssistantMessage,
		Role:     string(protocol.RoleAssistant),
		Content: []zotigosession.DisplayContentPart{{
			Type: string(protocol.ContentTypeToolResult),
			ToolResult: &zotigosession.DisplayToolResult{
				ToolCallID: "call-1",
				ToolName:   "screenshot",
				ResultType: string(protocol.ToolResultTypeContent),
				Content: []zotigosession.DisplayToolResultContentPart{{
					Type: string(protocol.ContentTypeText),
					Text: "captured",
				}, {
					Type: string(protocol.ContentTypeImage),
					Image: &zotigosession.DisplayMediaPart{
						URL:       "file:///tmp/screenshot.png",
						MediaType: "image/png",
					},
				}},
				Metadata: map[string]any{
					"private": "must-not-leak",
					"subagent": map[string]any{
						"name": "inspect-code",
						"history": []protocol.Message{{
							Role:     protocol.RoleAssistant,
							Metadata: &protocol.MessageMetadata{Raw: map[string]any{"secret": "provider-continuation"}},
							Content: []protocol.ContentPart{{
								Type: protocol.ContentTypeReasoning, Text: "thinking", Signature: "anthropic-signature", EncryptedContent: "openai-encrypted",
							}, {
								Type: protocol.ContentTypeImage, Image: &protocol.MediaPart{Data: []byte("raw-image"), URL: "file:///tmp/child.png", MediaType: "image/png"},
							}, {
								Type:     protocol.ContentTypeToolCall,
								ToolCall: &protocol.ToolCall{ID: "child-call-1", Name: "read_file", Arguments: `{"path":"foo.go"}`},
							}, {
								Type:       protocol.ContentTypeToolResult,
								ToolResult: &protocol.ToolResult{ToolCallID: "child-call-1", Type: protocol.ToolResultTypeText, Text: "ok", Metadata: map[string]any{"nested-secret": "must-not-leak"}},
							}},
						}},
					},
				},
			},
		}},
		CreatedAt: time.Now().UTC(),
	}}

	resp := getItems(t, handler, "/sessions/"+created.ID+"/items")

	if len(resp.Items) != 1 || len(resp.Items[0].Content) != 1 {
		t.Fatalf("unexpected items response: %#v", resp)
	}
	result := resp.Items[0].Content[0].ToolResult
	if result == nil {
		t.Fatalf("expected structured tool result, got %#v", resp.Items[0].Content[0])
	}
	if result.ToolCallID != "call-1" || result.ToolName != "screenshot" || result.ResultType != string(protocol.ToolResultTypeContent) {
		t.Fatalf("unexpected tool result: %#v", result)
	}
	if len(result.Content) != 2 {
		t.Fatalf("expected structured content parts, got %#v", result.Content)
	}
	if result.Content[0].Type != string(protocol.ContentTypeText) || result.Content[0].Text != "captured" {
		t.Fatalf("unexpected text content part: %#v", result.Content[0])
	}
	if result.Content[1].Image == nil || result.Content[1].Image.URL != "file:///tmp/screenshot.png" {
		t.Fatalf("unexpected image content part: %#v", result.Content[1])
	}
	subagent, ok := result.Metadata["subagent"].(map[string]any)
	if !ok || subagent["name"] != "inspect-code" {
		t.Fatalf("expected public tool result to retain structured subagent metadata, got %#v", result.Metadata)
	}
	publicMetadata, err := json.Marshal(result.Metadata)
	if err != nil {
		t.Fatalf("marshal public metadata: %v", err)
	}
	for _, secret := range []string{"must-not-leak", "provider-continuation", "anthropic-signature", "openai-encrypted", "raw-image"} {
		if bytes.Contains(publicMetadata, []byte(secret)) {
			t.Fatalf("public tool metadata leaked %q: %s", secret, publicMetadata)
		}
	}
	if !bytes.Contains(publicMetadata, []byte("child-call-1")) || !bytes.Contains(publicMetadata, []byte("file:///tmp/child.png")) {
		t.Fatalf("public subagent history lost safe display fields: %s", publicMetadata)
	}
}

func TestSessionItemsReturnsContextCompaction(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	created := createSession(t, handler)
	source.items[created.ID] = []zotigosession.DisplayItem{{
		ID: "item_compacted", Sequence: 1, Type: zotigosession.DisplayItemContextCompacted,
		ContextCompaction: &zotigosession.DisplayContextCompaction{
			OriginalTokens: 1200, CompressedTokens: 300, MessagesBefore: 20, MessagesAfter: 6,
		},
		CreatedAt: time.Now().UTC(),
	}}

	resp := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(resp.Items) != 1 || resp.Items[0].ContextCompaction == nil {
		t.Fatalf("expected public context compaction metadata, got %#v", resp.Items)
	}
	if got := resp.Items[0].ContextCompaction; got.OriginalTokens != 1200 || got.CompressedTokens != 300 || got.MessagesBefore != 20 || got.MessagesAfter != 6 {
		t.Fatalf("unexpected public context compaction metadata: %#v", got)
	}
}

func TestSessionItemsReturnsStructuredToolCall(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	created := createSession(t, handler)
	source.items[created.ID] = []zotigosession.DisplayItem{{
		ID:       "item_tool_call",
		Sequence: 1,
		Type:     zotigosession.DisplayItemAssistantMessage,
		Role:     string(protocol.RoleAssistant),
		Content: []zotigosession.DisplayContentPart{{
			Type: string(protocol.ContentTypeToolCall),
			ToolCall: &zotigosession.DisplayToolCall{
				ID:        "call-1",
				Name:      "shell",
				Arguments: `{"command":"git status"}`,
			},
		}},
		CreatedAt: time.Now().UTC(),
	}}

	resp := getItems(t, handler, "/sessions/"+created.ID+"/items")

	if len(resp.Items) != 1 || len(resp.Items[0].Content) != 1 {
		t.Fatalf("unexpected items response: %#v", resp)
	}
	call := resp.Items[0].Content[0].ToolCall
	if call == nil {
		t.Fatalf("expected structured tool call, got %#v", resp.Items[0].Content[0])
	}
	if call.ID != "call-1" || call.Name != "shell" || call.Arguments != `{"command":"git status"}` {
		t.Fatalf("unexpected tool call: %#v", call)
	}
}

func TestSessionItemsNormalizesHistoricalCodexFileChangeArguments(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	created := createSession(t, handler)
	source.items[created.ID] = []zotigosession.DisplayItem{{
		ID: "item_file_change", Sequence: 1, Type: zotigosession.DisplayItemAssistantMessage,
		Content: []zotigosession.DisplayContentPart{{
			Type: string(protocol.ContentTypeToolCall),
			ToolCall: &zotigosession.DisplayToolCall{
				ID: "file-1", Name: "apply_patch",
				Arguments: `{"changes":[{"path":"a.go","kind":{"type":"update"},"diff":"*** Begin Patch\n*** Update File: a.go\n@@\n-old\n+new\n*** End Patch"}]}`,
			},
		}}, CreatedAt: time.Now().UTC(),
	}}

	resp := getItems(t, handler, "/sessions/"+created.ID+"/items")
	call := resp.Items[0].Content[0].ToolCall
	var input map[string]any
	if call == nil {
		t.Fatal("missing tool call")
	}
	if err := json.Unmarshal([]byte(call.Arguments), &input); err != nil {
		t.Fatal(err)
	}
	if input["files"] != "a.go" || input["change_count"] != float64(1) || input["changes"] != nil {
		t.Fatalf("historical apply_patch arguments = %#v", input)
	}
}

func TestSessionItemsReadsStoredSessionWithoutRegistryEntry(t *testing.T) {
	source := &fakeDisplayItemSource{
		items: map[string][]zotigosession.DisplayItem{
			"sess-stored": makeDisplayItems(2),
		},
	}
	handler := newHandler(newSessionRegistry(), source)

	resp := getItems(t, handler, "/sessions/sess-stored/items")

	assertItemSequences(t, resp.Items, []uint64{1, 2})
}

func TestStoredDisplayItemSourceReadsDisplayLog(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	sess := &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:        "sess-store",
			CreatedAt: time.Now().UTC(),
			UpdatedAt: time.Now().UTC(),
		},
		AgentSnapshot: agent.Snapshot{
			History: []protocol.Message{protocol.NewUserMessage("runtime summary")},
		},
	}
	if err := store.Put(context.Background(), sess); err != nil {
		t.Fatalf("put session: %v", err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), sess.ID, zotigosession.DisplayItem{
		Type:    zotigosession.DisplayItemUserMessage,
		Role:    string(protocol.RoleUser),
		Content: []zotigosession.DisplayContentPart{{Type: string(protocol.ContentTypeText), Text: "display prompt"}},
	}); err != nil {
		t.Fatalf("append display item: %v", err)
	}

	source := sessionlog.StoredItemSource{Store: store}
	items, ok, err := source.LoadItems(context.Background(), "sess-store")
	if err != nil {
		t.Fatalf("load items: %v", err)
	}
	if !ok {
		t.Fatal("expected session to exist")
	}
	if len(items) != 1 || items[0].Content[0].Text != "display prompt" {
		t.Fatalf("unexpected items: %#v", items)
	}
}

func TestSessionStartTransitionsToStarting(t *testing.T) {
	handler := newTestHandler()
	created := createSession(t, handler)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/start", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	var started Session
	if err := decodeAPIData(t, rec.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	if started.State != SessionStateStarting {
		t.Fatalf("expected state %q, got %q", SessionStateStarting, started.State)
	}
	if started.StartedAt == nil || started.StartedAt.IsZero() {
		t.Fatal("expected started_at")
	}
}

func TestSessionStartLaunchesWorkerAndWaitsForReady(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	var server *httptest.Server
	workers := newWorkerRegistry()
	workerReady := make(chan *websocket.Conn, 2)
	launcher := workerLauncherFunc(func(_ context.Context, sessionID string, _ string) error {
		go func() {
			conn, err := connectReadyWorker(server, sessionID)
			if err != nil {
				workerReady <- nil
				return
			}
			workerReady <- conn
		}()
		return nil
	})
	handler := newHandler(newSessionRegistry(), source, handlerOptions{
		launcher:             launcher,
		workers:              workers,
		workerConnectTimeout: time.Second,
	})
	server = httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	started := startSession(t, handler, created.ID)
	if started.State != SessionStateRunning {
		t.Fatalf("expected state %q, got %q", SessionStateRunning, started.State)
	}

	worker := <-workerReady
	if worker == nil {
		t.Fatal("expected worker websocket connection")
	}
	defer worker.Close()
}

func TestSessionStartPassesWorkingDirectoryToWorkerLauncher(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	var server *httptest.Server
	workers := newWorkerRegistry()
	workerReady := make(chan *websocket.Conn, 2)
	workDir := t.TempDir()
	var launchedWorkDir string
	launcher := workerLauncherFunc(func(_ context.Context, sessionID string, workingDirectory string) error {
		launchedWorkDir = workingDirectory
		go func() {
			conn, err := connectReadyWorker(server, sessionID)
			if err != nil {
				workerReady <- nil
				return
			}
			workerReady <- conn
		}()
		return nil
	})
	handler := newHandler(newSessionRegistry(), source, handlerOptions{
		launcher:             launcher,
		workers:              workers,
		workerConnectTimeout: time.Second,
	})
	server = httptest.NewServer(handler)
	defer server.Close()

	created := createSessionWithWorkingDirectory(t, handler, workDir)
	started := startSession(t, handler, created.ID)
	if started.State != SessionStateRunning {
		t.Fatalf("expected state %q, got %q", SessionStateRunning, started.State)
	}
	if launchedWorkDir != workDir {
		t.Fatalf("expected launcher workdir %q, got %q", workDir, launchedWorkDir)
	}

	worker := <-workerReady
	if worker == nil {
		t.Fatal("expected worker websocket connection")
	}
	defer worker.Close()
}

func TestSessionStartResumesStoredSession(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	workDir := t.TempDir()
	putStoredSession(t, store, "sess-stored", workDir)
	var server *httptest.Server
	workers := newWorkerRegistry()
	workerReady := make(chan *websocket.Conn, 1)
	var launchedWorkDir string
	launcher := workerLauncherFunc(func(_ context.Context, sessionID string, workingDirectory string) error {
		launchedWorkDir = workingDirectory
		go func() {
			conn, err := connectReadyWorker(server, sessionID)
			if err != nil {
				workerReady <- nil
				return
			}
			workerReady <- conn
		}()
		return nil
	})
	handler := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{
		store:                store,
		launcher:             launcher,
		workers:              workers,
		workerConnectTimeout: time.Second,
	})
	server = httptest.NewServer(handler)
	defer server.Close()

	started := startSession(t, handler, "sess-stored")
	if started.State != SessionStateRunning || !started.Live {
		t.Fatalf("expected running live session, got %#v", started)
	}
	if launchedWorkDir != workDir {
		t.Fatalf("expected launcher workdir %q, got %q", workDir, launchedWorkDir)
	}
	worker := <-workerReady
	if worker == nil {
		t.Fatal("expected worker websocket connection")
	}
	defer worker.Close()
}

func TestSessionStartConcurrentResumeLaunchesOneWorker(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	putStoredSession(t, store, "sess-stored", t.TempDir())
	var server *httptest.Server
	workers := newWorkerRegistry()
	workerReady := make(chan *websocket.Conn, 2)
	launchStarted := make(chan struct{})
	releaseLaunch := make(chan struct{})
	var launchOnce sync.Once
	var launches atomic.Int32
	launcher := workerLauncherFunc(func(_ context.Context, sessionID string, _ string) error {
		launches.Add(1)
		launchOnce.Do(func() { close(launchStarted) })
		<-releaseLaunch
		go func() {
			conn, err := connectReadyWorker(server, sessionID)
			if err != nil {
				workerReady <- nil
				return
			}
			workerReady <- conn
		}()
		return nil
	})
	handler := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{
		store:                store,
		launcher:             launcher,
		workers:              workers,
		workerConnectTimeout: time.Second,
	})
	server = httptest.NewServer(handler)
	defer server.Close()

	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for idx := 0; idx < 2; idx++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/sess-stored/start", nil))
			codes <- rec.Code
		}()
	}
	<-launchStarted
	close(releaseLaunch)
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Fatalf("expected concurrent start status %d, got %d", http.StatusOK, code)
		}
	}
	if got := launches.Load(); got != 1 {
		t.Fatalf("expected one worker launch, got %d", got)
	}
	worker := <-workerReady
	if worker == nil {
		t.Fatal("expected worker websocket connection")
	}
	defer worker.Close()
}

func TestSessionStartObserverTimeoutDoesNotFailActiveLaunch(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	putStoredSession(t, store, "sess-stored", t.TempDir())
	var server *httptest.Server
	workers := newWorkerRegistry()
	workerReady := make(chan *websocket.Conn, 1)
	launchStarted := make(chan struct{})
	releaseLaunch := make(chan struct{})
	var launchOnce sync.Once
	launcher := workerLauncherFunc(func(_ context.Context, sessionID string, _ string) error {
		launchOnce.Do(func() { close(launchStarted) })
		<-releaseLaunch
		go func() {
			conn, err := connectReadyWorker(server, sessionID)
			if err != nil {
				workerReady <- nil
				return
			}
			workerReady <- conn
		}()
		return nil
	})
	handler := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{
		store:                store,
		launcher:             launcher,
		workers:              workers,
		workerConnectTimeout: time.Second,
	})
	server = httptest.NewServer(handler)
	defer server.Close()

	firstDone := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/sess-stored/start", nil))
		firstDone <- rec.Code
	}()
	<-launchStarted

	waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/sess-stored/start", nil).WithContext(waitCtx)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected observer status %d, got %d: %s", http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	}

	close(releaseLaunch)
	if code := <-firstDone; code != http.StatusOK {
		t.Fatalf("expected owner start status %d, got %d", http.StatusOK, code)
	}
	worker := <-workerReady
	if worker == nil {
		t.Fatal("expected worker websocket connection")
	}
	defer worker.Close()
	if got := getSession(t, handler, "sess-stored"); got.State != SessionStateRunning {
		t.Fatalf("expected active launch to remain running, got %#v", got)
	}
}

func TestSessionStartOwnerCancellationDoesNotFailSharedLaunch(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer store.Close()
	putStoredSession(t, store, "sess-owner-cancel", t.TempDir())
	var server *httptest.Server
	workers := newWorkerRegistry()
	workerReady := make(chan *websocket.Conn, 1)
	launchStarted := make(chan struct{})
	releaseLaunch := make(chan struct{})
	launcher := workerLauncherFunc(func(_ context.Context, sessionID string, _ string) error {
		close(launchStarted)
		<-releaseLaunch
		go func() {
			conn, err := connectReadyWorker(server, sessionID)
			if err != nil {
				workerReady <- nil
				return
			}
			workerReady <- conn
		}()
		return nil
	})
	registry := newSessionRegistry()
	handler := newHandler(registry, sessionlog.StoredItemSource{Store: store}, handlerOptions{
		store:                store,
		launcher:             launcher,
		workers:              workers,
		workerConnectTimeout: time.Second,
	})
	server = httptest.NewServer(handler)
	defer server.Close()

	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	ownerDone := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/sessions/sess-owner-cancel/start", nil).WithContext(ownerCtx)
		handler.ServeHTTP(rec, req)
		ownerDone <- rec.Code
	}()
	<-launchStarted
	cancelOwner()
	<-ownerDone
	if session, ok := registry.Get("sess-owner-cancel"); !ok || session.State != SessionStateStarting {
		t.Fatalf("expected canceled owner to leave shared launch starting, got %#v", session)
	}

	observerDone := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/sess-owner-cancel/start", nil))
		observerDone <- rec.Code
	}()
	close(releaseLaunch)
	if code := <-observerDone; code != http.StatusOK {
		t.Fatalf("expected observer to complete shared launch with status %d, got %d", http.StatusOK, code)
	}
	worker := <-workerReady
	if worker == nil {
		t.Fatal("expected worker websocket connection")
	}
	defer worker.Close()
}

func TestSessionStartDaemonTimeoutSurvivesOwnerCancellation(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer store.Close()
	putStoredSession(t, store, "sess-owner-timeout", t.TempDir())
	launchStarted := make(chan struct{})
	releaseLaunch := make(chan struct{})
	launcher := workerLauncherFunc(func(context.Context, string, string) error {
		close(launchStarted)
		<-releaseLaunch
		return nil
	})
	registry := newSessionRegistry()
	handler := newHandler(registry, sessionlog.StoredItemSource{Store: store}, handlerOptions{
		store:               store,
		launcher:            launcher,
		workerLaunchTimeout: 20 * time.Millisecond,
	})

	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	ownerDone := make(chan struct{})
	go func() {
		defer close(ownerDone)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/sessions/sess-owner-timeout/start", nil).WithContext(ownerCtx)
		handler.ServeHTTP(rec, req)
	}()
	<-launchStarted
	cancelOwner()
	<-ownerDone

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		session, ok := registry.Get("sess-owner-timeout")
		if ok && session.State == SessionStateFailed {
			if !strings.Contains(session.Error, errWorkerLaunchTimeout.Error()) {
				t.Fatalf("unexpected daemon timeout error: %q", session.Error)
			}
			close(releaseLaunch)
			return
		}
		time.Sleep(time.Millisecond)
	}
	close(releaseLaunch)
	t.Fatalf("expected daemon-owned timeout to fail canceled launch, got %#v", func() Session {
		session, _ := registry.Get("sess-owner-timeout")
		return session
	}())
}

func TestSessionStartDaemonTimeoutCancelsLauncher(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer store.Close()
	putStoredSession(t, store, "sess-launch-cancel", t.TempDir())
	launcherCanceled := make(chan struct{})
	launcher := workerLauncherFunc(func(ctx context.Context, _, _ string) error {
		<-ctx.Done()
		close(launcherCanceled)
		return ctx.Err()
	})
	handler := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{
		store: store, launcher: launcher, workerLaunchTimeout: 20 * time.Millisecond,
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/sess-launch-cancel/start", nil))
	assertAPIError(t, rec, http.StatusServiceUnavailable, "service_unavailable", errWorkerLaunchTimeout.Error())
	select {
	case <-launcherCanceled:
	case <-time.After(time.Second):
		t.Fatal("startup timeout did not cancel launcher")
	}
}

func TestSessionStartFailsWhenWorkerDoesNotConnect(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source, handlerOptions{
		launcher:             workerLauncherFunc(func(context.Context, string, string) error { return nil }),
		workerConnectTimeout: 10 * time.Millisecond,
	})

	created := createSession(t, handler)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/start", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status %d, got %d: %s", http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	}
	if got := getSession(t, handler, created.ID); got.State != SessionStateFailed {
		t.Fatalf("expected state %q, got %q", SessionStateFailed, got.State)
	}
}

func TestSessionStartConnectTimeoutBeginsAfterLauncherReturns(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	var server *httptest.Server
	workerReady := make(chan *websocket.Conn, 1)
	const connectTimeout = 250 * time.Millisecond
	launcher := workerLauncherFunc(func(_ context.Context, sessionID string, _ string) error {
		time.Sleep(2 * connectTimeout)
		go func() {
			conn, err := connectReadyWorker(server, sessionID)
			if err != nil {
				workerReady <- nil
				return
			}
			workerReady <- conn
		}()
		return nil
	})
	handler := newHandler(newSessionRegistry(), source, handlerOptions{
		launcher:             launcher,
		workerConnectTimeout: connectTimeout,
	})
	server = httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	started := startSession(t, handler, created.ID)
	if started.State != SessionStateRunning {
		t.Fatalf("session state = %q, want %q", started.State, SessionStateRunning)
	}
	worker := <-workerReady
	if worker == nil {
		t.Fatal("expected worker websocket connection")
	}
	defer worker.Close()
}

func TestWaitForRunningWorkerReturnsWhenReadyConnectionIsGone(t *testing.T) {
	registry := newSessionRegistry()
	registry.Add(Session{ID: "sess-ready-disconnected", State: SessionStateRunning})
	handler := &handler{registry: registry, workers: newWorkerRegistry()}

	err := handler.waitForRunningWorker(context.Background(), "sess-ready-disconnected")
	if !errors.Is(err, errWorkerDisconnectedBeforeReady) {
		t.Fatalf("wait error = %v, want %v", err, errWorkerDisconnectedBeforeReady)
	}
}

func TestOccupiedRuntimeFailureIsConflictAndRetryable(t *testing.T) {
	registry := newSessionRegistry()
	registry.Add(Session{
		ID: "sess-occupied", State: SessionStateFailed, Agent: "codex",
		Error: "thread already has an active writer", ErrorCode: "runtime_occupied", CreatedAt: time.Now().UTC(),
	})
	handler := &handler{registry: registry, workers: newWorkerRegistry(), items: &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}}

	recorder := httptest.NewRecorder()
	handler.writeEnsureRunningError(recorder, fmt.Errorf("%w: thread already has an active writer", workerbase.ErrRuntimeOccupied))
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), `"code":"runtime_occupied"`) {
		t.Fatalf("occupied response = %d: %s", recorder.Code, recorder.Body.String())
	}

	session, launched, err := handler.ensureSessionStartedLocked(context.Background(), "sess-occupied")
	if err != nil {
		t.Fatal(err)
	}
	if !launched || session.State != SessionStateStarting || session.Error != "" || session.ErrorCode != "" {
		t.Fatalf("retried occupied session = %#v, launched=%v", session, launched)
	}
}

func TestSessionStartRejectsInvalidTransition(t *testing.T) {
	handler := newTestHandler()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/start", nil))

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d", http.StatusConflict, rec.Code)
	}
}

func TestPublicSessionRouteRejectsWorkerActions(t *testing.T) {
	handler := newTestHandler()
	created := createSession(t, handler)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/worker/attach", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d", http.StatusNotFound, rec.Code)
	}
}

func TestSessionRejectsUnknownAction(t *testing.T) {
	handler := newTestHandler()
	created := createSession(t, handler)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/bogus", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d", http.StatusNotFound, rec.Code)
	}
}

func TestSessionRouteRejectsEscapedSlashInID(t *testing.T) {
	registry := newSessionRegistry()
	registry.Add(Session{ID: "sess/escape"})
	handler := newHandler(registry, &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/sess%2Fescape", nil))

	assertAPIError(t, rec, http.StatusNotFound, "not_found", "not found")
}

func TestSessionsGetByIDNotFound(t *testing.T) {
	rec := httptest.NewRecorder()

	newTestHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/missing", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d", http.StatusNotFound, rec.Code)
	}
}

func TestSessionsListUsesCreationOrder(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	handler := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{store: store})
	createdIDs := make([]string, 0, 11)
	workDir := t.TempDir()
	writeTestProfileConfig(t, workDir)
	body := fmt.Sprintf(`{"working_directory":%q}`, workDir)

	for i := 0; i < 11; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(body)))
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %d: expected status %d, got %d", i, http.StatusCreated, rec.Code)
		}
		var created Session
		if err := decodeAPIData(t, rec.Body.Bytes(), &created); err != nil {
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
	if err := decodeAPIData(t, rec.Body.Bytes(), &list); err != nil {
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

func TestSessionsListSerializesEmptyArray(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	handler := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{store: store})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	var payload map[string]json.RawMessage
	if err := decodeAPIData(t, rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if got := string(payload["sessions"]); got != "[]" {
		t.Fatalf("expected empty sessions array, got %s", got)
	}
}

func TestRoutesRejectUnsupportedMethod(t *testing.T) {
	tests := []struct {
		name      string
		method    string
		path      string
		wantAllow string
	}{
		{name: "SessionsRejectsUnsupportedMethod", method: http.MethodDelete, path: "/sessions", wantAllow: "GET, POST"},
		{name: "SessionRejectsUnsupportedMethod", method: http.MethodPost, path: "/sessions/sess-1", wantAllow: http.MethodGet},
		{name: "SessionStartRejectsUnsupportedMethod", method: http.MethodGet, path: "/sessions/sess-1/start", wantAllow: http.MethodPost},
		{name: "HealthRejectsNonGET", method: http.MethodPost, path: "/health", wantAllow: http.MethodGet},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()

			newTestHandler().ServeHTTP(rec, req)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("expected status %d, got %d", http.StatusMethodNotAllowed, rec.Code)
			}
			if got := rec.Header().Get("Allow"); got != tc.wantAllow {
				t.Fatalf("expected Allow header %q, got %q", tc.wantAllow, got)
			}
		})
	}
}

func TestSessionContextUsageLifecycleSurvivesReload(t *testing.T) {
	root := t.TempDir()
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	meta := zotigosession.Metadata{ID: "session-context-lifecycle", Agent: "codex", CreatedAt: now, UpdatedAt: now}
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: meta}); err != nil {
		t.Fatal(err)
	}
	source := sessionlog.StoredItemSource{Store: store}
	h := &handler{store: store, items: source}
	if got := h.decorateSession(context.Background(), sessionFromMetadata(meta, SessionStateOffline, false), nil).ContextUsage; got == nil || got.Status != "unavailable" {
		t.Fatalf("new session context usage = %#v", got)
	}
	if _, err := source.AppendItem(context.Background(), meta.ID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemContextUsageUpdated,
		ContextUsage: &zotigosession.DisplayContextUsage{
			Tokens: 12345, Window: 512000, Status: "available", Source: "provider", UpdatedAt: now,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := source.AppendItem(context.Background(), meta.ID, zotigosession.DisplayItem{Type: zotigosession.DisplayItemAssistantMessage}); err != nil {
		t.Fatal(err)
	}
	got := h.decorateSession(context.Background(), sessionFromMetadata(meta, SessionStateRunning, true), nil).ContextUsage
	if got == nil || got.Status != "available" || got.Source != "provider" || got.Tokens != 12345 || got.Window != 512000 {
		t.Fatalf("running session context usage = %#v", got)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reloadedHandler := &handler{store: reopened, items: sessionlog.StoredItemSource{Store: reopened}}
	reloaded := reloadedHandler.decorateSession(context.Background(), sessionFromMetadata(meta, SessionStateOffline, false), nil).ContextUsage
	if reloaded == nil || *reloaded != *got {
		t.Fatalf("reloaded context usage = %#v, want %#v", reloaded, got)
	}
}

func TestLiveSessionGetUsesStoredChannelToolCapability(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	stored := &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: "session-channel-capability", Agent: "codex", Model: "gpt-5.6-sol", ReasoningEffort: "medium",
		Capabilities: zotigosession.Capabilities{ChannelToolsVersion: channels.RuntimeToolsVersion},
		CreatedAt:    now, UpdatedAt: now,
	}}
	if err := store.Put(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	registry := newSessionRegistry()
	registry.Add(Session{ID: stored.ID, State: SessionStateRunning, Live: true, Agent: "codex", CreatedAt: now})
	handler := &handler{registry: registry, store: store, items: &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{stored.ID: {}}}}
	recorder := httptest.NewRecorder()
	handler.handleSessionGet(recorder, httptest.NewRequest(http.MethodGet, "/sessions/"+stored.ID, nil), stored.ID)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var session Session
	if err := decodeAPIData(t, recorder.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	if session.ChannelToolsVersion != channels.RuntimeToolsVersion || !session.ChannelToolsEligible {
		t.Fatalf("session capability = version %d eligible %v", session.ChannelToolsVersion, session.ChannelToolsEligible)
	}
}

func TestNativeSessionContextUsageSurvivesCompactionAndReload(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()
	writeTestProfileConfig(t, workDir)
	compressor := compaction.NewCompressor(compaction.CompressorConfig{
		ContextWindowSize: 100_000, TriggerRatio: 0.7, PreserveRatio: 0.3,
		TokenCounter: func(text string) int { return (len(text) + 3) / 4 },
	})
	history := make([]protocol.Message, 0, 21)
	for range 10 {
		history = append(history,
			protocol.NewUserMessage(strings.Repeat("old context ", 4_000)),
			protocol.NewAssistantMessage("old answer"),
		)
	}
	history[len(history)-1].Metadata = &protocol.MessageMetadata{Usage: &protocol.Usage{InputTokens: 80_000, TotalTokens: 80_100}}
	compressed, result, err := compressor.Compress(context.Background(), history)
	if err != nil || !result.Compressed {
		t.Fatalf("compress history: result=%#v err=%v", result, err)
	}

	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	stored := &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID: "native-compacted", Agent: "zotigo", ProfileName: "test", WorkingDirectory: workDir,
			CreatedAt: now, UpdatedAt: now,
		},
		AgentSnapshot: agent.Snapshot{History: compressed},
	}
	if err := store.Put(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	assertUsage := func(handler *handler, saved *zotigosession.Session) {
		t.Helper()
		got := handler.decorateSession(context.Background(), sessionFromMetadata(saved.Metadata, SessionStateOffline, false), saved).ContextUsage
		want := compaction.NewCompressor(compaction.CompressorConfig{}).CountTokens(compressed)
		if got == nil || got.Status != "available" || got.Source != "estimate" || got.Tokens != want || got.Tokens >= 80_000 {
			t.Fatalf("context usage after compaction = %#v", got)
		}
	}
	assertUsage(&handler{store: store, items: sessionlog.StoredItemSource{Store: store}}, stored)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reloaded, err := reopened.Get(context.Background(), stored.ID)
	if err != nil || reloaded == nil {
		t.Fatalf("reload compacted session: %#v %v", reloaded, err)
	}
	assertUsage(&handler{store: reopened, items: sessionlog.StoredItemSource{Store: reopened}}, reloaded)
}
