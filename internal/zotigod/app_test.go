package zotigod

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	"github.com/jayyao97/zotigo/core/runner"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/core/tools"
	zotigotransport "github.com/jayyao97/zotigo/core/transport"
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

func putStoredSession(t *testing.T, store zotigosession.Store, id string, workDir string) {
	t.Helper()
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:               id,
			WorkingDirectory: workDir,
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

func TestProfilesReturnsMergedRedactedConfig(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	configDir := filepath.Join(homeDir, config.ConfigDirName)
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatalf("create global config directory: %v", err)
	}
	globalConfig := `
profiles:
  global-fast:
    provider: anthropic
    model: claude-fast
    api_key: global-secret
`
	if err := os.WriteFile(filepath.Join(configDir, config.ConfigFileName), []byte(globalConfig), 0644); err != nil {
		t.Fatalf("write global config: %v", err)
	}
	projectDir := t.TempDir()
	projectConfig := `
default_profile: project-high
profiles:
  project-high:
    provider: openai
    model: gpt-5.5
    thinking_level: high
    api_key: should-not-leak
    base_url: https://private.example.com
    params:
      private_option: should-not-leak
`
	if err := os.WriteFile(filepath.Join(projectDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}

	path := "/config/profiles?working_directory=" + url.QueryEscape(projectDir)
	rec := httptest.NewRecorder()
	NewHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var response profilesResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode profiles response: %v", err)
	}
	if response.DefaultProfile != "project-high" {
		t.Fatalf("expected default profile project-high, got %q", response.DefaultProfile)
	}
	for i := 1; i < len(response.Profiles); i++ {
		if response.Profiles[i-1].Name >= response.Profiles[i].Name {
			t.Fatalf("expected profiles sorted by name, got %#v", response.Profiles)
		}
	}
	var projectProfile *publicProfile
	var globalProfile *publicProfile
	for i := range response.Profiles {
		if response.Profiles[i].Name == "project-high" {
			projectProfile = &response.Profiles[i]
		}
		if response.Profiles[i].Name == "global-fast" {
			globalProfile = &response.Profiles[i]
		}
	}
	if projectProfile == nil {
		t.Fatal("expected project profile")
	}
	if projectProfile.Provider != "openai" || projectProfile.Model != "gpt-5.5" || projectProfile.ThinkingLevel != "high" {
		t.Fatalf("unexpected project profile: %#v", projectProfile)
	}
	if globalProfile == nil || globalProfile.Provider != "anthropic" || globalProfile.Model != "claude-fast" {
		t.Fatalf("expected merged global profile, got %#v", globalProfile)
	}
	for _, forbidden := range []string{"should-not-leak", "global-secret", "private.example.com", "private_option", "api_key", "base_url"} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Fatalf("response leaked %q: %s", forbidden, rec.Body.String())
		}
	}
}

func TestProfilesRejectsRelativeWorkingDirectory(t *testing.T) {
	rec := httptest.NewRecorder()
	NewHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config/profiles?working_directory=relative", nil))

	assertAPIError(t, rec, http.StatusBadRequest, "invalid_request", "working_directory must be an absolute path")
}

func TestProfilesRejectsMissingDefaultProfile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, config.ProjectConfig), []byte("default_profile: missing\n"), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}

	path := "/config/profiles?working_directory=" + url.QueryEscape(projectDir)
	rec := httptest.NewRecorder()
	NewHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

	assertAPIError(t, rec, http.StatusInternalServerError, "internal_error", `default profile "missing" not found`)
}

func TestProfilesRejectsUnsupportedMethod(t *testing.T) {
	rec := httptest.NewRecorder()
	NewHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/config/profiles", nil))

	assertAPIError(t, rec, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	if got := rec.Header().Get("Allow"); got != http.MethodGet {
		t.Fatalf("expected Allow %q, got %q", http.MethodGet, got)
	}
}

func TestSessionProfileChangeAppliesToOfflineSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer store.Close()
	workDir := t.TempDir()
	projectConfig := "default_profile: old\nprofiles:\n  old:\n    provider: openai\n    model: old\n  new:\n    provider: openai\n    model: new\n"
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	putStoredSession(t, store, "sess-offline-profile", workDir)
	stored, err := store.Get(context.Background(), "sess-offline-profile")
	if err != nil {
		t.Fatalf("load stored session: %v", err)
	}
	stored.ProfileName = "old"
	if err := store.Put(context.Background(), stored); err != nil {
		t.Fatalf("save old profile: %v", err)
	}
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{store: store})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/sessions/sess-offline-profile/profile", strings.NewReader(`{"profile":"new"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var response changeProfileResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode profile response: %v", err)
	}
	if response.Profile != "new" || response.Status != "applied" {
		t.Fatalf("unexpected profile response: %#v", response)
	}
	stored, err = store.Get(context.Background(), "sess-offline-profile")
	if err != nil || stored.ProfileName != "new" {
		t.Fatalf("expected persisted profile new, got session=%#v err=%v", stored, err)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "sess-offline-profile")
	if err != nil {
		t.Fatalf("load display items: %v", err)
	}
	if len(items) != 1 || items[0].Type != zotigosession.DisplayItemProfileChanged || items[0].Profile == nil || items[0].Profile.From != "old" || items[0].Profile.To != "new" {
		t.Fatalf("unexpected profile display items: %#v", items)
	}
	itemsRec := httptest.NewRecorder()
	handler.ServeHTTP(itemsRec, httptest.NewRequest(http.MethodGet, "/sessions/sess-offline-profile/items", nil))
	if itemsRec.Code != http.StatusOK {
		t.Fatalf("expected items status %d, got %d: %s", http.StatusOK, itemsRec.Code, itemsRec.Body.String())
	}
	var publicItems itemsResponse
	if err := decodeAPIData(t, itemsRec.Body.Bytes(), &publicItems); err != nil {
		t.Fatalf("decode public items: %v", err)
	}
	if len(publicItems.Items) != 1 || publicItems.Items[0].Profile == nil || publicItems.Items[0].Profile.To != "new" {
		t.Fatalf("unexpected public profile item: %#v", publicItems.Items)
	}
}

func TestSessionProfileChangeRejectsOfflineSessionLockedByWorker(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer store.Close()
	workDir := t.TempDir()
	projectConfig := "default_profile: old\nprofiles:\n  old:\n    provider: openai\n    model: old\n  new:\n    provider: openai\n    model: new\n"
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	putStoredSession(t, store, "sess-locked-profile", workDir)
	stored, err := store.Get(context.Background(), "sess-locked-profile")
	if err != nil {
		t.Fatalf("load stored session: %v", err)
	}
	stored.ProfileName = "old"
	if err := store.Put(context.Background(), stored); err != nil {
		t.Fatalf("save old profile: %v", err)
	}
	if err := store.Lock(context.Background(), "sess-locked-profile"); err != nil {
		t.Fatalf("simulate worker lock: %v", err)
	}
	defer func() { _ = store.Unlock(context.Background(), "sess-locked-profile") }()
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{store: store})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/sessions/sess-locked-profile/profile", strings.NewReader(`{"profile":"new"}`)))
	assertAPIError(t, rec, http.StatusConflict, "session_in_use", "active in another process")
	stored, err = store.Get(context.Background(), "sess-locked-profile")
	if err != nil || stored.ProfileName != "old" {
		t.Fatalf("expected locked session profile old, got session=%#v err=%v", stored, err)
	}
}

func TestSessionProfileChangeReportsRollbackFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	baseStore, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer baseStore.Close()
	workDir := t.TempDir()
	projectConfig := "default_profile: old\nprofiles:\n  old:\n    provider: openai\n    model: old\n  new:\n    provider: openai\n    model: new\n"
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	putStoredSession(t, baseStore, "sess-rollback-profile", workDir)
	stored, err := baseStore.Get(context.Background(), "sess-rollback-profile")
	if err != nil {
		t.Fatalf("load stored session: %v", err)
	}
	stored.ProfileName = "old"
	if err := baseStore.Put(context.Background(), stored); err != nil {
		t.Fatalf("save old profile: %v", err)
	}
	store := &rollbackFailingProfileStore{Store: baseStore}
	var markerAttempts atomic.Int32
	source := &fakeDisplayItemSource{
		items: map[string][]zotigosession.DisplayItem{"sess-rollback-profile": {
			{ID: "old-to-new", Sequence: 1, Type: zotigosession.DisplayItemProfileChanged, Profile: &zotigosession.DisplayProfileChange{From: "old", To: "new"}},
			{ID: "new-to-old", Sequence: 2, Type: zotigosession.DisplayItemProfileChanged, Profile: &zotigosession.DisplayProfileChange{From: "new", To: "old"}},
		}},
		appendErr: func(_ string, item zotigosession.DisplayItem) error {
			if item.Type == zotigosession.DisplayItemProfileChanged && markerAttempts.Add(1) == 1 {
				return errors.New("display log unavailable")
			}
			return nil
		},
	}
	handler := newHandler(newSessionRegistry(), source, handlerOptions{store: store})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/sessions/sess-rollback-profile/profile", strings.NewReader(`{"profile":"new"}`)))
	assertAPIError(t, rec, http.StatusInternalServerError, "internal_error", "rollback unavailable")
	if !strings.Contains(rec.Body.String(), "display log unavailable") {
		t.Fatalf("expected original append failure, got %s", rec.Body.String())
	}
	stored, err = baseStore.Get(context.Background(), "sess-rollback-profile")
	if err != nil || stored.ProfileName != "new" {
		t.Fatalf("expected uncertain durable profile new after rollback failure, got session=%#v err=%v", stored, err)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/sessions/sess-rollback-profile/profile", strings.NewReader(`{"profile":"new"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected retry to repair profile marker with status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	items, _, err := source.LoadItems(context.Background(), "sess-rollback-profile")
	if err != nil {
		t.Fatalf("load repaired marker: %v", err)
	}
	if len(items) != 3 || items[2].Type != zotigosession.DisplayItemProfileChanged || items[2].Profile == nil || items[2].Profile.To != "new" {
		t.Fatalf("expected repaired profile marker, got %#v", items)
	}
}

