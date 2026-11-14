package zotigod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/providers"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

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
    max_output_tokens: 65536
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
	newTestHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

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
	if len(response.Profiles) != 2 {
		t.Fatalf("expected only global and project profiles, got %#v", response.Profiles)
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
	if projectProfile.MaxOutputTokens != 65536 {
		t.Fatalf("project max_output_tokens = %d, want 65536", projectProfile.MaxOutputTokens)
	}
	if globalProfile == nil || globalProfile.Provider != "anthropic" || globalProfile.Model != "claude-fast" {
		t.Fatalf("expected merged global profile, got %#v", globalProfile)
	}
	if globalProfile.MaxOutputTokens != 120000 {
		t.Fatalf("global max_output_tokens = %d, want effective default 120000", globalProfile.MaxOutputTokens)
	}
	for _, forbidden := range []string{"should-not-leak", "global-secret", "private.example.com", "private_option", "api_key", "base_url"} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Fatalf("response leaked %q: %s", forbidden, rec.Body.String())
		}
	}
}

func TestProfilesReturnsEmptyWhenNoProfilesConfigured(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	projectDir := t.TempDir()

	path := "/config/profiles?working_directory=" + url.QueryEscape(projectDir)
	rec := httptest.NewRecorder()
	newTestHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var response profilesResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode profiles response: %v", err)
	}
	if response.DefaultProfile != "" || len(response.Profiles) != 0 {
		t.Fatalf("expected empty effective profile config, got %#v", response)
	}
}

func TestProfilesRejectsRelativeWorkingDirectory(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/config/profiles?working_directory=relative", nil))

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
	newTestHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

	assertAPIError(t, rec, http.StatusInternalServerError, "internal_error", `default profile "missing" not found`)
}

func TestProfilesReturnsCanonicalDefaultProfileName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	projectDir := t.TempDir()
	projectConfig := "default_profile: deepseek-v4-Flash\nprofiles:\n  deepseek-v4-Flash:\n    provider: deepseek\n    model: deepseek-v4-flash\n"
	if err := os.WriteFile(filepath.Join(projectDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}

	path := "/config/profiles?working_directory=" + url.QueryEscape(projectDir)
	rec := httptest.NewRecorder()
	newTestHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d: %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var response profilesResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode profiles response: %v", err)
	}
	if response.DefaultProfile != "deepseek-v4-flash" {
		t.Fatalf("default profile = %q, want canonical lower-case key", response.DefaultProfile)
	}
}

func TestProfilesRejectsUnsupportedMethod(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/config/profiles", nil))

	assertAPIError(t, rec, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	if got := rec.Header().Get("Allow"); got != http.MethodGet {
		t.Fatalf("expected Allow %q, got %q", http.MethodGet, got)
	}
}

func TestSessionProfileChangeAppliesToOfflineSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
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
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/sessions/sess-offline-profile/profile", strings.NewReader(`{"profile":"NEW"}`)))
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
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
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
	baseStore, err := zotigosession.NewFileStore(newTestStoreRoot(t))
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
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
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
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
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
	baseStore, err := zotigosession.NewFileStore(newTestStoreRoot(t))
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
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/sessions/"+created.ID+"/profile", strings.NewReader(`{"profile":"NEW"}`)))
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
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
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
