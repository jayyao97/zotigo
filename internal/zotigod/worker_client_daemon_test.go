package zotigod

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/internal/testutil/sessionlogtest"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/providers"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
	"github.com/jayyao97/zotigo/internal/zotigod/workerbase"
)

func TestWorkerClientInitializationFailurePreservesError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	const sessionID = "sess-init-failure"
	store, err := zotigosession.NewFileStore("")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sessionlogtest.PutStoredSession(t, store, sessionID, workDir)
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte("default_profile: ["), 0644); err != nil {
		t.Fatal(err)
	}

	registry := newSessionRegistry()
	registry.Add(Session{ID: sessionID, State: SessionStateStarting, WorkingDirectory: workDir})
	handler := newHandler(registry, sessionlog.StoredItemSource{Store: store}, handlerOptions{
		store:           store,
		workerAuthToken: "worker-secret",
	})
	server := httptest.NewServer(handler)
	defer server.Close()

	runErr := runWorkerClient(context.Background(), workerbase.Config{
		DaemonURL: server.URL,
		SessionID: sessionID,
		AuthToken: "worker-secret",
	})
	if runErr == nil {
		t.Fatal("expected worker initialization failure")
	}
	failed, ok := registry.Get(sessionID)
	if !ok {
		t.Fatal("session missing after worker failure")
	}
	if failed.State != SessionStateFailed {
		t.Fatalf("session state = %q, want %q", failed.State, SessionStateFailed)
	}
	if failed.Error != runErr.Error() {
		t.Fatalf("session error = %q, want %q", failed.Error, runErr.Error())
	}
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
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
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
	display := sessionlog.NewLog("sess-worker-profile", sessionlog.StoredItemSource{Store: store})
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
			Type:    wire.CommandProfile,
			Profile: "new",
		},
	})
	if err != nil {
		t.Fatalf("append profile command: %v", err)
	}
	if err := runtime.HandleCommand(context.Background(), sessionlog.ProfileCommandFromItem(commandItem)); err != nil {
		t.Fatalf("handle profile command: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		stored, err := store.Get(context.Background(), "sess-worker-profile")
		items, _, itemsErr := store.ListDisplayItems(context.Background(), "sess-worker-profile")
		// BeforeApply persists the change before the agent installs it. Wait for
		// both observations instead of racing the asynchronous runtime switch.
		if err == nil && stored != nil && stored.ProfileName == "new" && itemsErr == nil && len(items) == 2 && ag.ActiveProfileName() == "new" {
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
	if cursor := workerbase.RecoverAppliedCommandSequence(items); cursor != commandItem.Sequence {
		t.Fatalf("expected applied profile command cursor %d, got %d", commandItem.Sequence, cursor)
	}
	newerCommand, err := store.AppendDisplayItem(context.Background(), "sess-worker-profile", zotigosession.DisplayItem{
		Type:    zotigosession.DisplayItemSessionCommand,
		Command: &zotigosession.DisplayCommand{Type: wire.CommandProfile, Profile: "newer"},
	})
	if err != nil {
		t.Fatalf("append newer profile command: %v", err)
	}
	if err := runtime.HandleCommand(context.Background(), sessionlog.ProfileCommandFromItem(newerCommand)); err != nil {
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
	handler := newHandler(registry, sessionlog.StoredItemSource{Store: store}, handlerOptions{store: store})
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