func TestSessionProfileChangeRepairsPendingCommandWhenSelectingDifferentProfile(t *testing.T) {
	const providerName = "offline-profile-repair-provider"
	providers.Register(providerName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer store.Close()
	workDir := t.TempDir()
	projectConfig := fmt.Sprintf("default_profile: old\nprofiles:\n  old:\n    provider: %s\n    model: old\n  profile-a:\n    provider: %s\n    model: a\n  profile-b:\n    provider: %s\n    model: b\n", providerName, providerName, providerName)
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	putStoredSession(t, store, "sess-repair-different-profile", workDir)
	stored, err := store.Get(context.Background(), "sess-repair-different-profile")
	if err != nil {
		t.Fatalf("load stored session: %v", err)
	}
	stored.ProfileName = "profile-a"
	if err := store.Put(context.Background(), stored); err != nil {
		t.Fatalf("save uncertain profile metadata: %v", err)
	}
	commandA := zotigosession.DisplayItem{
		ID:       "profile-command-a",
		Sequence: 1,
		Type:     zotigosession.DisplayItemSessionCommand,
		Command:  &zotigosession.DisplayCommand{Type: sessionCommandProfile, Profile: "profile-a"},
	}
	pause := zotigosession.DisplayItem{
		ID:       "pause-command",
		Sequence: 2,
		Type:     zotigosession.DisplayItemSessionCommand,
		Command:  &zotigosession.DisplayCommand{Type: sessionCommandPause, TurnID: "turn-pending"},
	}
	commandB := zotigosession.DisplayItem{
		ID:       "profile-command-b",
		Sequence: 3,
		Type:     zotigosession.DisplayItemSessionCommand,
		Command:  &zotigosession.DisplayCommand{Type: sessionCommandProfile, Profile: "profile-b"},
	}
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{
		"sess-repair-different-profile": {commandA, pause, commandB},
	}}
	handler := newHandler(newSessionRegistry(), source, handlerOptions{store: store})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/sessions/sess-repair-different-profile/profile", strings.NewReader(`{"profile":"profile-b"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected offline repair status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	stored, err = store.Get(context.Background(), "sess-repair-different-profile")
	if err != nil || stored.ProfileName != "profile-b" {
		t.Fatalf("expected selected profile-b to remain durable, got session=%#v err=%v", stored, err)
	}
	items, _, err := source.LoadItems(context.Background(), "sess-repair-different-profile")
	if err != nil {
		t.Fatalf("load repaired markers: %v", err)
	}
	if len(items) != 6 || items[3].Type != zotigosession.DisplayItemProfileChanged || items[3].Profile == nil || items[3].Profile.To != "profile-b" || items[4].Type != zotigosession.DisplayItemProfileFailed || items[4].Profile == nil || items[4].Profile.CommandID != commandA.ID || items[5].Type != zotigosession.DisplayItemProfileFailed || items[5].Profile == nil || items[5].Profile.CommandID != commandB.ID {
		t.Fatalf("expected offline intent followed by correlated failures, got %#v", items)
	}
	if cursor := recoverAppliedCommandSequence(items); cursor != commandA.Sequence {
		t.Fatalf("expected cursor to stop at ordinary command after %d, got %d", commandA.Sequence, cursor)
	}

	localExec, err := executor.NewLocalExecutor(workDir)
	if err != nil {
		t.Fatalf("create executor: %v", err)
	}
	defer localExec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: providerName, Model: "b"}, localExec, agent.WithProfileName("profile-b"))
	if err != nil {
		t.Fatalf("create recovery agent: %v", err)
	}
	runtime := &workerRuntime{
		sessionID: "sess-repair-different-profile",
		agent:     ag,
		display:   newWorkerDisplayLog("sess-repair-different-profile", source),
	}
	commandServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, commandsResponse{
			Commands:   []commandResponse{profileCommandFromItem(commandA), pauseCommandFromItem(pause), profileCommandFromItem(commandB)},
			NextOffset: 1,
		})
	}))
	defer commandServer.Close()
	cursor, err := replayWorkerCommands(context.Background(), commandServer.Client(), commandServer.URL, runtime.sessionID, runtime, workerCommandCursor{Sequence: commandA.Sequence})
	if err != nil {
		t.Fatalf("replay repaired commands: %v", err)
	}
	if cursor.Sequence != commandB.Sequence || ag.ActiveProfileName() != "profile-b" {
		t.Fatalf("expected replay to skip completed profile commands, got cursor=%#v profile=%q", cursor, ag.ActiveProfileName())
	}
}

func TestSessionProfileChangeRetryCompletesPartialPendingRepair(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer store.Close()
	workDir := t.TempDir()
	projectConfig := "default_profile: profile-a\nprofiles:\n  profile-a:\n    provider: openai\n    model: a\n  profile-b:\n    provider: openai\n    model: b\n  profile-c:\n    provider: openai\n    model: c\n"
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	putStoredSession(t, store, "sess-partial-profile-repair", workDir)
	stored, err := store.Get(context.Background(), "sess-partial-profile-repair")
	if err != nil {
		t.Fatalf("load stored session: %v", err)
	}
	stored.ProfileName = "profile-a"
	if err := store.Put(context.Background(), stored); err != nil {
		t.Fatalf("save uncertain profile metadata: %v", err)
	}
	commandA := zotigosession.DisplayItem{ID: "profile-a", Sequence: 1, Type: zotigosession.DisplayItemSessionCommand, Command: &zotigosession.DisplayCommand{Type: sessionCommandProfile, Profile: "profile-a"}}
	commandC := zotigosession.DisplayItem{ID: "profile-c", Sequence: 2, Type: zotigosession.DisplayItemSessionCommand, Command: &zotigosession.DisplayCommand{Type: sessionCommandProfile, Profile: "profile-c"}}
	var failCOnce atomic.Bool
	source := &fakeDisplayItemSource{
		items: map[string][]zotigosession.DisplayItem{"sess-partial-profile-repair": {commandA, commandC}},
		appendErr: func(_ string, item zotigosession.DisplayItem) error {
			if item.Type == zotigosession.DisplayItemProfileFailed && item.Profile != nil && item.Profile.CommandID == commandC.ID && !failCOnce.Swap(true) {
				return errors.New("profile-c marker unavailable")
			}
			return nil
		},
	}
	handler := newHandler(newSessionRegistry(), source, handlerOptions{store: store})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/sessions/sess-partial-profile-repair/profile", strings.NewReader(`{"profile":"profile-b"}`)))
	assertAPIError(t, rec, http.StatusInternalServerError, "internal_error", "profile state is uncertain")
	stored, err = store.Get(context.Background(), "sess-partial-profile-repair")
	if err != nil || stored.ProfileName != "profile-b" {
		t.Fatalf("expected uncertain metadata to retain profile-b, got session=%#v err=%v", stored, err)
	}
	partialItems, _, err := source.LoadItems(context.Background(), "sess-partial-profile-repair")
	if err != nil {
		t.Fatalf("load partial repair: %v", err)
	}
	if cursor := recoverAppliedCommandSequence(partialItems); cursor != commandC.Sequence {
		t.Fatalf("expected durable offline intent to protect through command %d, got cursor %d", commandC.Sequence, cursor)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/sessions/sess-partial-profile-repair/profile", strings.NewReader(`{"profile":"profile-b"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected retry status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	items, _, err := source.LoadItems(context.Background(), "sess-partial-profile-repair")
	if err != nil {
		t.Fatalf("load repaired markers: %v", err)
	}
	if len(items) != 5 || items[2].Type != zotigosession.DisplayItemProfileChanged || items[2].Profile == nil || items[2].Profile.To != "profile-b" || items[3].Type != zotigosession.DisplayItemProfileFailed || items[3].Profile == nil || items[3].Profile.CommandID != commandA.ID || items[4].Type != zotigosession.DisplayItemProfileFailed || items[4].Profile == nil || items[4].Profile.CommandID != commandC.ID {
		t.Fatalf("expected durable offline intent and both correlated failures, got %#v", items)
	}
	if cursor := recoverAppliedCommandSequence(items); cursor != commandC.Sequence {
		t.Fatalf("expected repaired cursor %d, got %d", commandC.Sequence, cursor)
	}
}

func TestSessionProfileChangeReportsUnlockFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	baseStore, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer baseStore.Close()
	workDir := t.TempDir()
	projectConfig := "default_profile: old\nprofiles:\n  old:\n    provider: openai\n    model: old\n  new:\n    provider: openai\n    model: new\n"
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	putStoredSession(t, baseStore, "sess-unlock-profile", workDir)
	stored, err := baseStore.Get(context.Background(), "sess-unlock-profile")
	if err != nil {
		t.Fatalf("load stored session: %v", err)
	}
	stored.ProfileName = "old"
	if err := baseStore.Put(context.Background(), stored); err != nil {
		t.Fatalf("save old profile: %v", err)
	}
	store := unlockFailingStore{Store: baseStore}
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{store: store})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/sessions/sess-unlock-profile/profile", strings.NewReader(`{"profile":"new"}`)))
	assertAPIError(t, rec, http.StatusInternalServerError, "internal_error", "unlock unavailable")
	if err := baseStore.Unlock(context.Background(), "sess-unlock-profile"); err != nil {
		t.Fatalf("cleanup session lock: %v", err)
	}
}

