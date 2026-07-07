package zotigod

import (
	"context"
	"encoding/base64"
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

	"github.com/bytedance/sonic"
	"github.com/gorilla/websocket"
	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/core/tools"
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

type fakeDisplayItemSource struct {
	mu             sync.Mutex
	items          map[string][]zotigosession.DisplayItem
	err            error
	appendErr      func(sessionID string, item zotigosession.DisplayItem) error
	appendHook     func(sessionID string, item zotigosession.DisplayItem)
	offsetCalls    atomic.Int32
	offsetMaxLines []int
}

func (s *fakeDisplayItemSource) LoadItems(_ context.Context, sessionID string) ([]zotigosession.DisplayItem, bool, error) {
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

func createSession(t *testing.T, handler http.Handler) Session {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions", nil))
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
	if err := decodeAPIData(t, rec.Body.Bytes(), &body); err != nil {
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
	if err := decodeAPIData(t, rec.Body.Bytes(), &list); err != nil {
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

func TestAPIErrorEnvelopeContract(t *testing.T) {
	handler := NewHandler()

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

func TestSessionsCreatePersistsWorkingDirectory(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{store: store})
	workDir := t.TempDir()

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

func TestSessionsCreateRejectsInvalidWorkingDirectory(t *testing.T) {
	handler := NewHandler()

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

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions", nil))
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
	handler := NewHandler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d: %s", http.StatusInternalServerError, rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var list sessionListResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(list.Sessions) != 0 {
		t.Fatalf("expected no sessions after failed create, got %#v", list.Sessions)
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
	if err := decodeAPIData(t, getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if got.ID != created.ID || got.State != SessionStateCreated {
		t.Fatalf("unexpected session: %#v", got)
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
	store, err := zotigosession.NewFileStore(t.TempDir())
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

	source := storedDisplayItemSource{store: store}
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
	handler := NewHandler()
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

func TestSessionStartLaunchesWorkerAndWaitsForConnection(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	var server *httptest.Server
	workers := newWorkerRegistry()
	workerReady := make(chan *websocket.Conn, 1)
	launcher := workerLauncherFunc(func(_ context.Context, sessionID string, _ string) error {
		go func() {
			url := "ws" + strings.TrimPrefix(server.URL, "http") + "/internal/workers/connect?session_id=" + sessionID
			conn, _, err := websocket.DefaultDialer.Dial(url, nil)
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
	workerReady := make(chan *websocket.Conn, 1)
	workDir := t.TempDir()
	var launchedWorkDir string
	launcher := workerLauncherFunc(func(_ context.Context, sessionID string, workingDirectory string) error {
		launchedWorkDir = workingDirectory
		go func() {
			url := "ws" + strings.TrimPrefix(server.URL, "http") + "/internal/workers/connect?session_id=" + sessionID
			conn, _, err := websocket.DefaultDialer.Dial(url, nil)
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
	if err := decodeAPIData(t, rec.Body.Bytes(), &running); err != nil {
		t.Fatalf("decode attach response: %v", err)
	}
	if running.State != SessionStateRunning {
		t.Fatalf("expected state %q, got %q", SessionStateRunning, running.State)
	}
}

func TestWorkerConnectTransitionsToRunningAndReceivesCommands(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)

	worker := dialWorker(t, server, created.ID)
	defer worker.Close()

	if got := getSession(t, handler, created.ID); got.State != SessionStateRunning {
		t.Fatalf("expected state %q, got %q", SessionStateRunning, got.State)
	}
	appendTurnStarted(t, source, created.ID, "turn-1")

	postSteering(t, handler, created.ID, "use the smaller fix")
	msg := readWorkerMessage(t, worker)
	if msg.Type != workerMessageCommand || msg.Command == nil {
		t.Fatalf("expected command message, got %#v", msg)
	}
	if msg.Command.Type != sessionCommandSteering || msg.Command.Steering == nil || msg.Command.Steering.Text != "use the smaller fix" {
		t.Fatalf("unexpected steering command: %#v", msg.Command)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/pause", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, rec.Code, rec.Body.String())
	}
	msg = readWorkerMessage(t, worker)
	if msg.Type != workerMessageCommand || msg.Command == nil || msg.Command.Type != sessionCommandPause {
		t.Fatalf("expected pause command, got %#v", msg)
	}
}

func TestWorkerHeartbeatUnregistersUnresponsiveConnection(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	workers := newWorkerRegistry()
	workers.pingInterval = 10 * time.Millisecond
	workers.pongWait = 30 * time.Millisecond
	handler := newHandler(newSessionRegistry(), source, handlerOptions{workers: workers})
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()

	if !workers.Has(created.ID) {
		t.Fatal("expected worker to be registered")
	}
	for deadline := time.Now().Add(time.Second); workers.Has(created.ID) && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if workers.Has(created.ID) {
		t.Fatal("expected heartbeat timeout to unregister worker")
	}
}

func TestWorkerClientKeepaliveClosesUnresponsiveDaemon(t *testing.T) {
	serverConnReady := make(chan *websocket.Conn, 1)
	releaseServerConn := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := workerUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverConnReady <- conn
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
	defer clientConn.Close()
	serverConn := <-serverConnReady
	defer serverConn.Close()

	stopKeepalive := startWorkerClientKeepalive(clientConn, 10*time.Millisecond, 30*time.Millisecond)
	defer stopKeepalive()

	if _, _, err := clientConn.ReadMessage(); err == nil {
		t.Fatal("expected worker websocket read to fail after pong timeout")
	}
}

func TestWorkerCommandReaderFailsWhenBufferIsFull(t *testing.T) {
	serverConnReady := make(chan *websocket.Conn, 1)
	releaseServerConn := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := workerUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverConnReady <- conn
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
	defer clientConn.Close()
	serverConn := <-serverConnReady
	defer serverConn.Close()

	_, errCh := readWorkerCommands(clientConn)
	for idx := uint64(1); idx <= workerCommandBufferSize+1; idx++ {
		msg := workerMessage{
			Type: workerMessageCommand,
			Command: &commandResponse{
				ID:       fmt.Sprintf("cmd-%d", idx),
				Sequence: idx,
				Type:     sessionCommandPause,
			},
		}
		if err := serverConn.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatalf("set write deadline: %v", err)
		}
		if err := serverConn.WriteJSON(msg); err != nil {
			t.Fatalf("write command %d: %v", idx, err)
		}
	}

	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "buffer full") {
			t.Fatalf("expected buffer full error, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("expected worker command reader to fail")
	}
}

func TestReplayWorkerCommandsFetchesMultiplePages(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("limit"); got != strconv.Itoa(maxCommandsLimit) {
			t.Fatalf("expected limit %d, got %q", maxCommandsLimit, got)
		}
		offset := r.URL.Query().Get("offset")
		requests++
		switch offset {
		case "0":
			commands := make([]commandResponse, 0, maxCommandsLimit)
			for sequence := uint64(1); sequence <= maxCommandsLimit; sequence++ {
				commands = append(commands, commandResponse{
					ID:       fmt.Sprintf("cmd-%d", sequence),
					Sequence: sequence,
					Type:     "noop",
				})
			}
			writeJSON(w, http.StatusOK, commandsResponse{
				Commands:   commands,
				NextCursor: strconv.Itoa(maxCommandsLimit),
				NextOffset: 1,
			})
		case "1":
			writeJSON(w, http.StatusOK, commandsResponse{
				Commands: []commandResponse{{
					ID:       "cmd-201",
					Sequence: uint64(maxCommandsLimit + 1),
					Type:     "noop",
				}},
				NextCursor: strconv.Itoa(maxCommandsLimit + 1),
				NextOffset: 2,
			})
		default:
			t.Fatalf("unexpected offset %q", offset)
		}
	}))
	defer server.Close()

	cursor, err := replayWorkerCommands(context.Background(), server.Client(), server.URL, "sess-replay", &workerRuntime{}, workerCommandCursor{})
	if err != nil {
		t.Fatalf("replay commands: %v", err)
	}
	if requests != 2 {
		t.Fatalf("expected 2 command fetches, got %d", requests)
	}
	if cursor.Sequence != uint64(maxCommandsLimit+1) || cursor.Offset != 2 {
		t.Fatalf("unexpected cursor after replay: %#v", cursor)
	}

	saved, err := loadWorkerCommandCursor(context.Background(), nil, "sess-replay")
	if err != nil {
		t.Fatalf("load saved cursor: %v", err)
	}
	if saved != cursor {
		t.Fatalf("expected saved cursor %#v, got %#v", cursor, saved)
	}
}

func TestWorkerRuntimeRejectsMalformedTypedCommand(t *testing.T) {
	runtime := &workerRuntime{}
	if err := runtime.HandleCommand(context.Background(), commandResponse{
		ID:       "cmd-1",
		Sequence: 1,
		Type:     sessionCommandMessage,
	}); err == nil || !strings.Contains(err.Error(), "invalid message command payload") {
		t.Fatalf("expected invalid message payload error, got %v", err)
	}
	if err := runtime.HandleCommand(context.Background(), commandResponse{
		ID:       "cmd-2",
		Sequence: 2,
		Type:     sessionCommandPause,
		Message:  &messageCommandPayload{Text: "wrong"},
		Pause:    &pauseCommandPayload{Reason: userPauseReason},
	}); err == nil || !strings.Contains(err.Error(), "invalid pause command payload") {
		t.Fatalf("expected invalid pause payload error, got %v", err)
	}
}

func TestReplayWorkerCommandsDoesNotAdvanceMalformedCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, commandsResponse{
			Commands: []commandResponse{{
				ID:       "cmd-1",
				Sequence: 1,
				Type:     sessionCommandMessage,
			}},
			NextCursor: "1",
			NextOffset: 1,
		})
	}))
	defer server.Close()

	cursor, err := replayWorkerCommands(context.Background(), server.Client(), server.URL, "sess-malformed", &workerRuntime{}, workerCommandCursor{})
	if err == nil || !strings.Contains(err.Error(), "invalid message command payload") {
		t.Fatalf("expected malformed command error, got %v", err)
	}
	if cursor.Sequence != 0 || cursor.Offset != 0 {
		t.Fatalf("expected cursor not to advance, got %#v", cursor)
	}
	saved, err := loadWorkerCommandCursor(context.Background(), nil, "sess-malformed")
	if err != nil {
		t.Fatalf("load saved cursor: %v", err)
	}
	if saved.Sequence != 0 || saved.Offset != 0 {
		t.Fatalf("expected saved cursor not to advance, got %#v", saved)
	}
}

func TestLoadWorkerCommandCursorDoesNotSkipPendingMessage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	defer store.Close()
	sessionID := "sess-corrupt-cursor"
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:        sessionID,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	item, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemUserMessage,
		Command: &zotigosession.DisplayCommand{
			Type: sessionCommandMessage,
			Text: "already applied",
		},
	})
	if err != nil {
		t.Fatalf("append command item: %v", err)
	}
	_ = item
	if err := os.MkdirAll(filepath.Dir(workerCommandCursorPath(sessionID)), 0755); err != nil {
		t.Fatalf("create cursor dir: %v", err)
	}
	if err := os.WriteFile(workerCommandCursorPath(sessionID), []byte("{"), 0644); err != nil {
		t.Fatalf("write corrupt cursor: %v", err)
	}

	cursor, err := loadWorkerCommandCursor(context.Background(), store, sessionID)
	if err != nil {
		t.Fatalf("load worker command cursor: %v", err)
	}
	if cursor.Sequence != 0 || cursor.Offset != 0 {
		t.Fatalf("expected corrupt cursor recovery not to skip pending message, got %#v", cursor)
	}
}

func TestLoadWorkerCommandCursorRecoversAppliedMessage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	defer store.Close()
	sessionID := "sess-applied-cursor"
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:        sessionID,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	item, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemUserMessage,
		Command: &zotigosession.DisplayCommand{
			Type: sessionCommandMessage,
			Text: "already applied",
		},
	})
	if err != nil {
		t.Fatalf("append command item: %v", err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnStarted,
		Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
	}); err != nil {
		t.Fatalf("append turn started: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(workerCommandCursorPath(sessionID)), 0755); err != nil {
		t.Fatalf("create cursor dir: %v", err)
	}
	if err := os.WriteFile(workerCommandCursorPath(sessionID), []byte("{"), 0644); err != nil {
		t.Fatalf("write corrupt cursor: %v", err)
	}

	cursor, err := loadWorkerCommandCursor(context.Background(), store, sessionID)
	if err != nil {
		t.Fatalf("load worker command cursor: %v", err)
	}
	if cursor.Sequence != item.Sequence || cursor.Offset != 0 {
		t.Fatalf("expected recovered applied message sequence %d, got %#v", item.Sequence, cursor)
	}
}

