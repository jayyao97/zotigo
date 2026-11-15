package zotigod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bytedance/sonic"
	"github.com/gorilla/websocket"
	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/core/tools"
	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

type sessionListResponse struct {
	Sessions []Session `json:"sessions"`
}

func decodeAPIData(t *testing.T, data []byte, value any) error {
	t.Helper()
	var resp struct {
		Code    string          `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := sonic.Unmarshal(data, &resp); err != nil {
		t.Fatalf("decode api response: %v", err)
	}
	if resp.Code != "ok" {
		t.Fatalf("expected api code ok, got %q: %s", resp.Code, resp.Message)
	}
	if value == nil {
		return nil
	}
	if len(resp.Data) == 0 {
		t.Fatal("expected api response data")
	}
	if err := sonic.Unmarshal(resp.Data, value); err != nil {
		t.Fatalf("decode api response data: %v", err)
	}
	return nil
}

func assertAPIError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string, messageContains string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("expected status %d, got %d: %s", status, rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("expected JSON content type, got %q", got)
	}
	var resp struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Data    any    `json:"data,omitempty"`
	}
	if err := sonic.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode api error response: %v", err)
	}
	if resp.Code != code {
		t.Fatalf("expected error code %q, got %q: %s", code, resp.Code, rec.Body.String())
	}
	if !strings.Contains(resp.Message, messageContains) {
		t.Fatalf("expected error message containing %q, got %q", messageContains, resp.Message)
	}
	if resp.Data != nil {
		t.Fatalf("expected no data in error response, got %#v", resp.Data)
	}
}

type noopProvider struct{}

func (p *noopProvider) Name() string { return "noop" }

func (p *noopProvider) StreamChat(context.Context, []protocol.Message, []tools.Tool, ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	ch := make(chan protocol.Event)
	close(ch)
	return ch, nil
}

type blockingFinishProvider struct {
	started chan struct{}
	release chan struct{}
}

func (p *blockingFinishProvider) Name() string { return "blocking-finish" }

func (p *blockingFinishProvider) StreamChat(context.Context, []protocol.Message, []tools.Tool, ...providers.StreamChatOption) (<-chan protocol.Event, error) {
	events := make(chan protocol.Event, 2)
	go func() {
		defer close(events)
		close(p.started)
		<-p.release
		events <- protocol.NewTextDeltaEvent("old generation")
		events <- protocol.NewFinishEvent(protocol.FinishReasonStop)
	}()
	return events, nil
}

type fakeDisplayItemSource struct {
	mu             sync.Mutex
	items          map[string][]zotigosession.DisplayItem
	err            error
	appendErr      func(sessionID string, item zotigosession.DisplayItem) error
	appendHook     func(sessionID string, item zotigosession.DisplayItem)
	offsetCalls    atomic.Int32
	loadCalls      atomic.Int32
	offsetMaxLines []int
}

type blockingProfileUpdateStore struct {
	zotigosession.Store
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingProfileUpdateStore) UpdateProfile(ctx context.Context, id string, profileName string, updatedAt time.Time) error {
	s.once.Do(func() { close(s.started) })
	<-s.release
	return s.Store.(sessionProfileUpdater).UpdateProfile(ctx, id, profileName, updatedAt)
}

type rollbackFailingProfileStore struct {
	zotigosession.Store
	updates int
}

type unlockFailingStore struct {
	zotigosession.Store
}

func (s unlockFailingStore) Unlock(context.Context, string) error {
	return errors.New("unlock unavailable")
}

func (s *rollbackFailingProfileStore) UpdateProfile(ctx context.Context, id string, profileName string, updatedAt time.Time) error {
	s.updates++
	if s.updates == 2 {
		return errors.New("rollback unavailable")
	}
	return s.Store.(sessionProfileUpdater).UpdateProfile(ctx, id, profileName, updatedAt)
}

func (s *fakeDisplayItemSource) LoadItems(_ context.Context, sessionID string) ([]zotigosession.DisplayItem, bool, error) {
	s.loadCalls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, false, s.err
	}
	items, ok := s.items[sessionID]
	items = append([]zotigosession.DisplayItem(nil), items...)
	return items, ok, nil
}

func (s *fakeDisplayItemSource) LoadItemsFromOffset(_ context.Context, sessionID string, offset int64, maxLines int) ([]zotigosession.DisplayItem, bool, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.offsetCalls.Add(1)
	s.offsetMaxLines = append(s.offsetMaxLines, maxLines)
	if s.err != nil {
		return nil, false, offset, s.err
	}
	items, ok := s.items[sessionID]
	if !ok {
		return nil, false, offset, nil
	}
	if offset < 0 {
		return nil, true, offset, fmt.Errorf("offset must be non-negative")
	}
	start := int(offset)
	if start > len(items) {
		return nil, true, offset, fmt.Errorf("offset exceeds display log size")
	}
	end := len(items)
	if maxLines > 0 && start+maxLines < end {
		end = start + maxLines
	}
	result := append([]zotigosession.DisplayItem(nil), items[start:end]...)
	for idx := range result {
		result[idx].LogOffset = int64(start + idx + 1)
	}
	return result, true, int64(end), nil
}

func (s *fakeDisplayItemSource) AppendItem(_ context.Context, sessionID string, item zotigosession.DisplayItem) (zotigosession.DisplayItem, error) {
	if s.err != nil {
		return zotigosession.DisplayItem{}, s.err
	}
	if s.items == nil {
		s.items = make(map[string][]zotigosession.DisplayItem)
	}
	if s.appendErr != nil {
		if err := s.appendErr(sessionID, item); err != nil {
			return zotigosession.DisplayItem{}, err
		}
	}
	s.mu.Lock()
	next := uint64(len(s.items[sessionID]) + 1)
	item.Sequence = next
	if item.ID == "" {
		item.ID = fmt.Sprintf("item_%s_%d", sessionID, next)
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = time.Now().UTC()
	}
	s.items[sessionID] = append(s.items[sessionID], item)
	s.mu.Unlock()
	if s.appendHook != nil {
		s.appendHook(sessionID, item)
	}
	return item, nil
}

func (s *fakeDisplayItemSource) AppendItemIf(ctx context.Context, sessionID string, item zotigosession.DisplayItem, condition func([]zotigosession.DisplayItem) error) (zotigosession.DisplayItem, error) {
	if s.err != nil {
		return zotigosession.DisplayItem{}, s.err
	}
	if s.items == nil {
		s.items = make(map[string][]zotigosession.DisplayItem)
	}
	if s.appendErr != nil {
		if err := s.appendErr(sessionID, item); err != nil {
			return zotigosession.DisplayItem{}, err
		}
	}
	s.mu.Lock()
	items := append([]zotigosession.DisplayItem(nil), s.items[sessionID]...)
	if condition != nil {
		if err := condition(items); err != nil {
			s.mu.Unlock()
			return zotigosession.DisplayItem{}, err
		}
	}
	next := uint64(len(s.items[sessionID]) + 1)
	item.Sequence = next
	if item.ID == "" {
		item.ID = fmt.Sprintf("item_%s_%d", sessionID, next)
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = time.Now().UTC()
	}
	s.items[sessionID] = append(s.items[sessionID], item)
	s.mu.Unlock()
	if s.appendHook != nil {
		s.appendHook(sessionID, item)
	}
	return item, nil
}

func (s *fakeDisplayItemSource) AppendItemAtomically(_ context.Context, sessionID string, build func([]zotigosession.DisplayItem) (zotigosession.DisplayItem, bool, error)) (zotigosession.DisplayItem, bool, error) {
	if s.err != nil {
		return zotigosession.DisplayItem{}, false, s.err
	}
	s.mu.Lock()
	items := append([]zotigosession.DisplayItem(nil), s.items[sessionID]...)
	item, appendItem, err := build(items)
	if err != nil || !appendItem {
		s.mu.Unlock()
		return item, false, err
	}
	if s.appendErr != nil {
		if err := s.appendErr(sessionID, item); err != nil {
			s.mu.Unlock()
			return zotigosession.DisplayItem{}, false, err
		}
	}
	next := uint64(len(s.items[sessionID]) + 1)
	item.Sequence = next
	if item.ID == "" {
		item.ID = fmt.Sprintf("item_%s_%d", sessionID, next)
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = time.Now().UTC()
	}
	s.items[sessionID] = append(s.items[sessionID], item)
	s.mu.Unlock()
	if s.appendHook != nil {
		s.appendHook(sessionID, item)
	}
	return item, true, nil
}

func (s *fakeDisplayItemSource) AppendItems(_ context.Context, sessionID string, items []zotigosession.DisplayItem) ([]zotigosession.DisplayItem, error) {
	return s.appendItemsAfter(sessionID, nil, items)
}

func (s *fakeDisplayItemSource) AppendItemsAfter(_ context.Context, sessionID string, expectedSequence uint64, items []zotigosession.DisplayItem) ([]zotigosession.DisplayItem, error) {
	return s.appendItemsAfter(sessionID, &expectedSequence, items)
}

func (s *fakeDisplayItemSource) appendItemsAfter(sessionID string, expectedSequence *uint64, items []zotigosession.DisplayItem) ([]zotigosession.DisplayItem, error) {
	if s.err != nil {
		return nil, s.err
	}
	for _, item := range items {
		if s.appendErr != nil {
			if err := s.appendErr(sessionID, item); err != nil {
				return nil, err
			}
		}
	}
	s.mu.Lock()
	latest := uint64(0)
	if existing := s.items[sessionID]; len(existing) > 0 {
		latest = existing[len(existing)-1].Sequence
	}
	if expectedSequence != nil && latest != *expectedSequence {
		s.mu.Unlock()
		return nil, zotigosession.ErrDisplayLogChanged
	}
	if s.items == nil {
		s.items = make(map[string][]zotigosession.DisplayItem)
	}
	stored := append([]zotigosession.DisplayItem(nil), items...)
	for index := range stored {
		stored[index].Sequence = uint64(len(s.items[sessionID]) + 1)
		if stored[index].ID == "" {
			stored[index].ID = fmt.Sprintf("item_%s_%d", sessionID, stored[index].Sequence)
		}
		if stored[index].CreatedAt.IsZero() {
			stored[index].CreatedAt = time.Now().UTC()
		}
		s.items[sessionID] = append(s.items[sessionID], stored[index])
	}
	s.mu.Unlock()
	if s.appendHook != nil {
		for _, item := range stored {
			s.appendHook(sessionID, item)
		}
	}
	return stored, nil
}

func createSession(t *testing.T, handler http.Handler) Session {
	t.Helper()

	workDir := t.TempDir()
	writeTestProfileConfig(t, workDir)
	rec := httptest.NewRecorder()
	body := fmt.Sprintf(`{"working_directory":%q}`, workDir)
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d", http.StatusCreated, rec.Code)
	}
	var session Session
	if err := decodeAPIData(t, rec.Body.Bytes(), &session); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	return session
}

func createSessionWithWorkingDirectory(t *testing.T, handler http.Handler, workDir string) Session {
	t.Helper()

	writeTestProfileConfig(t, workDir)
	rec := httptest.NewRecorder()
	body := fmt.Sprintf(`{"working_directory":%q}`, workDir)
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	var session Session
	if err := decodeAPIData(t, rec.Body.Bytes(), &session); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	return session
}

func writeTestProfileConfig(t *testing.T, workDir string) {
	t.Helper()

	configPath := filepath.Join(workDir, config.ProjectConfig)
	if _, err := os.Stat(configPath); err == nil {
		return
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat project config: %v", err)
	}
	const contents = "default_profile: test\nprofiles:\n  test:\n    provider: openai\n    model: test\n"
	if err := os.WriteFile(configPath, []byte(contents), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
}

func putStoredSession(t *testing.T, store zotigosession.Store, id string, workDir string) {
	t.Helper()
	writeTestProfileConfig(t, workDir)
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:               id,
			WorkingDirectory: workDir,
			ProfileName:      "test",
			CreatedAt:        now,
			UpdatedAt:        now,
		},
		AgentSnapshot: agent.Snapshot{
			State:     agent.StateIdle,
			CreatedAt: now,
		},
		Turns: make([]zotigosession.Turn, 0),
	}); err != nil {
		t.Fatalf("put stored session: %v", err)
	}
}

func TestActiveToolNameTracksDurableExecution(t *testing.T) {
	items := []zotigosession.DisplayItem{
		{Type: zotigosession.DisplayItemTurnStarted},
		{Type: zotigosession.DisplayItemToolExecutionStarted, ToolExecution: &zotigosession.DisplayToolExecution{ToolCallID: "call-1", ToolName: "shell"}},
	}
	if got := activeToolName(items); got != "shell" {
		t.Fatalf("active tool = %q, want shell", got)
	}
	items = append(items, zotigosession.DisplayItem{Content: []zotigosession.DisplayContentPart{{ToolResult: &zotigosession.DisplayToolResult{ToolCallID: "call-1"}}}})
	if got := activeToolName(items); got != "" {
		t.Fatalf("active tool after result = %q", got)
	}
}

func TestAPIErrorEnvelopeContract(t *testing.T) {
	handler := newTestHandler()

	tests := []struct {
		name            string
		method          string
		path            string
		body            string
		status          int
		code            string
		messageContains string
	}{
		{
			name:            "public bad request",
			method:          http.MethodPost,
			path:            "/sessions",
			body:            `{"working_directory":"relative"}`,
			status:          http.StatusBadRequest,
			code:            "invalid_request",
			messageContains: "working_directory must be an absolute path",
		},
		{
			name:            "public not found",
			method:          http.MethodGet,
			path:            "/sessions/missing",
			status:          http.StatusNotFound,
			code:            "not_found",
			messageContains: "session not found",
		},
		{
			name:            "method not allowed",
			method:          http.MethodPost,
			path:            "/health",
			status:          http.StatusMethodNotAllowed,
			code:            "method_not_allowed",
			messageContains: "method not allowed",
		},
		{
			name:            "internal non commands bad request",
			method:          http.MethodPost,
			path:            "/internal/sessions/missing/worker/finish",
			body:            `{`,
			status:          http.StatusBadRequest,
			code:            "invalid_request",
			messageContains: "decode request",
		},
		{
			name:            "internal commands bad request",
			method:          http.MethodGet,
			path:            "/internal/sessions/{session}/commands?after=abc",
			status:          http.StatusBadRequest,
			code:            "invalid_request",
			messageContains: "invalid after cursor",
		},
	}

	session := createSession(t, handler)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			path := strings.ReplaceAll(tt.path, "{session}", session.ID)
			req := httptest.NewRequest(tt.method, path, strings.NewReader(tt.body))
			handler.ServeHTTP(rec, req)
			assertAPIError(t, rec, tt.status, tt.code, tt.messageContains)
		})
	}
}

func TestCodexWorkerConnectRejectsStaleActivation(t *testing.T) {
	registry := newSessionRegistry()
	workers := newWorkerRegistry()
	registry.Add(Session{
		ID: "sess-stale-activation", State: SessionStateStarting, Agent: string(zotigoruntime.AgentCodex), workerActivation: 2,
	})
	handler := newHandler(registry, &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}, handlerOptions{workers: workers})
	server := httptest.NewServer(handler)
	defer server.Close()

	staleURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/internal/workers/connect?session_id=sess-stale-activation&activation=1"
	stale, response, err := websocket.DefaultDialer.Dial(staleURL, nil)
	if stale != nil {
		_ = stale.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusConflict {
		t.Fatalf("stale worker connect error = %v, response = %#v", err, response)
	}
	_ = response.Body.Close()
	if workers.Has("sess-stale-activation") {
		t.Fatal("stale worker was registered")
	}

	current, generation := connectWorkerForActivation(t, server, "sess-stale-activation", "2")
	defer current.Close()
	markWorkerReady(t, server, "sess-stale-activation", generation)
	if session, _ := registry.Get("sess-stale-activation"); session.State != SessionStateRunning {
		t.Fatalf("current activation state = %q, want %q", session.State, SessionStateRunning)
	}
}

func TestConcurrentWorkerConnectKeepsFirstConnectedWorker(t *testing.T) {
	registry := newSessionRegistry()
	workers := newWorkerRegistry()
	sessionOps := newSessionOperationLocks()
	handler := newHandler(registry, &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}, handlerOptions{workers: workers, sessionOps: sessionOps})
	created := Session{ID: "sess-concurrent-connect", State: SessionStateStarting, WorkingDirectory: t.TempDir(), CreatedAt: time.Now().UTC()}
	registry.Add(created)
	server := httptest.NewServer(handler)
	defer server.Close()

	unlock := sessionOps.lock(created.ID)
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/internal/workers/connect?session_id=" + created.ID
	type connectedWorker struct {
		conn       *websocket.Conn
		generation string
	}
	connections := make(chan connectedWorker, 2)
	for range 2 {
		go func() {
			conn, resp, _ := websocket.DefaultDialer.Dial(url, nil)
			generation := ""
			if resp != nil {
				generation = resp.Header.Get(wire.GenerationHeader)
			}
			connections <- connectedWorker{conn: conn, generation: generation}
		}()
	}
	first := <-connections
	second := <-connections
	defer func() {
		if first.conn != nil {
			first.conn.Close()
		}
		if second.conn != nil {
			second.conn.Close()
		}
	}()
	unlock()

	for deadline := time.Now().Add(time.Second); !workers.Has(created.ID) && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if !workers.Has(created.ID) {
		t.Fatal("expected one connected worker")
	}
	session, _ := registry.Get(created.ID)
	if session.State != SessionStateStarting {
		t.Fatalf("session state after connect = %q, want %q", session.State, SessionStateStarting)
	}
	generation := first.generation
	if !workers.Matches(created.ID, generation) {
		generation = second.generation
	}
	markWorkerReady(t, server, created.ID, generation)
	session, _ = registry.Get(created.ID)
	if session.State != SessionStateRunning {
		t.Fatalf("session state after ready = %q, want %q", session.State, SessionStateRunning)
	}
}

func TestCanceledMessageKeepsLifecycleSerializedUntilWorkerResult(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()

	ctx, cancel := context.WithCancel(context.Background())
	messageDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(`{"text":"accept after cancel","client_message_id":"cancel-once"}`)).WithContext(ctx)
		handler.ServeHTTP(rec, req)
		messageDone <- rec
	}()
	input := readWorkerMessage(t, worker)
	if input.Type != wire.MessageInputRequest || input.InputRequest == nil {
		t.Fatalf("expected input request, got %#v", input)
	}
	cancel()
	if response := <-messageDone; response.Code != http.StatusInternalServerError {
		t.Fatalf("canceled message status = %d: %s", response.Code, response.Body.String())
	}

	finishDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/internal/sessions/"+created.ID+"/worker/finish", nil))
		finishDone <- rec
	}()
	select {
	case finish := <-finishDone:
		t.Fatalf("finish bypassed canceled input lease: %d: %s", finish.Code, finish.Body.String())
	case <-time.After(50 * time.Millisecond):
	}

	accepted, err := appendAcceptedSessionInput(context.Background(), source, created.ID, input.InputRequest.Command)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.WriteJSON(wire.Message{Type: wire.MessageInputResult, InputResult: &wire.InputResult{
		RequestID: input.InputRequest.RequestID, Command: &accepted,
	}}); err != nil {
		t.Fatal(err)
	}
	if finish := <-finishDone; finish.Code != http.StatusConflict {
		t.Fatalf("finish status = %d: %s", finish.Code, finish.Body.String())
	}
	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 1 || items.Items[0].ID != "cancel-once" {
		t.Fatalf("accepted items = %#v, want one cancel-once command", items.Items)
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
	if err := decodeAPIData(t, rec.Body.Bytes(), &session); err != nil {
		t.Fatalf("decode start response: %v", err)
	}
	return session
}

func attachWorker(t *testing.T, handler http.Handler, id string) Session {
	t.Helper()
	server := httptest.NewServer(handler)
	defer server.Close()
	worker, generation := connectWorker(t, server, id)
	defer worker.Close()

	resp, err := postWorkerReady(server, id, generation)
	if err != nil {
		t.Fatalf("attach worker: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, resp.StatusCode)
	}
	var session Session
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read attach response: %v", err)
	}
	if err := decodeAPIData(t, data, &session); err != nil {
		t.Fatalf("decode attach response: %v", err)
	}
	return session
}

func getSession(t *testing.T, handler http.Handler, id string) Session {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/"+id, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var session Session
	if err := decodeAPIData(t, rec.Body.Bytes(), &session); err != nil {
		t.Fatalf("decode session response: %v", err)
	}
	return session
}

func createApprovalRequestForTest(t *testing.T, registry *sessionRegistry, source sessionlog.ItemSource, id string) wire.ApprovalRequest {
	t.Helper()
	approval := sessionlog.ApprovalRequest{
		ID:        sessionlog.NewID("apr"),
		SessionID: id,
		TurnID:    "turn-1",
		Status:    sessionlog.ApprovalStatusPending,
		Pending: []zotigosession.DisplayPendingApproval{{
			ToolCallID:       "call-1",
			ToolName:         "shell",
			Arguments:        `{"command":"touch file"}`,
			Description:      "Run shell command",
			Reason:           "writes files",
			RiskLevel:        "medium",
			Source:           "classifier",
			RequiresSnapshot: true,
		}},
	}
	item, err := source.AppendItem(context.Background(), id, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemApprovalRequest,
		Approval: &zotigosession.DisplayApproval{
			ID:      approval.ID,
			TurnID:  approval.TurnID,
			Pending: approval.Pending,
		},
	})
	if err != nil {
		t.Fatalf("append worker approval request: %v", err)
	}
	approval.CreatedAt = item.CreatedAt
	if _, err := source.AppendItem(context.Background(), id, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnPaused,
		Turn: &zotigosession.DisplayTurn{ID: approval.TurnID, Reason: "need_approval"},
	}); err != nil {
		t.Fatalf("append worker turn paused: %v", err)
	}
	if _, err := registry.Pause(id); err != nil {
		t.Fatalf("apply worker display wake: %v", err)
	}
	return publicApprovalRequest(approval)
}

func respondToApprovalDecision(conn *websocket.Conn, source sessionlog.ItemSource, sessionID string, approval wire.ApprovalRequest) <-chan error {
	done := make(chan error, 1)
	go func() {
		var msg wire.Message
		if err := conn.ReadJSON(&msg); err != nil {
			done <- err
			return
		}
		if msg.Type != wire.MessageApprovalDecision || msg.ApprovalDecision == nil {
			done <- fmt.Errorf("unexpected worker message: %#v", msg)
			return
		}
		decision := msg.ApprovalDecision
		item, err := source.AppendItem(context.Background(), sessionID, zotigosession.DisplayItem{
			Type: zotigosession.DisplayItemApprovalDecision,
			Approval: &zotigosession.DisplayApproval{
				ID:        approval.ID,
				TurnID:    approval.TurnID,
				Decisions: decision.Decisions,
			},
		})
		if err != nil {
			done <- err
			return
		}
		resolvedAt := item.CreatedAt
		resolved := approval
		resolved.Status = sessionlog.ApprovalStatusResolved
		resolved.Decisions = publicDisplayApprovalDecisions(decision.Decisions)
		resolved.ResolvedAt = &resolvedAt
		if err := conn.WriteJSON(wire.Message{
			Type: wire.MessageApprovalResult,
			ApprovalResult: &wire.ApprovalResult{
				RequestID: decision.RequestID,
				Approval:  &resolved,
			},
		}); err != nil {
			done <- err
			return
		}
		done <- nil
	}()
	return done
}

func dialWorker(t *testing.T, server *httptest.Server, sessionID string) *websocket.Conn {
	t.Helper()
	conn, generation := connectWorker(t, server, sessionID)
	markWorkerReady(t, server, sessionID, generation)
	return conn
}

func connectReadyWorker(server *httptest.Server, sessionID string) (*websocket.Conn, error) {
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/internal/workers/connect?session_id=" + sessionID
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		return nil, err
	}
	generation := resp.Header.Get(wire.GenerationHeader)

	resp, err = postWorkerReady(server, sessionID, generation)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("mark worker ready: %s", resp.Status)
	}
	return conn, nil
}

func connectWorker(t *testing.T, server *httptest.Server, sessionID string) (*websocket.Conn, string) {
	return connectWorkerForActivation(t, server, sessionID, "")
}

func connectWorkerForActivation(t *testing.T, server *httptest.Server, sessionID string, activation string) (*websocket.Conn, string) {
	t.Helper()

	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/internal/workers/connect?session_id=" + sessionID
	if activation != "" {
		url += "&activation=" + activation
	}
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial worker websocket: %v", err)
	}
	generation := resp.Header.Get(wire.GenerationHeader)
	if generation == "" {
		_ = conn.Close()
		t.Fatal("worker websocket response missing generation")
	}
	return conn, generation
}

func markWorkerReady(t *testing.T, server *httptest.Server, sessionID string, generation string) {
	t.Helper()
	resp, err := postWorkerReady(server, sessionID, generation)
	if err != nil {
		t.Fatalf("mark worker ready: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mark worker ready status = %d", resp.StatusCode)
	}
}

func postWorkerReady(server *httptest.Server, sessionID string, generation string) (*http.Response, error) {
	body, err := sonic.Marshal(wire.ReadyRequest{Generation: generation})
	if err != nil {
		return nil, err
	}
	return server.Client().Post(server.URL+"/internal/sessions/"+sessionID+"/worker/attach", "application/json", bytes.NewReader(body))
}

func workerReadyBody(generation string) io.Reader {
	body, _ := sonic.Marshal(wire.ReadyRequest{Generation: generation})
	return bytes.NewReader(body)
}

func readWorkerMessage(t *testing.T, conn *websocket.Conn) wire.Message {
	t.Helper()

	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	var msg wire.Message
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("read worker message: %v", err)
	}
	return msg
}

func serveTestWorkerInputs(t *testing.T, conn *websocket.Conn, source sessionlog.ItemSource, sessionID string) <-chan wire.InputRequest {
	t.Helper()
	requests := make(chan wire.InputRequest, 8)
	go func() {
		defer close(requests)
		for {
			var message wire.Message
			if err := conn.ReadJSON(&message); err != nil {
				return
			}
			if message.Type != wire.MessageInputRequest || message.InputRequest == nil {
				continue
			}
			request := *message.InputRequest
			requests <- request
			result := wire.InputResult{RequestID: request.RequestID}
			existing, found, err := findExistingSessionInput(context.Background(), source, sessionID, request.Command, "")
			if err == nil && found {
				result.Command = &existing
				if err := conn.WriteJSON(wire.Message{Type: wire.MessageInputResult, InputResult: &result}); err != nil {
					return
				}
				continue
			}
			items, _, loadErr := source.LoadItems(context.Background(), sessionID)
			if err == nil {
				err = loadErr
			}
			if err == nil {
				turnID := sessionlog.LastOpenTurnID(items)
				switch {
				case request.ExpectedTurnID != "" && request.ExpectedTurnID != turnID:
					err = errTurnMismatch
				case request.SteeringOnly && turnID == "":
					err = errNoActiveTurn
				case turnID != "":
					command := steeringCommandForRequest(request, turnID)
					var stored wire.Command
					stored, err = appendAcceptedSessionInput(context.Background(), source, sessionID, command)
					if err == nil {
						result.Command = &stored
					}
				case hasPendingMessageCommand(items):
					err = errSessionBusy
				default:
					command := messageCommandForRequest(request)
					var stored wire.Command
					stored, err = appendAcceptedSessionInput(context.Background(), source, sessionID, command)
					if err == nil {
						result.Command = &stored
					}
				}
			}
			if err != nil {
				result.ErrorCode, result.Error = workerInputErrorDetails(err)
				if errors.Is(err, errSessionBusy) {
					result.ErrorCode = "command_pending"
				}
			}
			if err := conn.WriteJSON(wire.Message{Type: wire.MessageInputResult, InputResult: &result}); err != nil {
				return
			}
		}
	}()
	return requests
}

func serveTestWorkerConnectionInputs(connection *workerConnection, source sessionlog.ItemSource, sessionID string) {
	go func() {
		for {
			select {
			case <-connection.doneCh:
				return
			case message := <-connection.sendCh:
				if message.Type != wire.MessageInputRequest || message.InputRequest == nil {
					continue
				}
				request := *message.InputRequest
				result := wire.InputResult{RequestID: request.RequestID}
				items, _, err := source.LoadItems(context.Background(), sessionID)
				if err == nil {
					turnID := sessionlog.LastOpenTurnID(items)
					command := messageCommandForRequest(request)
					if turnID != "" {
						command = steeringCommandForRequest(request, turnID)
					}
					var stored wire.Command
					stored, err = appendAcceptedSessionInput(context.Background(), source, sessionID, command)
					if err == nil {
						result.Command = &stored
					}
				}
				if err != nil {
					result.ErrorCode, result.Error = workerInputErrorDetails(err)
				}
				connection.resolveInput(result)
			}
		}
	}()
}

func postSteering(t *testing.T, handler http.Handler, sessionID string, text string) {
	t.Helper()

	rec := httptest.NewRecorder()
	body := fmt.Sprintf(`{"text":%q}`, text)
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+sessionID+"/steering", strings.NewReader(body))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	var command publicCommandResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &command); err != nil {
		t.Fatalf("decode steering response: %v", err)
	}
}

func tinyPNGBase64() string {
	return "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
}

func tooManyImagesBody() string {
	parts := make([]string, 0, maxMessageImages+1)
	for range maxMessageImages + 1 {
		parts = append(parts, fmt.Sprintf(`{"mime_type":"image/png","data_base64":%q}`, tinyPNGBase64()))
	}
	return `{"text":"hello","images":[` + strings.Join(parts, ",") + `]}`
}

func appendTurnStarted(t *testing.T, source *fakeDisplayItemSource, sessionID string, turnID string) {
	t.Helper()
	if _, err := source.AppendItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnStarted,
		Turn: &zotigosession.DisplayTurn{ID: turnID},
	}); err != nil {
		t.Fatalf("append turn started: %v", err)
	}
}

func appendPendingApprovalTurn(t *testing.T, source *fakeDisplayItemSource, sessionID string, turnID string, approvalID string) {
	t.Helper()
	if _, err := source.AppendItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemApprovalRequest,
		Approval: &zotigosession.DisplayApproval{
			ID:     approvalID,
			TurnID: turnID,
			Pending: []zotigosession.DisplayPendingApproval{{
				ToolCallID: "call-1",
				ToolName:   "shell",
			}},
		},
	}); err != nil {
		t.Fatalf("append approval request: %v", err)
	}
	if _, err := source.AppendItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnPaused,
		Turn: &zotigosession.DisplayTurn{
			ID:     turnID,
			Reason: "need_approval",
		},
	}); err != nil {
		t.Fatalf("append turn paused: %v", err)
	}
}

func getCommands(t *testing.T, handler http.Handler, path string) wire.CommandsPage {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var resp wire.CommandsPage
	if err := sonic.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode commands response: %v", err)
	}
	return resp
}

func getItems(t *testing.T, handler http.Handler, path string) itemsResponse {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var resp itemsResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode items response: %v", err)
	}
	return resp
}

func makeDisplayItems(count int) []zotigosession.DisplayItem {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	items := make([]zotigosession.DisplayItem, 0, count)
	for i := 1; i <= count; i++ {
		items = append(items, zotigosession.DisplayItem{
			ID:        fmt.Sprintf("item_sess-test_%d", i),
			Sequence:  uint64(i),
			Type:      zotigosession.DisplayItemUserMessage,
			Role:      string(protocol.RoleUser),
			Content:   []zotigosession.DisplayContentPart{{Type: string(protocol.ContentTypeText), Text: "message"}},
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		})
	}
	return items
}

func assertItemSequences(t *testing.T, items []itemResponse, expected []uint64) {
	t.Helper()
	if len(items) != len(expected) {
		t.Fatalf("expected %d items, got %d", len(expected), len(items))
	}
	for idx, sequence := range expected {
		if items[idx].Sequence != sequence {
			t.Fatalf("item %d: expected sequence %d, got %d", idx, sequence, items[idx].Sequence)
		}
	}
}