func TestSessionProfileChangeQueuesCommandForRunningWorker(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	projectConfig := "default_profile: old\nprofiles:\n  old:\n    provider: openai\n    model: old\n  new:\n    provider: openai\n    model: new\n"
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()
	created := createSessionWithWorkingDirectory(t, handler, workDir)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/sessions/"+created.ID+"/profile", strings.NewReader(`{"profile":"new"}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, rec.Code, rec.Body.String())
	}
	var response changeProfileResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode profile response: %v", err)
	}
	if response.Profile != "new" || response.Status != "pending" {
		t.Fatalf("unexpected profile response: %#v", response)
	}
	msg := readWorkerMessage(t, worker)
	if msg.Command == nil || msg.Command.Type != sessionCommandProfile || msg.Command.Profile == nil || msg.Command.Profile.Name != "new" {
		t.Fatalf("unexpected worker profile command: %#v", msg)
	}
	if response.CommandID == "" || response.CommandID != msg.Command.ID {
		t.Fatalf("expected response command id %q, got %q", msg.Command.ID, response.CommandID)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/sessions/"+created.ID+"/profile", strings.NewReader(`{"profile":"old"}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected revert status %d, got %d: %s", http.StatusAccepted, rec.Code, rec.Body.String())
	}
	msg = readWorkerMessage(t, worker)
	if msg.Command == nil || msg.Command.Profile == nil || msg.Command.Profile.Name != "old" {
		t.Fatalf("expected second command to supersede pending profile, got %#v", msg)
	}
}

func TestSessionProfileChangeDoesNotRaceSessionStart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer store.Close()
	workDir := t.TempDir()
	projectConfig := "default_profile: old\nprofiles:\n  old:\n    provider: openai\n    model: old\n  new:\n    provider: openai\n    model: new\n"
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	launchStarted := make(chan struct{})
	releaseLaunch := make(chan struct{})
	launcher := workerLauncherFunc(func(context.Context, string, string) error {
		close(launchStarted)
		<-releaseLaunch
		return nil
	})
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{
		store:                store,
		launcher:             launcher,
		workerConnectTimeout: 10 * time.Millisecond,
	})
	created := createSessionWithWorkingDirectory(t, handler, workDir)

	startDone := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/start", nil))
		startDone <- rec.Code
	}()
	<-launchStarted

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/sessions/"+created.ID+"/profile", strings.NewReader(`{"profile":"new"}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected starting session profile change status %d, got %d: %s", http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	}
	stored, err := store.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("load stored session: %v", err)
	}
	if stored.ProfileName != "old" {
		t.Fatalf("expected start to preserve profile old, got %q", stored.ProfileName)
	}

	close(releaseLaunch)
	if code := <-startDone; code != http.StatusServiceUnavailable {
		t.Fatalf("expected start timeout status %d, got %d", http.StatusServiceUnavailable, code)
	}
}

func TestSessionsCreateAndList(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{store: store})

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

func TestSessionsCreatePersistsSelectedProfile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer store.Close()
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{store: store})
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
	restarted := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{store: store})
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
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer store.Close()
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{store: store})
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
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer store.Close()
	registry := newSessionRegistry()
	handler := newHandler(registry, storedDisplayItemSource{store: store}, handlerOptions{store: store})
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
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d: %s", http.StatusInternalServerError, rec.Code, rec.Body.String())
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
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	workDir := t.TempDir()
	putStoredSession(t, store, "sess-stored", workDir)
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{store: store})

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
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	workDir := t.TempDir()
	putStoredSession(t, store, "sess-stored", workDir)
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{store: store})
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