func TestLoadWorkerCommandCursorDowngradesUnsafeSequence(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	defer store.Close()
	sessionID := "sess-unsafe-sequence"
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:        sessionID,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemUserMessage,
		Command: &zotigosession.DisplayCommand{
			Type: sessionCommandMessage,
			Text: "pending",
		},
	}); err != nil {
		t.Fatalf("append pending command: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(workerCommandCursorPath(sessionID)), 0755); err != nil {
		t.Fatalf("create cursor dir: %v", err)
	}
	if err := os.WriteFile(workerCommandCursorPath(sessionID), []byte(`{"offset":0,"sequence":999}`), 0644); err != nil {
		t.Fatalf("write corrupt cursor: %v", err)
	}

	cursor, err := loadWorkerCommandCursor(context.Background(), store, sessionID)
	if err != nil {
		t.Fatalf("load worker command cursor: %v", err)
	}
	if cursor.Sequence != 0 || cursor.Offset != 0 {
		t.Fatalf("expected unsafe sequence to recover to pending boundary, got %#v", cursor)
	}
}

func TestLoadWorkerCommandCursorDowngradesInvalidOffset(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	defer store.Close()
	sessionID := "sess-invalid-offset"
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:        sessionID,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	item, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemUserMessage,
		Command: &zotigosession.DisplayCommand{
			Type: sessionCommandMessage,
			Text: "applied",
		},
	})
	if err != nil {
		t.Fatalf("append command: %v", err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnStarted,
		Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
	}); err != nil {
		t.Fatalf("append turn started: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(workerCommandCursorPath(sessionID)), 0755); err != nil {
		t.Fatalf("create cursor dir: %v", err)
	}
	if err := os.WriteFile(workerCommandCursorPath(sessionID), []byte(`{"offset":999999,"sequence":1}`), 0644); err != nil {
		t.Fatalf("write corrupt cursor: %v", err)
	}

	cursor, err := loadWorkerCommandCursor(context.Background(), store, sessionID)
	if err != nil {
		t.Fatalf("load worker command cursor: %v", err)
	}
	if cursor.Sequence != item.Sequence || cursor.Offset != 0 {
		t.Fatalf("expected invalid offset to recover to safe sequence %d, got %#v", item.Sequence, cursor)
	}
}

func TestLoadWorkerCommandCursorDowngradesOffsetBeforePendingCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	defer store.Close()
	sessionID := "sess-pending-offset"
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:        sessionID,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemUserMessage,
		Command: &zotigosession.DisplayCommand{
			Type: sessionCommandMessage,
			Text: "pending",
		},
	}); err != nil {
		t.Fatalf("append pending command: %v", err)
	}
	_, _, eofOffset, err := store.ListDisplayItemsFromOffset(context.Background(), sessionID, 0, 10)
	if err != nil {
		t.Fatalf("read display log offset: %v", err)
	}
	if eofOffset <= 0 {
		t.Fatalf("expected positive display log offset, got %d", eofOffset)
	}
	if err := os.MkdirAll(filepath.Dir(workerCommandCursorPath(sessionID)), 0755); err != nil {
		t.Fatalf("create cursor dir: %v", err)
	}
	cursorData := fmt.Sprintf(`{"offset":%d,"sequence":0}`, eofOffset)
	if err := os.WriteFile(workerCommandCursorPath(sessionID), []byte(cursorData), 0644); err != nil {
		t.Fatalf("write cursor: %v", err)
	}

	cursor, err := loadWorkerCommandCursor(context.Background(), store, sessionID)
	if err != nil {
		t.Fatalf("load worker command cursor: %v", err)
	}
	if cursor.Sequence != 0 || cursor.Offset != 0 {
		t.Fatalf("expected offset downgrade before pending command, got %#v", cursor)
	}
}