func TestReadSessionAPIsDoNotLaunchWorker(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	putStoredSession(t, store, "sess-stored", t.TempDir())
	var launches atomic.Int32
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{
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
	workerReady := make(chan *websocket.Conn, 2)
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
	workerReady := make(chan *websocket.Conn, 2)
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

func TestSessionStartResumesStoredSession(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
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
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{
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

func TestDisconnectedRunningSessionRejectsRemovedProfile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer store.Close()
	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte("default_profile: new\nprofiles:\n  new:\n    provider: openai\n    model: new\n"), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID:               "sess-running-removed-profile",
		WorkingDirectory: workDir,
		ProfileName:      "old",
		CreatedAt:        now,
		UpdatedAt:        now,
	}}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	registry := newSessionRegistry()
	registry.Add(Session{
		ID:               "sess-running-removed-profile",
		State:            SessionStateRunning,
		WorkingDirectory: workDir,
		ProfileName:      "old",
		CreatedAt:        now,
	})
	var launches atomic.Int32
	handler := newHandler(registry, storedDisplayItemSource{store: store}, handlerOptions{
		store: store,
		launcher: workerLauncherFunc(func(context.Context, string, string) error {
			launches.Add(1)
			return nil
		}),
		workerConnectTimeout: 10 * time.Millisecond,
	})

	startRec := httptest.NewRecorder()
	handler.ServeHTTP(startRec, httptest.NewRequest(http.MethodPost, "/sessions/sess-running-removed-profile/start", nil))
	assertAPIError(t, startRec, http.StatusConflict, "profile_not_found", `profile "old" not found`)

	messageRec := httptest.NewRecorder()
	handler.ServeHTTP(messageRec, httptest.NewRequest(http.MethodPost, "/sessions/sess-running-removed-profile/messages", strings.NewReader(`{"text":"resume"}`)))
	assertAPIError(t, messageRec, http.StatusConflict, "profile_not_found", `profile "old" not found`)
	if got := launches.Load(); got != 0 {
		t.Fatalf("expected no worker launch, got %d", got)
	}

	profileRec := httptest.NewRecorder()
	handler.ServeHTTP(profileRec, httptest.NewRequest(http.MethodPut, "/sessions/sess-running-removed-profile/profile", strings.NewReader(`{"profile":"new"}`)))
	if profileRec.Code != http.StatusOK {
		t.Fatalf("expected profile repair status %d, got %d: %s", http.StatusOK, profileRec.Code, profileRec.Body.String())
	}
	stored, err := store.Get(context.Background(), "sess-running-removed-profile")
	if err != nil || stored.ProfileName != "new" {
		t.Fatalf("expected repaired durable profile new, got session=%#v err=%v", stored, err)
	}
	if session, ok := registry.Get("sess-running-removed-profile"); !ok || session.ProfileName != "new" {
		t.Fatalf("expected repaired registry profile new, got %#v", session)
	}
}

func TestSessionStartConcurrentResumeLaunchesOneWorker(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
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
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{
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
	store, err := zotigosession.NewFileStore(t.TempDir())
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
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{
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
	store, err := zotigosession.NewFileStore(t.TempDir())
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
	registry := newSessionRegistry()
	handler := newHandler(registry, storedDisplayItemSource{store: store}, handlerOptions{
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
	store, err := zotigosession.NewFileStore(t.TempDir())
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
	handler := newHandler(registry, storedDisplayItemSource{store: store}, handlerOptions{
		store:                store,
		launcher:             launcher,
		workerConnectTimeout: 20 * time.Millisecond,
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
			if !strings.Contains(session.Error, errWorkerConnectTimeout.Error()) {
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
	store, err := zotigosession.NewFileStore(t.TempDir())
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
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{
		store: store, launcher: launcher, workerConnectTimeout: 20 * time.Millisecond,
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/sess-launch-cancel/start", nil))
	assertAPIError(t, rec, http.StatusServiceUnavailable, "service_unavailable", errWorkerConnectTimeout.Error())
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

func TestLoadWorkerCommandCursorDoesNotSkipPendingSteering(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	defer store.Close()
	sessionID := "sess-pending-steering-cursor"
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
		Type: zotigosession.DisplayItemTurnStarted,
		Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
	}); err != nil {
		t.Fatalf("append turn started: %v", err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemSteeringMessage,
		Role: string(protocol.RoleUser),
		Content: []zotigosession.DisplayContentPart{{
			Type: string(protocol.ContentTypeText),
			Text: "still pending",
		}},
		Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
		Command: &zotigosession.DisplayCommand{
			Type:   sessionCommandSteering,
			Text:   "still pending",
			TurnID: "turn-1",
		},
	}); err != nil {
		t.Fatalf("append steering command: %v", err)
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
	if cursor.Sequence != 0 || cursor.Offset != 0 {
		t.Fatalf("expected pending steering to remain replayable, got %#v", cursor)
	}
}

func TestLoadWorkerCommandCursorRecoversAppliedImageOnlySteering(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("new file store: %v", err)
	}
	defer store.Close()
	sessionID := "sess-applied-image-steering-cursor"
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
		Type: zotigosession.DisplayItemTurnStarted,
		Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
	}); err != nil {
		t.Fatalf("append turn started: %v", err)
	}
	item, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemSteeringMessage,
		Role: string(protocol.RoleUser),
		Content: []zotigosession.DisplayContentPart{{
			Type: string(protocol.ContentTypeImage),
			Image: &zotigosession.DisplayMediaPart{
				MediaType: "image/png",
				SizeBytes: 1,
				Width:     1,
				Height:    1,
			},
		}},
		Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
		Command: &zotigosession.DisplayCommand{
			Type: sessionCommandSteering,
			Images: []zotigosession.DisplayCommandImage{{
				MimeType:  "image/png",
				SizeBytes: 1,
				Width:     1,
				Height:    1,
			}},
			TurnID: "turn-1",
		},
	})
	if err != nil {
		t.Fatalf("append steering command: %v", err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnCompleted,
		Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
	}); err != nil {
		t.Fatalf("append turn completed: %v", err)
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
		t.Fatalf("expected applied image steering sequence %d, got %#v", item.Sequence, cursor)
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

func TestResolveWorkerProfile(t *testing.T) {
	appConfig := &config.Config{
		DefaultProfile: "default-profile",
		Profiles: map[string]config.ProfileConfig{
			"default-profile":  {Provider: "openai", Model: "default-model"},
			"selected-profile": {Provider: "anthropic", Model: "selected-model"},
		},
	}

	t.Run("persisted profile", func(t *testing.T) {
		name, profile, err := resolveWorkerProfile(&zotigosession.Session{Metadata: zotigosession.Metadata{ProfileName: "selected-profile"}}, appConfig)
		if err != nil {
			t.Fatalf("resolve selected profile: %v", err)
		}
		if name != "selected-profile" || profile.Model != "selected-model" {
			t.Fatalf("expected selected profile, got name=%q profile=%#v", name, profile)
		}
	})

	t.Run("legacy session uses default", func(t *testing.T) {
		name, profile, err := resolveWorkerProfile(&zotigosession.Session{}, appConfig)
		if err != nil {
			t.Fatalf("resolve default profile: %v", err)
		}
		if name != "default-profile" || profile.Model != "default-model" {
			t.Fatalf("expected default profile, got name=%q profile=%#v", name, profile)
		}
	})
}

func TestWorkerRuntimeAppliesAndPersistsProfileCommand(t *testing.T) {
	const oldProviderName = "worker-profile-old"
	const newProviderName = "worker-profile-new"
	const newerProviderName = "worker-profile-newer"
	providers.Register(oldProviderName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	providers.Register(newProviderName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	providers.Register(newerProviderName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	projectConfig := fmt.Sprintf("default_profile: old\nprofiles:\n  old:\n    provider: %s\n    model: old\n  new:\n    provider: %s\n    model: new\n  newer:\n    provider: %s\n    model: newer\n", oldProviderName, newProviderName, newerProviderName)
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID:               "sess-worker-profile",
		WorkingDirectory: workDir,
		ProfileName:      "old",
		CreatedAt:        now,
		UpdatedAt:        now,
	}}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	localExec, err := executor.NewLocalExecutor(workDir)
	if err != nil {
		t.Fatalf("create executor: %v", err)
	}
	defer localExec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: oldProviderName, Model: "old"}, localExec, agent.WithProfileName("old"))
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	display := newWorkerDisplayLog("sess-worker-profile", storedDisplayItemSource{store: store})
	runtime := &workerRuntime{
		sessionID: "sess-worker-profile",
		workDir:   workDir,
		store:     store,
		agent:     ag,
		display:   display,
	}
	commandItem, err := store.AppendDisplayItem(context.Background(), "sess-worker-profile", zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemSessionCommand,
		Command: &zotigosession.DisplayCommand{
			Type:    sessionCommandProfile,
			Profile: "new",
		},
	})
	if err != nil {
		t.Fatalf("append profile command: %v", err)
	}
	if err := runtime.HandleCommand(context.Background(), profileCommandFromItem(commandItem)); err != nil {
		t.Fatalf("handle profile command: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		stored, err := store.Get(context.Background(), "sess-worker-profile")
		items, _, itemsErr := store.ListDisplayItems(context.Background(), "sess-worker-profile")
		if err == nil && stored != nil && stored.ProfileName == "new" && itemsErr == nil && len(items) == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	stored, err := store.Get(context.Background(), "sess-worker-profile")
	if err != nil || stored.ProfileName != "new" {
		t.Fatalf("expected persisted profile new, got session=%#v err=%v", stored, err)
	}
	if ag.ActiveProfileName() != "new" {
		t.Fatalf("expected active profile new, got %q", ag.ActiveProfileName())
	}
	items, _, err := store.ListDisplayItems(context.Background(), "sess-worker-profile")
	if err != nil {
		t.Fatalf("load display items: %v", err)
	}
	if len(items) != 2 || items[1].Type != zotigosession.DisplayItemProfileChanged || items[1].Profile == nil || items[1].Profile.CommandID != commandItem.ID {
		t.Fatalf("unexpected profile display items: %#v", items)
	}
	if cursor := recoverAppliedCommandSequence(items); cursor != commandItem.Sequence {
		t.Fatalf("expected applied profile command cursor %d, got %d", commandItem.Sequence, cursor)
	}
	newerCommand, err := store.AppendDisplayItem(context.Background(), "sess-worker-profile", zotigosession.DisplayItem{
		Type:    zotigosession.DisplayItemSessionCommand,
		Command: &zotigosession.DisplayCommand{Type: sessionCommandProfile, Profile: "newer"},
	})
	if err != nil {
		t.Fatalf("append newer profile command: %v", err)
	}
	if err := runtime.HandleCommand(context.Background(), profileCommandFromItem(newerCommand)); err != nil {
		t.Fatalf("handle newer profile command: %v", err)
	}
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) && ag.ActiveProfileName() != "newer" {
		time.Sleep(time.Millisecond)
	}
	items, _, err = store.ListDisplayItems(context.Background(), "sess-worker-profile")
	if err != nil {
		t.Fatalf("load consecutive profile items: %v", err)
	}
	if len(items) != 4 || items[3].Profile == nil || items[3].Profile.CommandID != newerCommand.ID || items[3].Profile.From != "new" || items[3].Profile.To != "newer" {
		t.Fatalf("expected consecutive marker new->newer, got %#v", items)
	}
	registry := newSessionRegistry()
	registry.Add(Session{
		ID:               "sess-worker-profile",
		State:            SessionStateRunning,
		WorkingDirectory: workDir,
		ProfileName:      "old",
		CreatedAt:        now,
	})
	handler := newHandler(registry, storedDisplayItemSource{store: store}, handlerOptions{store: store})
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/sessions/sess-worker-profile", nil))
	if getRec.Code != http.StatusOK {
		t.Fatalf("expected live session status %d, got %d: %s", http.StatusOK, getRec.Code, getRec.Body.String())
	}
	var live Session
	if err := decodeAPIData(t, getRec.Body.Bytes(), &live); err != nil {
		t.Fatalf("decode live session: %v", err)
	}
	if live.ProfileName != "newer" {
		t.Fatalf("expected durable profile to override stale registry value, got %#v", live)
	}
	startRec := httptest.NewRecorder()
	handler.ServeHTTP(startRec, httptest.NewRequest(http.MethodPost, "/sessions/sess-worker-profile/start", nil))
	if startRec.Code != http.StatusOK {
		t.Fatalf("expected idempotent start status %d, got %d: %s", http.StatusOK, startRec.Code, startRec.Body.String())
	}
	var started Session
	if err := decodeAPIData(t, startRec.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode idempotent start: %v", err)
	}
	if started.ProfileName != "newer" {
		t.Fatalf("expected idempotent start profile new, got %#v", started)
	}
}

func TestRecoverAppliedCommandSequenceWaitsForProfileResult(t *testing.T) {
	command := zotigosession.DisplayItem{
		ID:       "item-profile",
		Sequence: 1,
		Type:     zotigosession.DisplayItemSessionCommand,
		Command:  &zotigosession.DisplayCommand{Type: sessionCommandProfile, Profile: "new"},
	}
	if cursor := recoverAppliedCommandSequence([]zotigosession.DisplayItem{command}); cursor != 0 {
		t.Fatalf("expected pending profile command cursor 0, got %d", cursor)
	}
	failed := zotigosession.DisplayItem{
		Sequence: 2,
		Type:     zotigosession.DisplayItemProfileFailed,
		Profile:  &zotigosession.DisplayProfileChange{CommandID: command.ID, To: "new"},
	}
	if cursor := recoverAppliedCommandSequence([]zotigosession.DisplayItem{command, failed}); cursor != command.Sequence {
		t.Fatalf("expected failed profile command cursor %d, got %d", command.Sequence, cursor)
	}
}

func TestWorkerRuntimeKeepsProfileWhenPersistenceFails(t *testing.T) {
	const oldProviderName = "worker-profile-failure-old"
	const newProviderName = "worker-profile-failure-new"
	providers.Register(oldProviderName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	providers.Register(newProviderName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	projectConfig := fmt.Sprintf("default_profile: old\nprofiles:\n  old:\n    provider: %s\n    model: old\n  new:\n    provider: %s\n    model: new\n", oldProviderName, newProviderName)
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	localExec, err := executor.NewLocalExecutor(workDir)
	if err != nil {
		t.Fatalf("create executor: %v", err)
	}
	defer localExec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: oldProviderName, Model: "old"}, localExec, agent.WithProfileName("old"))
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	runtime := &workerRuntime{
		sessionID: "sess-profile-failure",
		workDir:   workDir,
		store:     unavailableSessionStore{err: errors.New("store unavailable")},
		agent:     ag,
		display:   newWorkerDisplayLog("sess-profile-failure", source),
	}
	if err := runtime.HandleCommand(context.Background(), commandResponse{
		ID:      "profile-command",
		Type:    sessionCommandProfile,
		Profile: &profileCommandPayload{Name: "new"},
	}); err != nil {
		t.Fatalf("handle profile command: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		items, _, _ := source.LoadItems(context.Background(), "sess-profile-failure")
		if len(items) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	items, _, err := source.LoadItems(context.Background(), "sess-profile-failure")
	if err != nil {
		t.Fatalf("load failure item: %v", err)
	}
	if len(items) != 1 || items[0].Type != zotigosession.DisplayItemProfileFailed || !strings.Contains(items[0].Error, "store unavailable") {
		t.Fatalf("expected persisted profile failure, got %#v", items)
	}
	if got := ag.ActiveProfileName(); got != "old" {
		t.Fatalf("expected active profile old after persistence failure, got %q", got)
	}
}

func TestWorkerRuntimeKeepsProfileWhenProviderBuildFails(t *testing.T) {
	const oldProviderName = "worker-profile-build-failure-old"
	providers.Register(oldProviderName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	projectConfig := fmt.Sprintf("default_profile: old\nprofiles:\n  old:\n    provider: %s\n    model: old\n  broken:\n    provider: provider-that-does-not-exist\n    model: broken\n", oldProviderName)
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID:               "sess-profile-build-failure",
		WorkingDirectory: workDir,
		ProfileName:      "old",
		CreatedAt:        now,
		UpdatedAt:        now,
	}}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	localExec, err := executor.NewLocalExecutor(workDir)
	if err != nil {
		t.Fatalf("create executor: %v", err)
	}
	defer localExec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: oldProviderName, Model: "old"}, localExec, agent.WithProfileName("old"))
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	runtime := &workerRuntime{
		sessionID: "sess-profile-build-failure",
		workDir:   workDir,
		store:     store,
		agent:     ag,
		display:   newWorkerDisplayLog("sess-profile-build-failure", source),
	}
	if err := runtime.HandleCommand(context.Background(), commandResponse{
		ID:      "profile-command",
		Type:    sessionCommandProfile,
		Profile: &profileCommandPayload{Name: "broken"},
	}); err != nil {
		t.Fatalf("handle profile command: %v", err)
	}
	items, _, err := source.LoadItems(context.Background(), "sess-profile-build-failure")
	if err != nil {
		t.Fatalf("load failure item: %v", err)
	}
	if len(items) != 1 || items[0].Type != zotigosession.DisplayItemProfileFailed || !strings.Contains(items[0].Error, "provider-that-does-not-exist") {
		t.Fatalf("expected provider build failure item, got %#v", items)
	}
	stored, err := store.Get(context.Background(), "sess-profile-build-failure")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if stored.ProfileName != "old" || ag.ActiveProfileName() != "old" {
		t.Fatalf("expected old profile after provider build failure, got stored=%q active=%q", stored.ProfileName, ag.ActiveProfileName())
	}
}

func TestWorkerRuntimeTreatsFailureMarkerWriteAsReplayable(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	projectConfig := "default_profile: old\nprofiles:\n  old:\n    provider: openai\n    model: old\n  broken:\n    provider: provider-that-does-not-exist\n    model: broken\n"
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	localExec, err := executor.NewLocalExecutor(workDir)
	if err != nil {
		t.Fatalf("create executor: %v", err)
	}
	defer localExec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: "openai", Model: "old"}, localExec, agent.WithProfileName("old"))
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}, appendErr: func(_ string, item zotigosession.DisplayItem) error {
		if item.Type == zotigosession.DisplayItemProfileFailed {
			return errors.New("display log unavailable")
		}
		return nil
	}}
	runtime := &workerRuntime{sessionID: "sess-marker-failure", workDir: workDir, agent: ag, display: newWorkerDisplayLog("sess-marker-failure", source)}
	if err := runtime.HandleCommand(context.Background(), commandResponse{ID: "profile-command", Type: sessionCommandProfile, Profile: &profileCommandPayload{Name: "broken"}}); err != nil {
		t.Fatalf("handle profile command: %v", err)
	}
	fatalErr := runtime.currentFatalError()
	var uncertain *profileCompletionUncertainError
	if !errors.As(fatalErr, &uncertain) || !isExpectedWorkerClose(fatalErr) {
		t.Fatalf("expected replayable completion uncertainty, got %v", fatalErr)
	}
}