func TestAdvanceWorkerCommandOffsetFindsAppliedCommand(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	defer store.Close()
	sessionID := "sess-live-offset"
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:        sessionID,
			CreatedAt: now,
			UpdatedAt: now,
		},
	}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{Type: zotigosession.DisplayItemAssistantMessage}); err != nil {
		t.Fatalf("append non-command: %v", err)
	}
	command, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemUserMessage,
		Command: &zotigosession.DisplayCommand{
			Type: sessionCommandMessage,
			Text: "live",
		},
	})
	if err != nil {
		t.Fatalf("append command: %v", err)
	}

	offset := advanceWorkerCommandOffset(context.Background(), store, sessionID, 0, command.Sequence)
	if offset <= 0 {
		t.Fatalf("expected live command offset to advance, got %d", offset)
	}
	items, _, nextOffset, err := store.ListDisplayItemsFromOffset(context.Background(), sessionID, offset, 10)
	if err != nil {
		t.Fatalf("read advanced offset: %v", err)
	}
	if len(items) != 0 || nextOffset != offset {
		t.Fatalf("expected offset at end of complete log, got items=%#v next=%d offset=%d", items, nextOffset, offset)
	}
}

func TestWorkerClientInitializesRuntimeBeforeConnect(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	t.Chdir(workDir)
	if err := os.WriteFile(filepath.Join(workDir, "zotigo.yaml"), []byte("default_profile: ["), 0644); err != nil {
		t.Fatalf("write invalid project config: %v", err)
	}

	var connected atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/workers/connect" {
			connected.Store(true)
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	err := runWorkerClient(context.Background(), workerClientConfig{
		DaemonURL: server.URL,
		SessionID: "sess-init-before-connect",
	})
	if err == nil {
		t.Fatal("expected worker runtime initialization to fail")
	}
	if connected.Load() {
		t.Fatal("worker connected before runtime initialization completed")
	}
}

func TestWorkerConnectRejectsNonLiveSession(t *testing.T) {
	handler := NewHandler()
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/internal/workers/connect?session_id=" + created.ID
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if conn != nil {
		conn.Close()
	}
	if err == nil {
		t.Fatal("expected worker connect to fail")
	}
	if resp == nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected status %d, got response %#v err %v", http.StatusConflict, resp, err)
	}
}

func TestWorkerSessionLockRejectsReuse(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}
	defer store.Close()

	unlock, err := acquireWorkerSessionLock(context.Background(), store, "sess-lock")
	if err != nil {
		t.Fatalf("acquire first lock: %v", err)
	}
	if _, err := acquireWorkerSessionLock(context.Background(), store, "sess-lock"); err == nil {
		t.Fatal("expected second lock to fail")
	}
	unlock()

	unlock, err = acquireWorkerSessionLock(context.Background(), store, "sess-lock")
	if err != nil {
		t.Fatalf("expected lock after unlock: %v", err)
	}
	unlock()
}

func TestSessionMessageCreatesDisplayItemAndWorkerCommand(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(`{"text":"build the runtime"}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	var command publicCommandResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &command); err != nil {
		t.Fatalf("decode message response: %v", err)
	}
	if command.Type != sessionCommandMessage || command.Text != "build the runtime" {
		t.Fatalf("unexpected message command: %#v", command)
	}

	msg := readWorkerMessage(t, worker)
	if msg.Command == nil || msg.Command.Type != sessionCommandMessage || msg.Command.Message == nil || msg.Command.Message.Text != "build the runtime" {
		t.Fatalf("unexpected worker message command: %#v", msg)
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 1 || items.Items[0].Type != string(zotigosession.DisplayItemUserMessage) ||
		items.Items[0].Command == nil || items.Items[0].Command.Type != sessionCommandMessage {
		t.Fatalf("expected user message display item, got %#v", items.Items)
	}

	commands := getCommands(t, handler, "/internal/sessions/"+created.ID+"/commands?after=0")
	if len(commands.Commands) != 1 || commands.Commands[0].Type != sessionCommandMessage {
		t.Fatalf("expected one message command, got %#v", commands)
	}
}

func TestSessionMessageAcceptsImageInput(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{store: store})
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()

	imageBase64 := tinyPNGBase64()
	body := fmt.Sprintf(`{"text":"describe this","images":[{"mime_type":"image/png","data_base64":%q}]}`, imageBase64)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(body))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}

	var publicCommand publicCommandResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &publicCommand); err != nil {
		t.Fatalf("decode message response: %v", err)
	}
	if len(publicCommand.Images) != 1 {
		t.Fatalf("expected one public image metadata, got %#v", publicCommand.Images)
	}
	if strings.Contains(rec.Body.String(), "data_base64") || strings.Contains(rec.Body.String(), imageBase64) {
		t.Fatalf("public message response leaked image payload: %s", rec.Body.String())
	}

	msg := readWorkerMessage(t, worker)
	if msg.Command == nil || msg.Command.Message == nil || len(msg.Command.Message.Images) != 1 {
		t.Fatalf("expected worker image command, got %#v", msg)
	}
	if msg.Command.Message.Images[0].DataBase64 == "" {
		t.Fatalf("worker command did not include image data")
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 1 {
		t.Fatalf("expected one display item, got %#v", items.Items)
	}
	item := items.Items[0]
	if len(item.Content) != 2 || item.Content[1].Image == nil {
		t.Fatalf("expected text and image display parts, got %#v", item.Content)
	}
	if item.Content[1].Image.MediaType != "image/png" || item.Content[1].Image.SizeBytes == 0 ||
		item.Content[1].Image.Width != 1 || item.Content[1].Image.Height != 1 {
		t.Fatalf("unexpected image metadata: %#v", item.Content[1].Image)
	}
	encodedItems, err := sonic.MarshalString(items)
	if err != nil {
		t.Fatalf("encode items: %v", err)
	}
	if strings.Contains(encodedItems, imageBase64) {
		t.Fatalf("items response leaked image base64")
	}
}

func TestSessionMessageRejectsImageInputWithoutBlobPersistence(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()

	body := fmt.Sprintf(`{"text":"describe this","images":[{"mime_type":"image/png","data_base64":%q}]}`, tinyPNGBase64())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(body))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d: %s", http.StatusInternalServerError, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "image persistence is not configured") {
		t.Fatalf("expected image persistence error, got %q", rec.Body.String())
	}
}

func TestSessionMessageImageCommandReplaysFromBlob(t *testing.T) {
	root := t.TempDir()
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{store: store})
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()

	imageBase64 := tinyPNGBase64()
	body := fmt.Sprintf(`{"text":"describe this","images":[{"mime_type":"image/png","data_base64":%q}]}`, imageBase64)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(body))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	_ = readWorkerMessage(t, worker)

	rawLog, err := os.ReadFile(filepath.Join(root, "sessions", created.ID+".display.jsonl"))
	if err != nil {
		t.Fatalf("read display log: %v", err)
	}
	if strings.Contains(string(rawLog), imageBase64) {
		t.Fatalf("display log leaked image base64")
	}
	if strings.Contains(string(rawLog), root) {
		t.Fatalf("display log leaked absolute store root: %s", string(rawLog))
	}

	commands := getCommands(t, handler, "/internal/sessions/"+created.ID+"/commands?after=0")
	if len(commands.Commands) != 1 || commands.Commands[0].Message == nil || len(commands.Commands[0].Message.Images) != 1 {
		t.Fatalf("expected replayable image command, got %#v", commands)
	}
	if commands.Commands[0].Message.Images[0].DataBase64 == "" {
		t.Fatalf("replayed command did not hydrate image payload")
	}
}

func TestSessionMessageFailedDuplicateImageDoesNotDeleteAcceptedBlob(t *testing.T) {
	root := t.TempDir()
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{store: store})
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()

	body := fmt.Sprintf(`{"text":"describe this","images":[{"mime_type":"image/png","data_base64":%q}]}`, tinyPNGBase64())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(body))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	_ = readWorkerMessage(t, worker)

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(body))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d: %s", http.StatusConflict, rec.Code, rec.Body.String())
	}

	commands := getCommands(t, handler, "/internal/sessions/"+created.ID+"/commands?after=0")
	if len(commands.Commands) != 1 || commands.Commands[0].Message == nil || len(commands.Commands[0].Message.Images) != 1 ||
		commands.Commands[0].Message.Images[0].DataBase64 == "" {
		t.Fatalf("expected accepted image command to remain replayable, got %#v", commands)
	}
}

func TestWorkerCommandsSkipsMissingImageBlob(t *testing.T) {
	root := t.TempDir()
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{store: store})
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()

	body := fmt.Sprintf(`{"text":"describe this","images":[{"mime_type":"image/png","data_base64":%q}]}`, tinyPNGBase64())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(body))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	_ = readWorkerMessage(t, worker)

	if err := os.RemoveAll(filepath.Join(root, "sessions", created.ID+".images")); err != nil {
		t.Fatalf("remove image dir: %v", err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), created.ID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemSessionCommand,
		Command: &zotigosession.DisplayCommand{
			Type:   sessionCommandPause,
			Reason: userPauseReason,
		},
	}); err != nil {
		t.Fatalf("append pause command: %v", err)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/internal/sessions/"+created.ID+"/commands?after=0", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var commands commandsResponse
	if err := sonic.Unmarshal(rec.Body.Bytes(), &commands); err != nil {
		t.Fatalf("decode commands: %v", err)
	}
	if len(commands.Commands) != 1 || commands.Commands[0].Type != sessionCommandPause {
		t.Fatalf("expected missing image command to be skipped, got %#v", commands)
	}
}

func TestMessageFromCommandIncludesImages(t *testing.T) {
	msg, err := messageFromCommand("item_1", &messageCommandPayload{
		Text: "describe this",
		Images: []commandImageResponse{{
			MimeType:   "image/png",
			DataBase64: tinyPNGBase64(),
		}},
	})
	if err != nil {
		t.Fatalf("messageFromCommand: %v", err)
	}
	if msg.Role != protocol.RoleUser || len(msg.Content) != 2 {
		t.Fatalf("unexpected message: %#v", msg)
	}
	if msg.Content[1].Type != protocol.ContentTypeImage || msg.Content[1].Image == nil ||
		msg.Content[1].Image.MediaType != "image/png" || len(msg.Content[1].Image.Data) == 0 {
		t.Fatalf("expected image content part, got %#v", msg.Content[1])
	}
}

func TestMessageFromCommandRejectsMissingImagePayload(t *testing.T) {
	_, err := messageFromCommand("item_1", &messageCommandPayload{
		Text: "describe this",
		Images: []commandImageResponse{{
			MimeType: "image/png",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "image payload unavailable") {
		t.Fatalf("expected missing image payload error, got %v", err)
	}
}

func TestSessionMessageRejectsInvalidImages(t *testing.T) {
	tests := []struct {
		name string
		body string
		code int
	}{
		{
			name: "invalid base64",
			body: `{"text":"hello","images":[{"mime_type":"image/png","data_base64":"not-base64"}]}`,
			code: http.StatusBadRequest,
		},
		{
			name: "unsupported mime type",
			body: fmt.Sprintf(`{"text":"hello","images":[{"mime_type":"image/gif","data_base64":%q}]}`, tinyPNGBase64()),
			code: http.StatusBadRequest,
		},
		{
			name: "too many images",
			body: tooManyImagesBody(),
			code: http.StatusBadRequest,
		},
		{
			name: "image too large",
			body: fmt.Sprintf(`{"text":"hello","images":[{"mime_type":"image/png","data_base64":%q}]}`,
				base64.StdEncoding.EncodeToString(make([]byte, maxMessageImageBytes+1))),
			code: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
			handler := newHandler(newSessionRegistry(), source)
			server := httptest.NewServer(handler)
			defer server.Close()

			created := createSession(t, handler)
			startSession(t, handler, created.ID)
			worker := dialWorker(t, server, created.ID)
			defer worker.Close()

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(tt.body))
			handler.ServeHTTP(rec, req)
			if rec.Code != tt.code {
				t.Fatalf("expected status %d, got %d: %s", tt.code, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestSessionMessageRejectsOversizedRequestBody(t *testing.T) {
	handler := newHandler(newSessionRegistry(), &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}})
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()

	rec := httptest.NewRecorder()
	body := `{"text":"` + strings.Repeat("x", maxMessageRequestBytes) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(body))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected status %d, got %d: %s", http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	}
}

func TestSessionSteeringRejectsImages(t *testing.T) {
	handler := newHandler(newSessionRegistry(), &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}})
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()

	rec := httptest.NewRecorder()
	body := fmt.Sprintf(`{"text":"adjust","images":[{"mime_type":"image/png","data_base64":%q}]}`, tinyPNGBase64())
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/steering", strings.NewReader(body))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
}

func TestSessionSteeringRejectsOversizedRequestBody(t *testing.T) {
	handler := newHandler(newSessionRegistry(), &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}})
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()

	rec := httptest.NewRecorder()
	body := `{"text":"` + strings.Repeat("x", maxMessageRequestBytes) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/steering", strings.NewReader(body))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected status %d, got %d: %s", http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	}
}

func TestSessionMessageReturnsAcceptedWhenWorkerDisconnectsAfterAppend(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	workers := newWorkerRegistry()
	handler := newHandler(newSessionRegistry(), source, handlerOptions{workers: workers})
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()

	source.appendHook = func(sessionID string, item zotigosession.DisplayItem) {
		if sessionID != created.ID || item.Command == nil || item.Command.Type != sessionCommandMessage {
			return
		}
		_ = worker.Close()
		for deadline := time.Now().Add(time.Second); workers.Has(created.ID) && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(`{"text":"persist this"}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}

	commands := getCommands(t, handler, "/internal/sessions/"+created.ID+"/commands?after=0")
	if len(commands.Commands) != 1 || commands.Commands[0].Message == nil || commands.Commands[0].Message.Text != "persist this" {
		t.Fatalf("expected durable message command, got %#v", commands)
	}
}

func TestSessionMessageRejectsActiveTurn(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	appendTurnStarted(t, source, created.ID, "turn-1")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(`{"text":"second message"}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d: %s", http.StatusConflict, rec.Code, rec.Body.String())
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 1 || items.Items[0].Type != string(zotigosession.DisplayItemTurnStarted) {
		t.Fatalf("expected no message command to be appended, got %#v", items.Items)
	}
}

func TestSessionMessageRejectsPendingMessageCommand(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(`{"text":"first message"}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(`{"text":"second message"}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d: %s", http.StatusConflict, rec.Code, rec.Body.String())
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 1 || items.Items[0].Command == nil || items.Items[0].Command.Type != sessionCommandMessage {
		t.Fatalf("expected only the pending first message command, got %#v", items.Items)
	}
}

func TestSessionMessageAdmissionIsSerialized(t *testing.T) {
	var appendCalls atomic.Int32
	appendEntered := make(chan struct{})
	releaseAppend := make(chan struct{})
	source := &fakeDisplayItemSource{
		items: map[string][]zotigosession.DisplayItem{},
		appendErr: func(_ string, item zotigosession.DisplayItem) error {
			if item.Command == nil || item.Command.Type != sessionCommandMessage {
				return nil
			}
			if appendCalls.Add(1) == 1 {
				close(appendEntered)
			}
			<-releaseAppend
			return nil
		},
	}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()

	results := make(chan int, 2)
	postMessage := func(text string) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(fmt.Sprintf(`{"text":%q}`, text)))
		handler.ServeHTTP(rec, req)
		results <- rec.Code
	}
	go postMessage("first message")
	select {
	case <-appendEntered:
	case <-time.After(time.Second):
		t.Fatal("expected first append to block")
	}
	go postMessage("second message")
	time.Sleep(20 * time.Millisecond)
	close(releaseAppend)

	got := []int{<-results, <-results}
	createdCount := 0
	conflictCount := 0
	for _, code := range got {
		switch code {
		case http.StatusCreated:
			createdCount++
		case http.StatusConflict:
			conflictCount++
		default:
			t.Fatalf("unexpected status codes %#v", got)
		}
	}
	if createdCount != 1 || conflictCount != 1 {
		t.Fatalf("expected one created and one conflict, got %#v", got)
	}
	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 1 || items.Items[0].Command == nil || items.Items[0].Command.Type != sessionCommandMessage {
		t.Fatalf("expected one message command, got %#v", items.Items)
	}
}

func TestWorkerRuntimeCloseInterruptsActiveTurn(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := newWorkerDisplayLog("sess-close", source)
	if _, err := display.StartTurn(context.Background()); err != nil {
		t.Fatalf("start turn: %v", err)
	}
	runtime := &workerRuntime{
		display:    display,
		turnActive: true,
	}

	runtime.Close()
	if err := display.HandleEvent(context.Background(), protocol.NewFinishEvent(protocol.FinishReasonStop)); err != nil {
		t.Fatalf("late finish event should be ignored: %v", err)
	}

	items := source.items["sess-close"]
	if len(items) != 2 {
		t.Fatalf("expected turn_started and turn_interrupted, got %#v", items)
	}
	if items[1].Type != zotigosession.DisplayItemTurnInterrupted || items[1].Turn == nil {
		t.Fatalf("expected turn_interrupted item, got %#v", items[1])
	}
	if items[1].Turn.Reason != controlChannelClosedReason {
		t.Fatalf("expected reason %q, got %#v", controlChannelClosedReason, items[1].Turn)
	}
	if got := lastOpenTurnID(items); got != "" {
		t.Fatalf("expected no open turn, got %q", got)
	}
}

func TestWorkerRuntimeSteeringWaitsForTurnReady(t *testing.T) {
	providers.Register("zotigod-ready-test", func(config.ProfileConfig) (providers.Provider, error) {
		return &noopProvider{}, nil
	})
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := newWorkerDisplayLog("sess-ready", source)
	turnID, err := display.StartTurn(context.Background())
	if err != nil {
		t.Fatalf("start turn: %v", err)
	}
	localExec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	defer localExec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: "zotigod-ready-test"}, localExec)
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	runtime := &workerRuntime{
		agent:      ag,
		display:    display,
		turnActive: true,
		turnReady:  make(chan struct{}),
		turnDone:   make(chan struct{}),
	}

	result := make(chan error, 1)
	go func() {
		result <- runtime.queueTurnUserInput(context.Background(), &steeringCommandPayload{
			TurnID: turnID,
			Text:   "use the smaller fix",
		})
	}()

	select {
	case err := <-result:
		t.Fatalf("steering returned before turn was ready: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	ag.Restore(agent.Snapshot{State: agent.StateRunning})
	runtime.markTurnReady()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("queue steering after turn ready: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("expected steering to apply after turn ready")
	}
}

func TestWorkerRuntimeIgnoresStaleSteeringAfterAgentStops(t *testing.T) {
	providers.Register("zotigod-stale-steering-test", func(config.ProfileConfig) (providers.Provider, error) {
		return &noopProvider{}, nil
	})
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	display := newWorkerDisplayLog("sess-stale-steering", source)
	turnID, err := display.StartTurn(context.Background())
	if err != nil {
		t.Fatalf("start turn: %v", err)
	}
	localExec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	defer localExec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: "zotigod-stale-steering-test"}, localExec)
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	ready := make(chan struct{})
	close(ready)
	runtime := &workerRuntime{
		agent:      ag,
		display:    display,
		turnActive: true,
		turnReady:  ready,
		turnDone:   make(chan struct{}),
		readyDone:  true,
	}

	err = runtime.queueTurnUserInput(context.Background(), &steeringCommandPayload{
		TurnID: turnID,
		Text:   "too late",
	})
	if err != nil {
		t.Fatalf("expected stale steering to be ignored, got %v", err)
	}
}