func TestWorkerRuntimeDoesNotCompleteLaterProfileAfterMarkerFailure(t *testing.T) {
	localExec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatalf("create executor: %v", err)
	}
	defer localExec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: "openai", Model: "old"}, localExec, agent.WithProfileName("old"))
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}, appendErr: func(_ string, item zotigosession.DisplayItem) error {
		if item.Profile != nil && item.Profile.CommandID == "profile-a" {
			return errors.New("display log unavailable")
		}
		return nil
	}}
	runtime := &workerRuntime{sessionID: "sess-marker-order", agent: ag, display: newWorkerDisplayLog("sess-marker-order", source)}
	ready := make(chan struct{})
	close(ready)
	orderedA := make(chan struct{})
	resultA := make(chan error, 1)
	resultA <- agent.ErrRuntimeProfileSuperseded
	completionA := make(chan error, 1)
	runtime.finishProfileSwitch(ready, orderedA, "profile-a", "new", resultA, completionA)
	if err := <-completionA; err == nil {
		t.Fatal("expected first marker failure")
	}

	completionB := make(chan error, 1)
	runtime.completeProfileFailure(orderedA, make(chan struct{}), completionB, "profile-b", "newer", errors.New("not applied"))
	if err := <-completionB; err == nil {
		t.Fatal("expected later profile to inherit fatal completion uncertainty")
	}
	items, _, err := source.LoadItems(context.Background(), "sess-marker-order")
	if err != nil {
		t.Fatalf("load items: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("later profile marker crossed failed predecessor: %#v", items)
	}
}

func TestWorkerRuntimeBrokenLatestProfileSupersedesPendingProfile(t *testing.T) {
	const oldProviderName = "worker-profile-supersede-old"
	const newProviderName = "worker-profile-supersede-new"
	oldProvider := &blockingFinishProvider{started: make(chan struct{}), release: make(chan struct{})}
	providers.Register(oldProviderName, func(config.ProfileConfig) (providers.Provider, error) { return oldProvider, nil })
	providers.Register(newProviderName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	projectConfig := fmt.Sprintf("default_profile: old\nprofiles:\n  old:\n    provider: %s\n    model: old\n  new:\n    provider: %s\n    model: new\n  broken:\n    provider: provider-that-does-not-exist\n    model: broken\n", oldProviderName, newProviderName)
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID:               "sess-profile-supersede",
		WorkingDirectory: workDir,
		ProfileName:      "old",
		CreatedAt:        now,
		UpdatedAt:        now,
	}}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	localExec, err := executor.NewLocalExecutor(workDir)
	if err != nil {
		t.Fatalf("create executor: %v", err)
	}
	defer localExec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: oldProviderName, Model: "old"}, localExec, agent.WithProfileName("old"))
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	runtime := &workerRuntime{
		sessionID: "sess-profile-supersede",
		workDir:   workDir,
		store:     store,
		agent:     ag,
		display:   newWorkerDisplayLog("sess-profile-supersede", source),
	}
	events, err := ag.Run(context.Background(), "keep the current generation active")
	if err != nil {
		t.Fatalf("run agent: %v", err)
	}
	<-oldProvider.started
	if err := runtime.HandleCommand(context.Background(), commandResponse{
		ID:      "profile-a",
		Type:    sessionCommandProfile,
		Profile: &profileCommandPayload{Name: "new"},
	}); err != nil {
		t.Fatalf("queue valid profile: %v", err)
	}
	if err := runtime.HandleCommand(context.Background(), commandResponse{
		ID:      "profile-b",
		Type:    sessionCommandProfile,
		Profile: &profileCommandPayload{Name: "broken"},
	}); err != nil {
		t.Fatalf("handle broken latest profile: %v", err)
	}
	close(oldProvider.release)
	for range events {
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		items, _, _ := source.LoadItems(context.Background(), "sess-profile-supersede")
		if len(items) == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	items, _, err := source.LoadItems(context.Background(), "sess-profile-supersede")
	if err != nil {
		t.Fatalf("load profile failures: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected superseded and build failure items, got %#v", items)
	}
	if items[0].Profile == nil || items[0].Profile.CommandID != "profile-a" || items[1].Profile == nil || items[1].Profile.CommandID != "profile-b" {
		t.Fatalf("expected profile completion markers in command order, got %#v", items)
	}
	failures := map[string]string{}
	for _, item := range items {
		if item.Type != zotigosession.DisplayItemProfileFailed || item.Profile == nil {
			t.Fatalf("expected profile failure item, got %#v", item)
		}
		failures[item.Profile.CommandID] = item.Error
	}
	if !strings.Contains(failures["profile-a"], agent.ErrRuntimeProfileSuperseded.Error()) {
		t.Fatalf("expected profile A to be superseded, got %q", failures["profile-a"])
	}
	if !strings.Contains(failures["profile-b"], "provider-that-does-not-exist") {
		t.Fatalf("expected profile B build failure, got %q", failures["profile-b"])
	}
	stored, err := store.Get(context.Background(), "sess-profile-supersede")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if stored.ProfileName != "old" || ag.ActiveProfileName() != "old" {
		t.Fatalf("expected old profile to remain, got stored=%q active=%q", stored.ProfileName, ag.ActiveProfileName())
	}
}