func TestWorkerDisplayLogInterruptsOpenTurnAfterRestart(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	if _, err := source.AppendItem(context.Background(), "sess-restart", zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnStarted,
		Turn: &zotigosession.DisplayTurn{ID: "turn-stale"},
	}); err != nil {
		t.Fatalf("append stale turn: %v", err)
	}
	display := newWorkerDisplayLog("sess-restart", source)
	if err := display.InterruptOpenTurn(context.Background(), workerRestartedReason); err != nil {
		t.Fatalf("interrupt open turn: %v", err)
	}

	items := source.items["sess-restart"]
	if len(items) != 2 {
		t.Fatalf("expected start and interrupt items, got %#v", items)
	}
	if items[1].Type != zotigosession.DisplayItemTurnInterrupted || items[1].Turn == nil {
		t.Fatalf("expected interrupted turn, got %#v", items[1])
	}
	if items[1].Turn.ID != "turn-stale" || items[1].Turn.Reason != workerRestartedReason {
		t.Fatalf("unexpected interrupted turn: %#v", items[1].Turn)
	}
}

func TestSessionPauseCreatesWorkerCommand(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	if _, err := source.AppendItem(context.Background(), created.ID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnStarted,
		Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
	}); err != nil {
		t.Fatalf("append turn started: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/pause", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, rec.Code, rec.Body.String())
	}
	var command publicCommandResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &command); err != nil {
		t.Fatalf("decode pause response: %v", err)
	}
	if command.Type != sessionCommandPause || command.TurnID != "turn-1" || command.Reason != userPauseReason {
		t.Fatalf("unexpected pause command: %#v", command)
	}
	if got := getSession(t, handler, created.ID); got.State != SessionStateRunning {
		t.Fatalf("expected session to remain %q, got %q", SessionStateRunning, got.State)
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 2 {
		t.Fatalf("expected turn_started and pause command items, got %#v", items.Items)
	}
	if items.Items[1].Type != string(zotigosession.DisplayItemSessionCommand) || items.Items[1].Command == nil {
		t.Fatalf("expected pause command item, got %#v", items.Items[1])
	}

	commands := getCommands(t, handler, "/internal/sessions/"+created.ID+"/commands?after=0")
	if len(commands.Commands) != 1 || commands.Commands[0].Type != sessionCommandPause {
		t.Fatalf("expected one pause command, got %#v", commands)
	}
}

func TestSessionPauseRejectsTurnCompletedDuringAdmission(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	var completed atomic.Bool
	source.appendErr = func(sessionID string, item zotigosession.DisplayItem) error {
		if item.Command == nil || item.Command.Type != sessionCommandPause || !completed.CompareAndSwap(false, true) {
			return nil
		}
		_, err := source.AppendItem(context.Background(), sessionID, zotigosession.DisplayItem{
			Type: zotigosession.DisplayItemTurnCompleted,
			Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
		})
		return err
	}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	appendTurnStarted(t, source, created.ID, "turn-1")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/pause", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d: %s", http.StatusConflict, rec.Code, rec.Body.String())
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 2 || items.Items[1].Type != string(zotigosession.DisplayItemTurnCompleted) {
		t.Fatalf("expected only lifecycle completion to be appended, got %#v", items.Items)
	}
}

func TestSessionPauseRejectsPendingApprovalBeforeRegistryPause(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	attachWorker(t, handler, created.ID)
	appendTurnStarted(t, source, created.ID, "turn-1")
	appendPendingApprovalTurn(t, source, created.ID, "turn-1", "approval-1")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/pause", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d: %s", http.StatusConflict, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "approval is pending") {
		t.Fatalf("expected pending approval conflict, got %q", rec.Body.String())
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 3 {
		t.Fatalf("expected no pause command to be appended, got %#v", items.Items)
	}
}

func TestSessionPauseLaunchesMissingWorker(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	var server *httptest.Server
	workers := newWorkerRegistry()
	workerReady := make(chan *websocket.Conn, 1)
	launcher := workerLauncherFunc(func(_ context.Context, sessionID string, _ string) error {
		go func() {
			url := "ws" + strings.TrimPrefix(server.URL, "http") + "/internal/workers/connect?session_id=" + sessionID
			conn, _, err := websocket.DefaultDialer.Dial(url, nil)
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
	startSession(t, handler, created.ID)
	worker := <-workerReady
	if worker == nil {
		t.Fatal("expected initial worker websocket connection")
	}
	worker.Close()
	for deadline := time.Now().Add(time.Second); workers.Has(created.ID) && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if workers.Has(created.ID) {
		t.Fatal("expected closed worker to unregister")
	}
	appendTurnStarted(t, source, created.ID, "turn-1")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/pause", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, rec.Code, rec.Body.String())
	}
	restarted := <-workerReady
	if restarted == nil {
		t.Fatal("expected restarted worker websocket connection")
	}
	defer restarted.Close()
	msg := readWorkerMessage(t, restarted)
	if msg.Command == nil || msg.Command.Type != sessionCommandPause {
		t.Fatalf("expected pause command after worker restart, got %#v", msg)
	}
}

func TestWorkerTurnInterruptedConfirmsPauseLifecycle(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	if _, err := source.AppendItem(context.Background(), created.ID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnStarted,
		Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
	}); err != nil {
		t.Fatalf("append turn started: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/pause", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, rec.Code, rec.Body.String())
	}
	var pauseCommand publicCommandResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &pauseCommand); err != nil {
		t.Fatalf("decode pause response: %v", err)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/internal/sessions/"+created.ID+"/turn/interrupted", strings.NewReader(`{
		"turn_id":"turn-1",
		"reason":"user_pause",
		"duration_ms":123
	}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	got := items.Items[len(items.Items)-1]
	if got.Type != string(zotigosession.DisplayItemTurnInterrupted) || got.Turn == nil {
		t.Fatalf("expected confirmed turn_interrupted item, got %#v", got)
	}
	if got.Turn.ID != "turn-1" || got.Turn.Reason != userPauseReason || got.Turn.DurationMS != 123 {
		t.Fatalf("unexpected interrupted turn: %#v", got.Turn)
	}

	commands := getCommands(t, handler, fmt.Sprintf("/internal/sessions/%s/commands?after=%d", created.ID, pauseCommand.Sequence))
	if len(commands.Commands) != 0 {
		t.Fatalf("expected lifecycle confirmation not to be returned as command, got %#v", commands)
	}
}

func TestWorkerTurnInterruptedRejectsMissingOrStaleTurn(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	attachWorker(t, handler, created.ID)
	appendTurnStarted(t, source, created.ID, "turn-1")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/internal/sessions/"+created.ID+"/turn/interrupted", strings.NewReader(`{
		"reason":"user_pause"
	}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/internal/sessions/"+created.ID+"/turn/interrupted", strings.NewReader(`{
		"turn_id":"turn-stale",
		"reason":"user_pause"
	}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d: %s", http.StatusConflict, rec.Code, rec.Body.String())
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 1 || items.Items[0].Type != string(zotigosession.DisplayItemTurnStarted) {
		t.Fatalf("expected no interrupted item, got %#v", items.Items)
	}
}

func TestSessionSteeringCreatesDisplayItemAndWorkerCommand(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	appendTurnStarted(t, source, created.ID, "turn-1")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/steering", strings.NewReader(`{"text":"use the smaller fix"}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	var command publicCommandResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &command); err != nil {
		t.Fatalf("decode steering response: %v", err)
	}
	if command.Type != sessionCommandSteering || command.Text != "use the smaller fix" || command.TurnID != "turn-1" {
		t.Fatalf("unexpected steering command: %#v", command)
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 2 || items.Items[1].Type != string(zotigosession.DisplayItemSteeringMessage) || items.Items[1].Turn == nil || items.Items[1].Turn.ID != "turn-1" {
		t.Fatalf("expected steering display item, got %#v", items.Items)
	}

	commands := getCommands(t, handler, "/internal/sessions/"+created.ID+"/commands?after=0")
	if len(commands.Commands) != 1 || commands.Commands[0].Steering == nil ||
		commands.Commands[0].Steering.Text != "use the smaller fix" || commands.Commands[0].Steering.TurnID != "turn-1" {
		t.Fatalf("expected one steering command, got %#v", commands)
	}
}

func TestSessionSteeringRejectsTurnCompletedDuringAdmission(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	var completed atomic.Bool
	source.appendErr = func(sessionID string, item zotigosession.DisplayItem) error {
		if item.Type != zotigosession.DisplayItemSteeringMessage || !completed.CompareAndSwap(false, true) {
			return nil
		}
		_, err := source.AppendItem(context.Background(), sessionID, zotigosession.DisplayItem{
			Type: zotigosession.DisplayItemTurnCompleted,
			Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
		})
		return err
	}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	appendTurnStarted(t, source, created.ID, "turn-1")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/steering", strings.NewReader(`{"text":"too late"}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d: %s", http.StatusConflict, rec.Code, rec.Body.String())
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 2 || items.Items[1].Type != string(zotigosession.DisplayItemTurnCompleted) {
		t.Fatalf("expected only lifecycle completion to be appended, got %#v", items.Items)
	}
}

func TestSessionSteeringRejectsPendingApprovalBeforeRegistryPause(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	attachWorker(t, handler, created.ID)
	appendTurnStarted(t, source, created.ID, "turn-1")
	appendPendingApprovalTurn(t, source, created.ID, "turn-1", "approval-1")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/steering", strings.NewReader(`{"text":"use another command"}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d: %s", http.StatusConflict, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "approval is pending") {
		t.Fatalf("expected pending approval conflict, got %q", rec.Body.String())
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 3 {
		t.Fatalf("expected no steering item to be appended, got %#v", items.Items)
	}
}

func TestWorkerCommandsReturnSteeringItemsUnmerged(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	appendTurnStarted(t, source, created.ID, "turn-1")

	postSteering(t, handler, created.ID, "first correction")
	postSteering(t, handler, created.ID, "second correction")
	if _, err := source.AppendItem(context.Background(), created.ID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemAssistantMessage,
		Role: string(protocol.RoleAssistant),
		Content: []zotigosession.DisplayContentPart{{
			Type: string(protocol.ContentTypeToolResult),
			ToolResult: &zotigosession.DisplayToolResult{
				ToolCallID: "call-1",
				ToolName:   "shell",
				ResultType: string(protocol.ToolResultTypeText),
				Text:       "done",
			},
		}},
	}); err != nil {
		t.Fatalf("append tool result boundary: %v", err)
	}
	postSteering(t, handler, created.ID, "third correction")

	commands := getCommands(t, handler, "/internal/sessions/"+created.ID+"/commands?after=0")
	if len(commands.Commands) != 3 {
		t.Fatalf("expected three commands, got %#v", commands)
	}
	if commands.Commands[0].Steering == nil || commands.Commands[0].Steering.Text != "first correction" {
		t.Fatalf("unexpected first steering command: %#v", commands.Commands[0])
	}
	if commands.Commands[1].Steering == nil || commands.Commands[1].Steering.Text != "second correction" {
		t.Fatalf("unexpected second steering command: %#v", commands.Commands[1])
	}
	if commands.Commands[2].Steering == nil || commands.Commands[2].Steering.Text != "third correction" {
		t.Fatalf("unexpected third steering command: %#v", commands.Commands[2])
	}

	afterFirst := getCommands(t, handler, fmt.Sprintf("/internal/sessions/%s/commands?after=%d", created.ID, commands.Commands[0].Sequence))
	if len(afterFirst.Commands) != 2 || afterFirst.Commands[0].Steering == nil || afterFirst.Commands[0].Steering.Text != "second correction" ||
		afterFirst.Commands[1].Steering == nil || afterFirst.Commands[1].Steering.Text != "third correction" {
		t.Fatalf("expected later steering after cursor, got %#v", afterFirst)
	}
}