func TestWorkerRuntimeSerializesNewProfileRequestWithDurableCommit(t *testing.T) {
	const oldProviderName = "worker-profile-linear-old"
	const newProviderName = "worker-profile-linear-new"
	providers.Register(oldProviderName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	providers.Register(newProviderName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	projectConfig := fmt.Sprintf("default_profile: old\nprofiles:\n  old:\n    provider: %s\n    model: old\n  new:\n    provider: %s\n    model: new\n  broken:\n    provider: provider-that-does-not-exist\n    model: broken\n", oldProviderName, newProviderName)
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	baseStore, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer baseStore.Close()
	now := time.Now().UTC()
	if err := baseStore.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID:               "sess-profile-linear",
		WorkingDirectory: workDir,
		ProfileName:      "old",
		CreatedAt:        now,
		UpdatedAt:        now,
	}}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	store := &blockingProfileUpdateStore{
		Store:   baseStore,
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	localExec, err := executor.NewLocalExecutor(workDir)
	if err != nil {
		t.Fatalf("create executor: %v", err)
	}
	defer localExec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: oldProviderName, Model: "old"}, localExec, agent.WithProfileName("old"))
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	runtime := &workerRuntime{
		sessionID: "sess-profile-linear",
		workDir:   workDir,
		store:     store,
		agent:     ag,
		display:   newWorkerDisplayLog("sess-profile-linear", storedDisplayItemSource{store: store}),
	}
	if err := runtime.HandleCommand(context.Background(), commandResponse{
		ID:      "profile-a",
		Type:    sessionCommandProfile,
		Profile: &profileCommandPayload{Name: "new"},
	}); err != nil {
		t.Fatalf("handle profile A: %v", err)
	}
	<-store.started

	secondDone := make(chan error, 1)
	go func() {
		secondDone <- runtime.HandleCommand(context.Background(), commandResponse{
			ID:      "profile-b",
			Type:    sessionCommandProfile,
			Profile: &profileCommandPayload{Name: "broken"},
		})
	}()
	select {
	case err := <-secondDone:
		t.Fatalf("profile B crossed the active durable commit: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(store.release)
	if err := <-secondDone; err != nil {
		t.Fatalf("handle profile B: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && ag.ActiveProfileName() != "new" {
		time.Sleep(time.Millisecond)
	}
	stored, err := baseStore.Get(context.Background(), "sess-profile-linear")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if stored.ProfileName != "new" || ag.ActiveProfileName() != "new" {
		t.Fatalf("expected profile A to linearize before B, got stored=%q active=%q", stored.ProfileName, ag.ActiveProfileName())
	}
	items, _, err := baseStore.ListDisplayItems(context.Background(), "sess-profile-linear")
	if err != nil {
		t.Fatalf("load display items: %v", err)
	}
	if len(items) != 2 || items[0].Type != zotigosession.DisplayItemProfileChanged || items[1].Type != zotigosession.DisplayItemProfileFailed {
		t.Fatalf("expected applied A then failed B, got %#v", items)
	}
}

func TestWorkerRuntimeLeavesUncertainProfileCommandForReplay(t *testing.T) {
	const oldProviderName = "worker-profile-uncertain-old"
	const newProviderName = "worker-profile-uncertain-new"
	providers.Register(oldProviderName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	providers.Register(newProviderName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	projectConfig := fmt.Sprintf("default_profile: old\nprofiles:\n  old:\n    provider: %s\n    model: old\n  new:\n    provider: %s\n    model: new\n", oldProviderName, newProviderName)
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	baseStore, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer baseStore.Close()
	now := time.Now().UTC()
	if err := baseStore.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID:               "sess-profile-uncertain",
		WorkingDirectory: workDir,
		ProfileName:      "old",
		CreatedAt:        now,
		UpdatedAt:        now,
	}}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	command := zotigosession.DisplayItem{
		ID:       "profile-command",
		Sequence: 1,
		Type:     zotigosession.DisplayItemSessionCommand,
		Command:  &zotigosession.DisplayCommand{Type: sessionCommandProfile, Profile: "new"},
	}
	messageCommand := zotigosession.DisplayItem{
		ID:       "message-command",
		Sequence: 2,
		Type:     zotigosession.DisplayItemUserMessage,
		Role:     string(protocol.RoleUser),
		Content:  []zotigosession.DisplayContentPart{{Type: string(protocol.ContentTypeText), Text: "must not run"}},
		Command:  &zotigosession.DisplayCommand{Type: sessionCommandMessage, Text: "must not run"},
	}
	var markerAttempts atomic.Int32
	source := &fakeDisplayItemSource{
		items: map[string][]zotigosession.DisplayItem{"sess-profile-uncertain": {command, messageCommand}},
		appendErr: func(_ string, item zotigosession.DisplayItem) error {
			if item.Type == zotigosession.DisplayItemProfileChanged && markerAttempts.Add(1) == 1 {
				return errors.New("display log unavailable")
			}
			return nil
		},
	}
	store := &rollbackFailingProfileStore{Store: baseStore}
	oldExec, err := executor.NewLocalExecutor(workDir)
	if err != nil {
		t.Fatalf("create old executor: %v", err)
	}
	defer oldExec.Close()
	oldAgent, err := agent.New(config.ProfileConfig{Provider: oldProviderName, Model: "old"}, oldExec, agent.WithProfileName("old"))
	if err != nil {
		t.Fatalf("create old agent: %v", err)
	}
	firstRuntime := &workerRuntime{
		sessionID: "sess-profile-uncertain",
		workDir:   workDir,
		store:     store,
		agent:     oldAgent,
		display:   newWorkerDisplayLog("sess-profile-uncertain", source),
		fatalCh:   make(chan error, 1),
	}
	messageResponse, err := messageCommandFromItem(messageCommand, "")
	if err != nil {
		t.Fatalf("build message command: %v", err)
	}
	commandServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		data, marshalErr := sonic.Marshal(commandsResponse{
			Commands:   []commandResponse{profileCommandFromItem(command), messageResponse},
			NextOffset: 2,
		})
		if marshalErr != nil {
			http.Error(w, marshalErr.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	defer commandServer.Close()
	cursor, replayErr := replayWorkerCommands(context.Background(), commandServer.Client(), commandServer.URL, "sess-profile-uncertain", firstRuntime, workerCommandCursor{})
	var uncertain *profileStateUncertainError
	if !errors.As(replayErr, &uncertain) {
		t.Fatalf("expected uncertain replay error, got %v", replayErr)
	}
	if cursor.Sequence != 0 || cursor.Offset != 0 {
		t.Fatalf("expected cursor to remain before uncertain profile, got %#v", cursor)
	}
	select {
	case fatalErr := <-firstRuntime.fatalCh:
		if !errors.As(fatalErr, &uncertain) {
			t.Fatalf("expected uncertain profile error, got %v", fatalErr)
		}
	case <-time.After(time.Second):
		t.Fatal("expected uncertain profile to stop worker runtime")
	}
	items, _, err := source.LoadItems(context.Background(), "sess-profile-uncertain")
	if err != nil {
		t.Fatalf("load pending command: %v", err)
	}
	if len(items) != 2 || recoverAppliedCommandSequence(items) != 0 {
		t.Fatalf("expected uncertain command to remain pending, got %#v", items)
	}
	for _, item := range items {
		if item.Type == zotigosession.DisplayItemTurnStarted {
			t.Fatalf("message after uncertain profile was executed: %#v", items)
		}
	}
	stored, err := baseStore.Get(context.Background(), "sess-profile-uncertain")
	if err != nil || stored.ProfileName != "new" || oldAgent.ActiveProfileName() != "old" {
		t.Fatalf("expected durable/runtime split before replay, stored=%#v active=%q err=%v", stored, oldAgent.ActiveProfileName(), err)
	}

	newExec, err := executor.NewLocalExecutor(workDir)
	if err != nil {
		t.Fatalf("create new executor: %v", err)
	}
	defer newExec.Close()
	newAgent, err := agent.New(config.ProfileConfig{Provider: newProviderName, Model: "new"}, newExec, agent.WithProfileName("new"))
	if err != nil {
		t.Fatalf("create new agent: %v", err)
	}
	secondRuntime := &workerRuntime{
		sessionID: "sess-profile-uncertain",
		workDir:   workDir,
		store:     baseStore,
		agent:     newAgent,
		display:   newWorkerDisplayLog("sess-profile-uncertain", source),
	}
	if err := secondRuntime.HandleCommand(context.Background(), profileCommandFromItem(command)); err != nil {
		t.Fatalf("replay uncertain profile command: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		items, _, _ = source.LoadItems(context.Background(), "sess-profile-uncertain")
		if recoverAppliedCommandSequence(items) == command.Sequence {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if recoverAppliedCommandSequence(items) != command.Sequence {
		t.Fatalf("expected replay to complete uncertain command, got %#v", items)
	}
}

func TestWorkerRuntimeRollsBackProfileWhenChangedItemFails(t *testing.T) {
	const oldProviderName = "worker-profile-append-failure-old"
	const newProviderName = "worker-profile-append-failure-new"
	providers.Register(oldProviderName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	providers.Register(newProviderName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	projectConfig := fmt.Sprintf("default_profile: old\nprofiles:\n  old:\n    provider: %s\n    model: old\n  new:\n    provider: %s\n    model: new\n", oldProviderName, newProviderName)
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID:               "sess-profile-append-failure",
		WorkingDirectory: workDir,
		ProfileName:      "old",
		CreatedAt:        now,
		UpdatedAt:        now,
	}}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	localExec, err := executor.NewLocalExecutor(workDir)
	if err != nil {
		t.Fatalf("create executor: %v", err)
	}
	defer localExec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: oldProviderName, Model: "old"}, localExec, agent.WithProfileName("old"))
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	source := &fakeDisplayItemSource{
		items: map[string][]zotigosession.DisplayItem{},
		appendErr: func(_ string, item zotigosession.DisplayItem) error {
			if item.Type == zotigosession.DisplayItemProfileChanged {
				return errors.New("display log unavailable")
			}
			return nil
		},
	}
	runtime := &workerRuntime{
		sessionID: "sess-profile-append-failure",
		workDir:   workDir,
		store:     store,
		agent:     ag,
		display:   newWorkerDisplayLog("sess-profile-append-failure", source),
	}
	if err := runtime.HandleCommand(context.Background(), commandResponse{
		ID:      "profile-command",
		Type:    sessionCommandProfile,
		Profile: &profileCommandPayload{Name: "new"},
	}); err != nil {
		t.Fatalf("handle profile command: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		items, _, _ := source.LoadItems(context.Background(), "sess-profile-append-failure")
		if len(items) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	items, _, err := source.LoadItems(context.Background(), "sess-profile-append-failure")
	if err != nil {
		t.Fatalf("load failure item: %v", err)
	}
	if len(items) != 1 || items[0].Type != zotigosession.DisplayItemProfileFailed || !strings.Contains(items[0].Error, "display log unavailable") {
		t.Fatalf("expected profile failure item, got %#v", items)
	}
	stored, err := store.Get(context.Background(), "sess-profile-append-failure")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if stored.ProfileName != "old" || !stored.UpdatedAt.Equal(now) {
		t.Fatalf("expected stored profile rollback, got profile=%q updated_at=%s", stored.ProfileName, stored.UpdatedAt)
	}
	if got := ag.ActiveProfileName(); got != "old" {
		t.Fatalf("expected active profile old after display append failure, got %q", got)
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

func TestConcurrentWorkerConnectKeepsFirstAttachedWorker(t *testing.T) {
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
	connections := make(chan *websocket.Conn, 2)
	for range 2 {
		go func() {
			conn, _, _ := websocket.DefaultDialer.Dial(url, nil)
			connections <- conn
		}()
	}
	first := <-connections
	second := <-connections
	defer func() {
		if first != nil {
			first.Close()
		}
		if second != nil {
			second.Close()
		}
	}()
	unlock()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		session, _ := registry.Get(created.ID)
		if session.State == SessionStateRunning && workers.Has(created.ID) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	session, _ := registry.Get(created.ID)
	t.Fatalf("expected running session with one attached worker, got state=%q worker=%v", session.State, workers.Has(created.ID))
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
	if err := unlock(); err != nil {
		t.Fatalf("release first lock: %v", err)
	}

	unlock, err = acquireWorkerSessionLock(context.Background(), store, "sess-lock")
	if err != nil {
		t.Fatalf("expected lock after unlock: %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatalf("release reacquired lock: %v", err)
	}
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

func TestSessionMessageAutoResumesStoredSession(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
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
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{
		store:                store,
		launcher:             launcher,
		workers:              workers,
		workerConnectTimeout: time.Second,
	})
	server = httptest.NewServer(handler)
	defer server.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/sess-stored/messages", strings.NewReader(`{"text":"continue this session"}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	if launchedWorkDir != workDir {
		t.Fatalf("expected launcher workdir %q, got %q", workDir, launchedWorkDir)
	}
	worker := <-workerReady
	if worker == nil {
		t.Fatal("expected worker websocket connection")
	}
	defer worker.Close()
	msg := readWorkerMessage(t, worker)
	if msg.Command == nil || msg.Command.Message == nil || msg.Command.Message.Text != "continue this session" {
		t.Fatalf("unexpected worker command: %#v", msg)
	}

	got := getSession(t, handler, "sess-stored")
	if got.State != SessionStateRunning || !got.Live {
		t.Fatalf("expected resumed running session, got %#v", got)
	}
}

func TestSessionMessageReloadsDisplayLogAfterResume(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	putStoredSession(t, store, "sess-stored", t.TempDir())
	if _, err := store.AppendDisplayItem(context.Background(), "sess-stored", zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnStarted,
		Turn: &zotigosession.DisplayTurn{ID: "turn-stale"},
	}); err != nil {
		t.Fatalf("append stale turn: %v", err)
	}
	var server *httptest.Server
	workers := newWorkerRegistry()
	workerReady := make(chan *websocket.Conn, 1)
	launcher := workerLauncherFunc(func(_ context.Context, sessionID string, _ string) error {
		if _, err := store.AppendDisplayItem(context.Background(), sessionID, zotigosession.DisplayItem{
			Type: zotigosession.DisplayItemTurnInterrupted,
			Turn: &zotigosession.DisplayTurn{
				ID:     "turn-stale",
				Status: "interrupted",
				Reason: workerRestartedReason,
			},
		}); err != nil {
			return err
		}
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
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{
		store:                store,
		launcher:             launcher,
		workers:              workers,
		workerConnectTimeout: time.Second,
	})
	server = httptest.NewServer(handler)
	defer server.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/sess-stored/messages", strings.NewReader(`{"text":"continue after restart"}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}
	worker := <-workerReady
	if worker == nil {
		t.Fatal("expected worker websocket connection")
	}
	defer worker.Close()
	msg := readWorkerMessage(t, worker)
	if msg.Command == nil || msg.Command.Message == nil || msg.Command.Message.Text != "continue after restart" {
		t.Fatalf("unexpected worker command: %#v", msg)
	}

	items := getItems(t, handler, "/sessions/sess-stored/items?limit=10")
	if len(items.Items) != 3 {
		t.Fatalf("expected stale start, interruption, and message, got %#v", items.Items)
	}
	if items.Items[1].Type != string(zotigosession.DisplayItemTurnInterrupted) ||
		items.Items[1].Turn == nil || items.Items[1].Turn.Reason != workerRestartedReason {
		t.Fatalf("expected worker restart interruption, got %#v", items.Items[1])
	}
	if items.Items[2].Type != string(zotigosession.DisplayItemUserMessage) ||
		items.Items[2].Command == nil || items.Items[2].Command.Type != sessionCommandMessage {
		t.Fatalf("expected accepted message after interruption, got %#v", items.Items[2])
	}
}

func TestSessionMessageAcceptsImageInput(t *testing.T) {
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

	var publicCommand publicCommandResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &publicCommand); err != nil {
		t.Fatalf("decode message response: %v", err)
	}
	if len(publicCommand.Images) != 1 {
		t.Fatalf("expected one public image metadata, got %#v", publicCommand.Images)
	}
	if publicCommand.Images[0].URL == "" {
		t.Fatalf("expected public image URL, got %#v", publicCommand.Images[0])
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
	if item.Content[1].Image.URL == "" || item.Command == nil || len(item.Command.Images) != 1 || item.Command.Images[0].URL == "" {
		t.Fatalf("expected item image URLs, got content=%#v command=%#v", item.Content[1].Image, item.Command)
	}
	encodedItems, err := sonic.MarshalString(items)
	if err != nil {
		t.Fatalf("encode items: %v", err)
	}
	if strings.Contains(encodedItems, imageBase64) {
		t.Fatalf("items response leaked image base64")
	}
	if err := os.Remove(filepath.Join(root, "sessions", created.ID+".display.jsonl")); err != nil {
		t.Fatalf("remove display log: %v", err)
	}

	imageRec := httptest.NewRecorder()
	handler.ServeHTTP(imageRec, httptest.NewRequest(http.MethodGet, item.Content[1].Image.URL, nil))
	if imageRec.Code != http.StatusOK {
		t.Fatalf("expected image status %d, got %d: %s", http.StatusOK, imageRec.Code, imageRec.Body.String())
	}
	expectedImage, err := base64.StdEncoding.DecodeString(imageBase64)
	if err != nil {
		t.Fatalf("decode image fixture: %v", err)
	}
	if imageRec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("expected image/png, got %q", imageRec.Header().Get("Content-Type"))
	}
	if !bytes.Equal(imageRec.Body.Bytes(), expectedImage) {
		t.Fatalf("unexpected image bytes")
	}
}

func TestSessionMessageAcceptsImageOnlyInput(t *testing.T) {
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
	body := fmt.Sprintf(`{"images":[{"mime_type":"image/png","data_base64":%q}]}`, imageBase64)
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
	if publicCommand.Text != "" {
		t.Fatalf("expected empty public text, got %q", publicCommand.Text)
	}
	if len(publicCommand.Images) != 1 {
		t.Fatalf("expected one public image metadata, got %#v", publicCommand.Images)
	}

	msg := readWorkerMessage(t, worker)
	if msg.Command == nil || msg.Command.Message == nil {
		t.Fatalf("expected worker message command, got %#v", msg)
	}
	if msg.Command.Message.Text != "" {
		t.Fatalf("expected empty worker text, got %q", msg.Command.Message.Text)
	}
	if len(msg.Command.Message.Images) != 1 || msg.Command.Message.Images[0].DataBase64 == "" {
		t.Fatalf("expected worker image payload, got %#v", msg.Command.Message.Images)
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 1 {
		t.Fatalf("expected one display item, got %#v", items.Items)
	}
	item := items.Items[0]
	if len(item.Content) != 1 || item.Content[0].Text != "" || item.Content[0].Image == nil {
		t.Fatalf("expected image-only display item, got %#v", item.Content)
	}
	if strings.Contains(rec.Body.String(), "data_base64") || strings.Contains(rec.Body.String(), imageBase64) {
		t.Fatalf("public message response leaked image payload: %s", rec.Body.String())
	}
}

func TestSessionImageReadRejectsUnreferencedBlob(t *testing.T) {
	root := t.TempDir()
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{store: store})

	created := createSession(t, handler)
	dir := filepath.Join(root, "sessions", created.ID+".images")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("create image dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "orphan.png"), []byte("orphan"), 0600); err != nil {
		t.Fatalf("write orphan image: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/"+created.ID+"/images/orphan.png", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d: %s", http.StatusNotFound, rec.Code, rec.Body.String())
	}
}

func TestSessionImageReadRejectsNestedImageName(t *testing.T) {
	root := t.TempDir()
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{store: store})

	created := createSession(t, handler)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/"+created.ID+"/images/nested/secret.png", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d: %s", http.StatusNotFound, rec.Code, rec.Body.String())
	}
}

func TestSessionImageReadBackfillsLegacyDisplayLogReference(t *testing.T) {
	root := t.TempDir()
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{store: store})

	created := createSession(t, handler)
	imageData, err := base64.StdEncoding.DecodeString(tinyPNGBase64())
	if err != nil {
		t.Fatalf("decode image fixture: %v", err)
	}
	blobPath := filepath.Join("sessions", created.ID+".images", "legacy.png")
	if err := os.MkdirAll(filepath.Join(root, "sessions", created.ID+".images"), 0700); err != nil {
		t.Fatalf("create image dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, blobPath), imageData, 0600); err != nil {
		t.Fatalf("write legacy image: %v", err)
	}
	if _, err := store.AppendDisplayItem(context.Background(), created.ID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemUserMessage,
		Command: &zotigosession.DisplayCommand{
			Type: sessionCommandMessage,
			Images: []zotigosession.DisplayCommandImage{{
				MimeType:  "image/png",
				SizeBytes: len(imageData),
				Width:     1,
				Height:    1,
				BlobPath:  blobPath,
			}},
		},
	}); err != nil {
		t.Fatalf("append legacy image display item: %v", err)
	}

	imageURL := "/sessions/" + created.ID + "/images/legacy.png"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, imageURL, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), imageData) {
		t.Fatalf("unexpected image bytes")
	}

	if err := os.Remove(filepath.Join(root, "sessions", created.ID+".display.jsonl")); err != nil {
		t.Fatalf("remove display log: %v", err)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, imageURL, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected indexed image status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), imageData) {
		t.Fatalf("unexpected indexed image bytes")
	}
}

func TestSessionMessageRejectsEmptyTextWithoutImages(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{store: store})

	created := createSession(t, handler)
	startSession(t, handler, created.ID)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(`{"text":"   "}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "message requires text or images") {
		t.Fatalf("unexpected response: %s", rec.Body.String())
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
		Images: []commandImageData{{
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
		Images: []commandImageData{{
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

func TestSessionSteeringAcceptsImageOnlyInput(t *testing.T) {
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
	if _, err := store.AppendDisplayItem(context.Background(), created.ID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnStarted,
		Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
	}); err != nil {
		t.Fatalf("append turn started: %v", err)
	}

	imageBase64 := tinyPNGBase64()
	rec := httptest.NewRecorder()
	body := fmt.Sprintf(`{"images":[{"mime_type":"image/png","data_base64":%q}]}`, imageBase64)
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/steering", strings.NewReader(body))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d: %s", http.StatusCreated, rec.Code, rec.Body.String())
	}

	var publicCommand publicCommandResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &publicCommand); err != nil {
		t.Fatalf("decode steering response: %v", err)
	}
	if publicCommand.Text != "" || publicCommand.TurnID != "turn-1" || len(publicCommand.Images) != 1 {
		t.Fatalf("unexpected public steering response: %#v", publicCommand)
	}
	if publicCommand.Images[0].URL == "" {
		t.Fatalf("expected public steering image URL, got %#v", publicCommand.Images[0])
	}
	if strings.Contains(rec.Body.String(), "data_base64") || strings.Contains(rec.Body.String(), imageBase64) {
		t.Fatalf("public steering response leaked image payload: %s", rec.Body.String())
	}

	msg := readWorkerMessage(t, worker)
	if msg.Command == nil || msg.Command.Steering == nil || len(msg.Command.Steering.Images) != 1 {
		t.Fatalf("expected worker steering image command, got %#v", msg)
	}
	if msg.Command.Steering.Images[0].DataBase64 == "" {
		t.Fatalf("worker steering command did not include image data")
	}

	items := getItems(t, handler, "/sessions/"+created.ID+"/items")
	if len(items.Items) != 2 {
		t.Fatalf("expected turn and steering display items, got %#v", items.Items)
	}
	item := items.Items[1]
	if len(item.Content) != 1 || item.Content[0].Image == nil || item.Content[0].Image.URL == "" {
		t.Fatalf("expected image-only steering display item, got %#v", item.Content)
	}
	if item.Command == nil || len(item.Command.Images) != 1 || item.Command.Images[0].URL == "" {
		t.Fatalf("expected steering command image URL, got %#v", item.Command)
	}

	commands := getCommands(t, handler, "/internal/sessions/"+created.ID+"/commands?after=0")
	if len(commands.Commands) != 1 || commands.Commands[0].Steering == nil || len(commands.Commands[0].Steering.Images) != 1 {
		t.Fatalf("expected replayable steering image command, got %#v", commands)
	}
	if commands.Commands[0].Steering.Images[0].DataBase64 == "" {
		t.Fatalf("replayed steering command did not hydrate image payload")
	}

	imageRec := httptest.NewRecorder()
	handler.ServeHTTP(imageRec, httptest.NewRequest(http.MethodGet, item.Content[0].Image.URL, nil))
	if imageRec.Code != http.StatusOK {
		t.Fatalf("expected image status %d, got %d: %s", http.StatusOK, imageRec.Code, imageRec.Body.String())
	}
	expectedImage, err := base64.StdEncoding.DecodeString(imageBase64)
	if err != nil {
		t.Fatalf("decode image fixture: %v", err)
	}
	if !bytes.Equal(imageRec.Body.Bytes(), expectedImage) {
		t.Fatalf("unexpected image bytes")
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

func TestWorkerRuntimeCloseWaitsForTurnPersistence(t *testing.T) {
	done := make(chan struct{})
	runtime := &workerRuntime{
		turnActive: true,
		turnDone:   done,
	}
	closed := make(chan struct{})
	go func() {
		runtime.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("worker runtime closed before active turn persistence completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(done)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("worker runtime did not close after turn persistence completed")
	}
}

func TestWorkerRuntimeCloseWaitsForIdleProfileApply(t *testing.T) {
	const providerName = "worker-close-idle-profile"
	providers.Register(providerName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	exec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatalf("create executor: %v", err)
	}
	defer exec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: providerName}, exec, agent.WithProfileName("old"))
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	applyStarted := make(chan struct{})
	releaseApply := make(chan struct{})
	result := ag.QueueRuntimeProfile(agent.RuntimeProfile{
		Name:     "new",
		Config:   config.ProfileConfig{Provider: providerName},
		Provider: &noopProvider{},
		BeforeApply: func() error {
			close(applyStarted)
			<-releaseApply
			return nil
		},
	})
	<-applyStarted
	runtime := &workerRuntime{agent: ag}
	closed := make(chan struct{})
	go func() {
		runtime.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("worker runtime closed while idle profile apply was active")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseApply)
	if err := <-result; err != nil {
		t.Fatalf("apply runtime profile: %v", err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("worker runtime did not close after idle profile apply")
	}
}

func TestWorkerRuntimeCloseWaitsForAgentStreamExit(t *testing.T) {
	const providerName = "worker-close-delayed-stream"
	provider := &blockingFinishProvider{started: make(chan struct{}), release: make(chan struct{})}
	providers.Register(providerName, func(config.ProfileConfig) (providers.Provider, error) { return provider, nil })
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID:        "sess-close-stream",
		CreatedAt: now,
		UpdatedAt: now,
	}}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	exec, err := executor.NewLocalExecutor(t.TempDir())
	if err != nil {
		t.Fatalf("create executor: %v", err)
	}
	defer exec.Close()
	ag, err := agent.New(config.ProfileConfig{Provider: providerName}, exec)
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	transport := zotigotransport.NewChannelTransport(10)
	defer transport.Close()
	runtime := &workerRuntime{
		sessionID: "sess-close-stream",
		store:     store,
		agent:     ag,
		display:   newWorkerDisplayLog("sess-close-stream", source),
	}
	runtime.runner = runner.New(ag, transport)
	if err := runtime.startMessageTurn(context.Background(), "message-command", &messageCommandPayload{Text: "wait"}); err != nil {
		t.Fatalf("start message turn: %v", err)
	}
	<-provider.started
	closed := make(chan struct{})
	go func() {
		runtime.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("worker runtime closed before Agent stream exited")
	case <-time.After(20 * time.Millisecond):
	}
	close(provider.release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("worker runtime did not close after Agent stream exited")
	}
	stored, err := store.Get(context.Background(), "sess-close-stream")
	if err != nil {
		t.Fatalf("load final snapshot: %v", err)
	}
	if stored.AgentSnapshot.State == agent.StateRunning {
		t.Fatalf("expected final non-running snapshot, got %q", stored.AgentSnapshot.State)
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

func TestTurnScopedControlsRejectStoredOfflineSession(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	putStoredSession(t, store, "sess-stored", t.TempDir())
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{store: store})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/sess-stored/pause", nil))
	assertAPIError(t, rec, http.StatusConflict, "session_not_live", "pause requires a live session")

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/sess-stored/steering", strings.NewReader(`{"text":"adjust"}`))
	handler.ServeHTTP(rec, req)
	assertAPIError(t, rec, http.StatusConflict, "session_not_live", "steering requires a live session")
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

func TestApprovalDecisionRejectsOfflineSession(t *testing.T) {
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
	assertAPIError(t, rec, http.StatusConflict, "session_not_live", "approval decision requires a live session")
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
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create session store: %v", err)
	}
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{store: store})
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