func TestWorkerCommandsSupportDisplayLogOffset(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	source := storedDisplayItemSource{store: store}
	handler := newHandler(newSessionRegistry(), source)
	created := createSession(t, handler)

	if _, err := source.AppendItem(context.Background(), created.ID, zotigosession.DisplayItem{Type: zotigosession.DisplayItemTurnStarted}); err != nil {
		t.Fatalf("append non-command: %v", err)
	}
	if _, err := source.AppendItem(context.Background(), created.ID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemSessionCommand,
		Command: &zotigosession.DisplayCommand{
			Type: sessionCommandMessage,
			Text: "first",
		},
	}); err != nil {
		t.Fatalf("append first command: %v", err)
	}
	if _, err := source.AppendItem(context.Background(), created.ID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemSessionCommand,
		Command: &zotigosession.DisplayCommand{
			Type: sessionCommandPause,
		},
	}); err != nil {
		t.Fatalf("append second command: %v", err)
	}

	first := getCommands(t, handler, "/internal/sessions/"+created.ID+"/commands?offset=0&limit=1")
	if len(first.Commands) != 1 || first.Commands[0].Type != sessionCommandMessage {
		t.Fatalf("expected first command, got %#v", first)
	}
	if first.NextOffset <= 0 {
		t.Fatalf("expected positive next offset, got %d", first.NextOffset)
	}

	second := getCommands(t, handler, fmt.Sprintf("/internal/sessions/%s/commands?offset=%d&limit=1", created.ID, first.NextOffset))
	if len(second.Commands) != 1 || second.Commands[0].Type != sessionCommandPause {
		t.Fatalf("expected second command, got %#v", second)
	}
	if second.NextOffset <= first.NextOffset {
		t.Fatalf("expected offset to advance, got %d after %d", second.NextOffset, first.NextOffset)
	}

	if _, _, _, err := store.ListDisplayItemsFromOffset(context.Background(), created.ID, second.NextOffset, 10); err != nil {
		t.Fatalf("offset should point to a valid display log boundary: %v", err)
	}
}

func TestWorkerCommandsOffsetScansInBatches(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	created := createSession(t, handler)
	for idx := 0; idx < 20; idx++ {
		if _, err := source.AppendItem(context.Background(), created.ID, zotigosession.DisplayItem{Type: zotigosession.DisplayItemAssistantMessage}); err != nil {
			t.Fatalf("append non-command: %v", err)
		}
	}
	if _, err := source.AppendItem(context.Background(), created.ID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemSessionCommand,
		Command: &zotigosession.DisplayCommand{
			Type: sessionCommandMessage,
			Text: "batched",
		},
	}); err != nil {
		t.Fatalf("append command: %v", err)
	}

	commands := getCommands(t, handler, "/internal/sessions/"+created.ID+"/commands?offset=0&limit=1")
	if len(commands.Commands) != 1 || commands.Commands[0].Message == nil || commands.Commands[0].Message.Text != "batched" {
		t.Fatalf("expected command after batched scan, got %#v", commands)
	}
	if got := source.offsetCalls.Load(); got != 1 {
		t.Fatalf("expected one batched offset read, got %d", got)
	}
	if len(source.offsetMaxLines) != 1 || source.offsetMaxLines[0] != commandOffsetScanLines {
		t.Fatalf("expected maxLines %d, got %#v", commandOffsetScanLines, source.offsetMaxLines)
	}
}

func TestSessionControlsRejectInvalidRequests(t *testing.T) {
	handler := NewHandler()
	created := createSession(t, handler)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/pause", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d", http.StatusConflict, rec.Code)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/steering", strings.NewReader(`{"text":"later"}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d", http.StatusConflict, rec.Code)
	}

	startSession(t, handler, created.ID)
	attachWorker(t, handler, created.ID)

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/pause", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d", http.StatusConflict, rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/steering", strings.NewReader(`{"text":"   "}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/steering", strings.NewReader(`{"text":"later"}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d", http.StatusConflict, rec.Code)
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 0 {
		t.Fatalf("expected offline steering to leave no display item, got %#v", items.Items)
	}
}

func TestApprovalRequestFlowCreatesItemsAndAcceptsDecision(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	attachWorker(t, handler, created.ID)

	approval := createApprovalRequestForTest(t, handler, created.ID)
	if approval.Status != approvalStatusPending {
		t.Fatalf("expected status %q, got %q", approvalStatusPending, approval.Status)
	}
	if approval.TurnID != "turn-1" || len(approval.Pending) != 1 {
		t.Fatalf("unexpected approval response: %#v", approval)
	}
	if got := getSession(t, handler, created.ID); got.State != SessionStatePaused {
		t.Fatalf("expected state %q, got %q", SessionStatePaused, got.State)
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 2 {
		t.Fatalf("expected approval request and turn paused items, got %d", len(items.Items))
	}
	if items.Items[0].Type != string(zotigosession.DisplayItemApprovalRequest) {
		t.Fatalf("expected approval_request item, got %s", items.Items[0].Type)
	}
	if items.Items[0].Approval == nil || items.Items[0].Approval.ID != approval.ID {
		t.Fatalf("unexpected approval item: %#v", items.Items[0].Approval)
	}
	if got := items.Items[0].Approval.Pending[0]; got.ToolCallID != "call-1" || got.ToolName != "shell" || got.Reason != "writes files" {
		t.Fatalf("unexpected pending approval: %#v", got)
	}
	if items.Items[1].Type != string(zotigosession.DisplayItemTurnPaused) {
		t.Fatalf("expected turn_paused item, got %s", items.Items[1].Type)
	}
	if items.Items[1].Turn == nil || items.Items[1].Turn.ID != "turn-1" || items.Items[1].Turn.Reason != "need_approval" {
		t.Fatalf("unexpected turn paused item: %#v", items.Items[1].Turn)
	}

	approved := true
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/approvals/"+approval.ID, strings.NewReader(`{
		"decisions": [{"tool_call_id":"call-1","approved":true}]
	}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var resolved approvalRequestResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &resolved); err != nil {
		t.Fatalf("decode approval response: %v", err)
	}
	if resolved.Status != approvalStatusResolved || len(resolved.Decisions) != 1 || resolved.Decisions[0].Approved != approved {
		t.Fatalf("unexpected resolved approval: %#v", resolved)
	}
	if got := getSession(t, handler, created.ID); got.State != SessionStateRunning {
		t.Fatalf("expected state %q, got %q", SessionStateRunning, got.State)
	}

	workerView := getApprovalRequest(t, handler, created.ID, approval.ID)
	if workerView.Status != approvalStatusResolved || len(workerView.Decisions) != 1 {
		t.Fatalf("worker did not observe resolved approval: %#v", workerView)
	}

	items = getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 3 {
		t.Fatalf("expected approval request, turn paused, and decision items, got %d", len(items.Items))
	}
	if items.Items[2].Type != string(zotigosession.DisplayItemApprovalDecision) {
		t.Fatalf("expected approval_decision item, got %s", items.Items[2].Type)
	}
	if items.Items[2].Approval == nil || len(items.Items[2].Approval.Decisions) != 1 || !items.Items[2].Approval.Decisions[0].Approved {
		t.Fatalf("unexpected approval decision item: %#v", items.Items[2].Approval)
	}
}

func TestWorkerAttachDoesNotResumePausedApproval(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	attachWorker(t, handler, created.ID)
	approval := createApprovalRequestForTest(t, handler, created.ID)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/internal/sessions/"+created.ID+"/worker/attach", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d", http.StatusConflict, rec.Code)
	}
	if got := getSession(t, handler, created.ID); got.State != SessionStatePaused {
		t.Fatalf("expected state %q, got %q", SessionStatePaused, got.State)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/approvals/"+approval.ID, strings.NewReader(`{
		"decisions": [{"tool_call_id":"call-1","approved":true}]
	}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
}

func TestApprovalCreateStillPausesWhenTurnPausedItemAppendFails(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	source.appendErr = func(_ string, item zotigosession.DisplayItem) error {
		if item.Type == zotigosession.DisplayItemTurnPaused {
			return errors.New("write failed")
		}
		return nil
	}
	handler := newHandler(newSessionRegistry(), source)
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	attachWorker(t, handler, created.ID)

	approval := createApprovalRequestForTest(t, handler, created.ID)
	if got := getSession(t, handler, created.ID); got.State != SessionStatePaused {
		t.Fatalf("expected state %q, got %q", SessionStatePaused, got.State)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/approvals/"+approval.ID, strings.NewReader(`{
		"decisions": [{"tool_call_id":"call-1","approved":true}]
	}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
}

func TestApprovalDecisionSucceedsWhenRegistryResumeLosesRace(t *testing.T) {
	registry := newSessionRegistry()
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(registry, source)
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	attachWorker(t, handler, created.ID)
	approval := createApprovalRequestForTest(t, handler, created.ID)

	source.appendHook = func(sessionID string, item zotigosession.DisplayItem) {
		if sessionID == created.ID && item.Type == zotigosession.DisplayItemApprovalDecision {
			_, _ = registry.End(created.ID)
		}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/approvals/"+approval.ID, strings.NewReader(`{
		"decisions": [{"tool_call_id":"call-1","approved":true}]
	}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if items.Items[len(items.Items)-1].Type != string(zotigosession.DisplayItemApprovalDecision) {
		t.Fatalf("expected durable approval_decision, got %#v", items.Items[len(items.Items)-1])
	}
}

func TestApprovalDecisionSucceedsWhenSessionEndsBeforePause(t *testing.T) {
	registry := newSessionRegistry()
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(registry, source)
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	attachWorker(t, handler, created.ID)

	source.appendHook = func(sessionID string, item zotigosession.DisplayItem) {
		if sessionID == created.ID && item.Type == zotigosession.DisplayItemApprovalRequest {
			_, _ = registry.End(created.ID)
		}
	}

	approval := createApprovalRequestForTest(t, handler, created.ID)
	if got := getSession(t, handler, created.ID); got.State != SessionStateEnded {
		t.Fatalf("expected state %q, got %q", SessionStateEnded, got.State)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/approvals/"+approval.ID, strings.NewReader(`{
		"decisions": [{"tool_call_id":"call-1","approved":false,"reason":"session ended"}]
	}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	resolved := getApprovalRequest(t, handler, created.ID, approval.ID)
	if resolved.Status != approvalStatusResolved || len(resolved.Decisions) != 1 {
		t.Fatalf("expected resolved approval, got %#v", resolved)
	}
}

func TestApprovalRequestFlowCreatesStoredDisplayLogForDaemonSession(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store})
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	attachWorker(t, handler, created.ID)

	approval := createApprovalRequestForTest(t, handler, created.ID)

	items, ok, err := store.ListDisplayItems(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("list display items: %v", err)
	}
	if !ok {
		t.Fatal("expected daemon session to be materialized in store")
	}
	if len(items) != 2 {
		t.Fatalf("expected approval request and turn paused items, got %d", len(items))
	}
	if items[0].Type != zotigosession.DisplayItemApprovalRequest || items[0].Approval == nil || items[0].Approval.ID != approval.ID {
		t.Fatalf("unexpected stored approval request item: %#v", items[0])
	}
	if items[1].Type != zotigosession.DisplayItemTurnPaused {
		t.Fatalf("expected turn_paused item, got %s", items[1].Type)
	}
}

func TestApprovalDecisionRecoversPendingRequestFromDisplayLog(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store})
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	attachWorker(t, handler, created.ID)
	approval := createApprovalRequestForTest(t, handler, created.ID)

	restarted := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store})
	newSession := createSession(t, restarted)
	if newSession.ID == created.ID {
		t.Fatalf("expected restart-created session id not to reuse %q", created.ID)
	}

	workerView := getApprovalRequest(t, restarted, created.ID, approval.ID)
	if workerView.Status != approvalStatusPending || len(workerView.Pending) != 1 {
		t.Fatalf("expected pending approval after restart, got %#v", workerView)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/approvals/"+approval.ID, strings.NewReader(`{
		"decisions": [{"tool_call_id":"call-1","approved":false,"reason":"not now"}]
	}`))
	restarted.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var resolved approvalRequestResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &resolved); err != nil {
		t.Fatalf("decode approval response: %v", err)
	}
	if resolved.Status != approvalStatusResolved || len(resolved.Decisions) != 1 || resolved.Decisions[0].Approved {
		t.Fatalf("unexpected resolved approval: %#v", resolved)
	}
	if resolved.Decisions[0].Reason != "not now" {
		t.Fatalf("expected denial reason to persist, got %#v", resolved.Decisions[0])
	}

	workerView = getApprovalRequest(t, restarted, created.ID, approval.ID)
	if workerView.Status != approvalStatusResolved || len(workerView.Decisions) != 1 {
		t.Fatalf("expected resolved approval from display log, got %#v", workerView)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/approvals/"+approval.ID, strings.NewReader(`{
		"decisions": [{"tool_call_id":"call-1","approved":true}]
	}`))
	restarted.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d: %s", http.StatusConflict, rec.Code, rec.Body.String())
	}
}

func TestApprovalDecisionRejectsIncompleteOrUnknownDecisions(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	attachWorker(t, handler, created.ID)
	approval := createApprovalRequestForTest(t, handler, created.ID)

	tests := []struct {
		name string
		body string
	}{
		{name: "missing decision", body: `{"decisions":[]}`},
		{name: "unknown tool call", body: `{"decisions":[{"tool_call_id":"call-missing","approved":true}]}`},
		{name: "missing approved", body: `{"decisions":[{"tool_call_id":"call-1"}]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/approvals/"+approval.ID, strings.NewReader(tt.body))
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestApprovalCreateRejectsNonRunningSession(t *testing.T) {
	handler := NewHandler()
	created := createSession(t, handler)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/internal/sessions/"+created.ID+"/approvals", strings.NewReader(`{
		"turn_id":"turn-1",
		"pending":[{"tool_call_id":"call-1","tool_name":"shell"}]
	}`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d", http.StatusConflict, rec.Code)
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
	if err := decodeAPIData(t, rec.Body.Bytes(), &ended); err != nil {
		t.Fatalf("decode finish response: %v", err)
	}
	if ended.State != SessionStateEnded {
		t.Fatalf("expected state %q, got %q", SessionStateEnded, ended.State)
	}
	if ended.EndedAt == nil || ended.EndedAt.IsZero() {
		t.Fatal("expected ended_at")
	}
}

func TestWorkerFinishClosesRegisteredWorker(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	workers := newWorkerRegistry()
	handler := newHandler(newSessionRegistry(), source, handlerOptions{workers: workers})
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	if !workers.Has(created.ID) {
		t.Fatal("expected worker to be registered")
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/internal/sessions/"+created.ID+"/worker/finish", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if workers.Has(created.ID) {
		t.Fatal("expected worker finish to unregister live worker")
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
	if err := decodeAPIData(t, rec.Body.Bytes(), &failed); err != nil {
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

func TestSessionRegistryLifecycleTransitions(t *testing.T) {
	registry := newSessionRegistry()

	created := registry.Add(newSession(""))
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
			session := registry.Add(newSession(""))
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
	if err := decodeAPIData(t, rec.Body.Bytes(), &session); err != nil {
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
	if err := decodeAPIData(t, rec.Body.Bytes(), &session); err != nil {
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

func createApprovalRequestForTest(t *testing.T, handler http.Handler, id string) approvalRequestResponse {
	t.Helper()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/internal/sessions/"+id+"/approvals", strings.NewReader(`{
		"turn_id": "turn-1",
		"pending": [{
			"tool_call_id": "call-1",
			"tool_name": "shell",
			"arguments": "{\"command\":\"touch file\"}",
			"description": "Run shell command",
			"reason": "writes files",
			"risk_level": "medium",
			"source": "classifier",
			"requires_snapshot": true
		}]
	}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	var approval approvalRequestResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &approval); err != nil {
		t.Fatalf("decode approval response: %v", err)
	}
	return approval
}

func getApprovalRequest(t *testing.T, handler http.Handler, sessionID string, approvalID string) approvalRequestResponse {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/internal/sessions/"+sessionID+"/approvals/"+approvalID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var approval approvalRequestResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &approval); err != nil {
		t.Fatalf("decode approval response: %v", err)
	}
	return approval
}

func dialWorker(t *testing.T, server *httptest.Server, sessionID string) *websocket.Conn {
	t.Helper()

	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/internal/workers/connect?session_id=" + sessionID
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial worker websocket: %v", err)
	}
	return conn
}

func readWorkerMessage(t *testing.T, conn *websocket.Conn) workerMessage {
	t.Helper()

	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	var msg workerMessage
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("read worker message: %v", err)
	}
	return msg
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

func getCommands(t *testing.T, handler http.Handler, path string) commandsResponse {
	t.Helper()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var resp commandsResponse
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
