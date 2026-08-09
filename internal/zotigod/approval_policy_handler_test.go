package zotigod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/providers"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

type failingApprovalPolicyStore struct {
	zotigosession.Store
	onUpdate func()
}

func (s failingApprovalPolicyStore) UpdateApprovalPolicy(context.Context, string, agent.ApprovalPolicy, time.Time) error {
	if s.onUpdate != nil {
		s.onUpdate()
	}
	return errors.New("approval policy persistence unavailable")
}

func TestSessionsCreatePersistsApprovalPolicy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{store: store})
	workDir := t.TempDir()
	writeTestProfileConfig(t, workDir)

	create := func(policy string) Session {
		t.Helper()
		body := fmt.Sprintf(`{"working_directory":%q%s}`, workDir, policy)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(body)))
		if rec.Code != http.StatusCreated {
			t.Fatalf("create status = %d: %s", rec.Code, rec.Body.String())
		}
		var created Session
		if err := decodeAPIData(t, rec.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode create response: %v", err)
		}
		return created
	}

	automatic := create("")
	if automatic.ApprovalPolicy != agent.ApprovalPolicyAuto {
		t.Fatalf("default approval policy = %q, want auto", automatic.ApprovalPolicy)
	}
	bypass := create(`,"approval_policy":"bypass_permissions"`)
	if bypass.ApprovalPolicy != agent.ApprovalPolicyBypass {
		t.Fatalf("approval policy = %q, want bypass", bypass.ApprovalPolicy)
	}
	stored, err := store.Get(context.Background(), bypass.ID)
	if err != nil {
		t.Fatalf("load stored session: %v", err)
	}
	if stored.ApprovalPolicy != agent.ApprovalPolicyBypass {
		t.Fatalf("stored approval policy = %q, want bypass", stored.ApprovalPolicy)
	}

	restarted := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{store: store})
	rec := httptest.NewRecorder()
	restarted.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions/"+bypass.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d: %s", rec.Code, rec.Body.String())
	}
	var restored Session
	if err := decodeAPIData(t, rec.Body.Bytes(), &restored); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if restored.ApprovalPolicy != agent.ApprovalPolicyBypass {
		t.Fatalf("restored approval policy = %q, want bypass", restored.ApprovalPolicy)
	}
}

func TestSessionsCreateRejectsUnsupportedApprovalPolicy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	writeTestProfileConfig(t, workDir)
	handler := NewHandler()
	for _, policy := range []string{"manual", "unknown"} {
		rec := httptest.NewRecorder()
		body := fmt.Sprintf(`{"working_directory":%q,"approval_policy":%q}`, workDir, policy)
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(body)))
		assertAPIError(t, rec, http.StatusBadRequest, "invalid_request", "approval_policy must be")
	}
}

func TestSessionApprovalPolicyChangeAppliesOfflineAndRejectsBusySession(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()
	workDir := t.TempDir()
	putStoredSession(t, store, "sess-policy", workDir)
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source, handlerOptions{store: store})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/sessions/sess-policy/approval-policy", strings.NewReader(`{"approval_policy":"bypass_permissions"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("change status = %d: %s", rec.Code, rec.Body.String())
	}
	stored, err := store.Get(context.Background(), "sess-policy")
	if err != nil || stored.ApprovalPolicy != agent.ApprovalPolicyBypass {
		t.Fatalf("stored policy = %#v, err = %v", stored, err)
	}

	source.items["sess-policy"] = []zotigosession.DisplayItem{{
		Type: zotigosession.DisplayItemTurnStarted,
		Turn: &zotigosession.DisplayTurn{ID: "turn-active"},
	}}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/sessions/sess-policy/approval-policy", strings.NewReader(`{"approval_policy":"auto"}`)))
	assertAPIError(t, rec, http.StatusConflict, "conflict", "requires an idle session")
}

func TestSessionApprovalPolicyChangeQueuesRunningWorkerCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()
	created := createSessionWithWorkingDirectory(t, handler, workDir)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/sessions/"+created.ID+"/approval-policy", strings.NewReader(`{"approval_policy":"bypass_permissions"}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("change status = %d: %s", rec.Code, rec.Body.String())
	}
	msg := readWorkerMessage(t, worker)
	if msg.Command == nil || msg.Command.Type != sessionCommandApprovalPolicy || msg.Command.ApprovalPolicy == nil || msg.Command.ApprovalPolicy.Policy != agent.ApprovalPolicyBypass {
		t.Fatalf("unexpected worker command: %#v", msg)
	}
	var first changeApprovalPolicyResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode first response: %v", err)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/sessions/"+created.ID+"/approval-policy", strings.NewReader(`{"approval_policy":"bypass_permissions"}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("repeat status = %d: %s", rec.Code, rec.Body.String())
	}
	var repeated changeApprovalPolicyResponse
	if err := decodeAPIData(t, rec.Body.Bytes(), &repeated); err != nil {
		t.Fatalf("decode repeated response: %v", err)
	}
	if repeated.CommandID != first.CommandID {
		t.Fatalf("repeat command id = %q, want existing %q", repeated.CommandID, first.CommandID)
	}
	items, _, err := source.LoadItems(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("load commands after repeat: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("repeat request appended another command: %#v", items)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/sessions/"+created.ID+"/approval-policy", strings.NewReader(`{"approval_policy":"auto"}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("opposite status = %d: %s", rec.Code, rec.Body.String())
	}
	msg = readWorkerMessage(t, worker)
	if msg.Command == nil || msg.Command.ApprovalPolicy == nil || msg.Command.ApprovalPolicy.Policy != agent.ApprovalPolicyAuto {
		t.Fatalf("unexpected opposite worker command: %#v", msg)
	}
	items, _, err = source.LoadItems(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("load commands after opposite request: %v", err)
	}
	commands, err := buildCommandsResponse(items, commandQuery{Limit: maxCommandsLimit}, "")
	if err != nil {
		t.Fatalf("build durable commands: %v", err)
	}
	if len(commands.Commands) != 2 || commands.Commands[0].ApprovalPolicy == nil || commands.Commands[1].ApprovalPolicy == nil || commands.Commands[0].ApprovalPolicy.Policy != agent.ApprovalPolicyBypass || commands.Commands[1].ApprovalPolicy.Policy != agent.ApprovalPolicyAuto {
		t.Fatalf("unexpected durable policy order: %#v", commands.Commands)
	}
}

func TestSessionApprovalPolicyChangeWaitsForStartingWorkerWithoutHoldingSessionLock(t *testing.T) {
	registry := newSessionRegistry()
	registry.Add(Session{
		ID:             "sess-starting-policy",
		State:          SessionStateStarting,
		ApprovalPolicy: agent.ApprovalPolicyAuto,
	})
	workers := newWorkerRegistry()
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(registry, source, handlerOptions{
		workers:              workers,
		workerConnectTimeout: time.Second,
	})
	server := httptest.NewServer(handler)
	defer server.Close()

	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/sessions/sess-starting-policy/approval-policy", strings.NewReader(`{"approval_policy":"bypass_permissions"}`)))
		response <- rec
	}()

	deadline := time.Now().Add(time.Second)
	for {
		workers.mu.Lock()
		waiting := len(workers.waiters["sess-starting-policy"]) > 0
		workers.mu.Unlock()
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("approval policy request did not begin waiting for worker")
		}
		time.Sleep(time.Millisecond)
	}

	worker := dialWorker(t, server, "sess-starting-policy")
	defer worker.Close()
	select {
	case rec := <-response:
		if rec.Code != http.StatusAccepted {
			t.Fatalf("change status = %d: %s", rec.Code, rec.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("approval policy request remained blocked after worker connected")
	}
	msg := readWorkerMessage(t, worker)
	if msg.Command == nil || msg.Command.ApprovalPolicy == nil || msg.Command.ApprovalPolicy.Policy != agent.ApprovalPolicyBypass {
		t.Fatalf("unexpected worker command: %#v", msg)
	}
}

func TestWorkerRuntimeRestoresAndPersistsApprovalPolicy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const providerName = "approval-policy-runtime-test"
	providers.Register(providerName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	workDir := t.TempDir()
	projectConfig := fmt.Sprintf("default_profile: test\nprofiles:\n  test:\n    provider: %s\n    model: test\n", providerName)
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:               "sess-runtime-policy",
			WorkingDirectory: workDir,
			ProfileName:      "test",
			ApprovalPolicy:   agent.ApprovalPolicyBypass,
			CreatedAt:        now,
			UpdatedAt:        now,
		},
		AgentSnapshot: agent.Snapshot{State: agent.StateIdle, CreatedAt: now},
	}); err != nil {
		t.Fatalf("put session: %v", err)
	}

	runtime, err := newWorkerRuntime(context.Background(), workerRuntimeConfig{SessionID: "sess-runtime-policy", Store: store})
	if err != nil {
		t.Fatalf("create worker runtime: %v", err)
	}
	defer runtime.Close()
	if got := runtime.agent.Describe().ApprovalPolicy; got != agent.ApprovalPolicyBypass {
		t.Fatalf("restored policy = %q, want bypass", got)
	}
	commandItem, err := store.AppendDisplayItem(context.Background(), "sess-runtime-policy", zotigosession.DisplayItem{
		Type:    zotigosession.DisplayItemSessionCommand,
		Command: &zotigosession.DisplayCommand{Type: sessionCommandApprovalPolicy, ApprovalPolicy: string(agent.ApprovalPolicyAuto)},
	})
	if err != nil {
		t.Fatalf("append policy command: %v", err)
	}
	command := commandResponse{
		ID:             commandItem.ID,
		Sequence:       commandItem.Sequence,
		Type:           sessionCommandApprovalPolicy,
		ApprovalPolicy: &approvalPolicyCommandPayload{Policy: agent.ApprovalPolicyAuto},
	}
	oppositeItem, err := store.AppendDisplayItem(context.Background(), "sess-runtime-policy", zotigosession.DisplayItem{
		Type:    zotigosession.DisplayItemSessionCommand,
		Command: &zotigosession.DisplayCommand{Type: sessionCommandApprovalPolicy, ApprovalPolicy: string(agent.ApprovalPolicyBypass)},
	})
	if err != nil {
		t.Fatalf("append opposite policy command: %v", err)
	}
	oppositeCommand := commandResponse{
		ID:             oppositeItem.ID,
		Sequence:       oppositeItem.Sequence,
		Type:           sessionCommandApprovalPolicy,
		ApprovalPolicy: &approvalPolicyCommandPayload{Policy: agent.ApprovalPolicyBypass},
	}
	commandServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, commandsResponse{
			Commands:   []commandResponse{command, oppositeCommand},
			NextCursor: fmt.Sprintf("%d", oppositeCommand.Sequence),
			NextOffset: 1,
		})
	}))
	defer commandServer.Close()

	storedItems := runtime.display.items
	runtime.display.items = &fakeDisplayItemSource{
		items: map[string][]zotigosession.DisplayItem{"sess-runtime-policy": {commandItem, oppositeItem}},
		appendErr: func(_ string, item zotigosession.DisplayItem) error {
			if item.Type == zotigosession.DisplayItemApprovalPolicyChanged {
				return fmt.Errorf("completion marker unavailable")
			}
			return nil
		},
	}
	cursor, replayErr := replayWorkerCommands(context.Background(), commandServer.Client(), commandServer.URL, runtime.sessionID, runtime, workerCommandCursor{})
	if replayErr == nil || !strings.Contains(replayErr.Error(), "completion marker unavailable") {
		t.Fatalf("expected completion marker failure, got %v", replayErr)
	}
	if cursor != (workerCommandCursor{}) {
		t.Fatalf("cursor advanced before completion marker: %#v", cursor)
	}
	if got := runtime.agent.Describe().ApprovalPolicy; got != agent.ApprovalPolicyAuto {
		t.Fatalf("runtime policy = %q, want auto", got)
	}
	stored, err := store.Get(context.Background(), "sess-runtime-policy")
	if err != nil || stored.ApprovalPolicy != agent.ApprovalPolicyAuto {
		t.Fatalf("stored policy = %#v, err = %v", stored, err)
	}

	runtime.display.items = storedItems
	cursor, err = replayWorkerCommands(context.Background(), commandServer.Client(), commandServer.URL, runtime.sessionID, runtime, workerCommandCursor{})
	if err != nil {
		t.Fatalf("replay pending policy command: %v", err)
	}
	if cursor.Sequence != oppositeCommand.Sequence || cursor.Offset != 1 {
		t.Fatalf("unexpected replay cursor: %#v", cursor)
	}
	savedCursor, err := loadWorkerCommandCursor(context.Background(), nil, runtime.sessionID)
	if err != nil {
		t.Fatalf("load saved cursor: %v", err)
	}
	if savedCursor != cursor {
		t.Fatalf("saved cursor = %#v, want %#v", savedCursor, cursor)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "sess-runtime-policy")
	if err != nil {
		t.Fatalf("load display items: %v", err)
	}
	if runtime.agent.Describe().ApprovalPolicy != agent.ApprovalPolicyBypass {
		t.Fatalf("final runtime policy = %q, want bypass", runtime.agent.Describe().ApprovalPolicy)
	}
	stored, err = store.Get(context.Background(), "sess-runtime-policy")
	if err != nil || stored.ApprovalPolicy != agent.ApprovalPolicyBypass {
		t.Fatalf("final stored policy = %#v, err = %v", stored, err)
	}
	if got := recoverAppliedCommandSequence(items); got != oppositeCommand.Sequence {
		t.Fatalf("recovered cursor = %d, want %d", got, oppositeCommand.Sequence)
	}
	if len(items) != 4 || items[2].Type != zotigosession.DisplayItemApprovalPolicyChanged || items[3].Type != zotigosession.DisplayItemApprovalPolicyChanged {
		t.Fatalf("expected one completion marker per command, got %#v", items)
	}

	staleCursor, err := replayWorkerCommands(context.Background(), commandServer.Client(), commandServer.URL, runtime.sessionID, runtime, workerCommandCursor{})
	if err != nil {
		t.Fatalf("replay stale cursor: %v", err)
	}
	if staleCursor.Sequence != oppositeCommand.Sequence {
		t.Fatalf("stale replay cursor = %#v", staleCursor)
	}
	items, _, err = store.ListDisplayItems(context.Background(), "sess-runtime-policy")
	if err != nil {
		t.Fatalf("load items after stale replay: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("stale replay duplicated completion marker: %#v", items)
	}

}

func TestWorkerRuntimeApprovalPolicyPersistenceFailureKeepsSaferPolicy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const providerName = "approval-policy-persistence-failure-test"
	providers.Register(providerName, func(config.ProfileConfig) (providers.Provider, error) { return &noopProvider{}, nil })
	workDir := t.TempDir()
	projectConfig := fmt.Sprintf("default_profile: test\nprofiles:\n  test:\n    provider: %s\n    model: test\n", providerName)
	if err := os.WriteFile(filepath.Join(workDir, config.ProjectConfig), []byte(projectConfig), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	tests := []struct {
		name    string
		initial agent.ApprovalPolicy
		target  agent.ApprovalPolicy
	}{
		{name: "upgrade", initial: agent.ApprovalPolicyAuto, target: agent.ApprovalPolicyBypass},
		{name: "downgrade", initial: agent.ApprovalPolicyBypass, target: agent.ApprovalPolicyAuto},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, err := zotigosession.NewFileStore(t.TempDir())
			if err != nil {
				t.Fatalf("create store: %v", err)
			}
			defer store.Close()
			now := time.Now().UTC()
			sessionID := "policy-persistence-failure-" + tt.name
			if err := store.Put(context.Background(), &zotigosession.Session{
				Metadata: zotigosession.Metadata{
					ID:               sessionID,
					WorkingDirectory: workDir,
					ProfileName:      "test",
					ApprovalPolicy:   tt.initial,
					CreatedAt:        now,
					UpdatedAt:        now,
				},
				AgentSnapshot: agent.Snapshot{State: agent.StateIdle, CreatedAt: now},
			}); err != nil {
				t.Fatalf("put session: %v", err)
			}
			failingStore := failingApprovalPolicyStore{Store: store}
			runtime, err := newWorkerRuntime(context.Background(), workerRuntimeConfig{
				SessionID: sessionID,
				Store:     &failingStore,
			})
			if err != nil {
				t.Fatalf("create worker runtime: %v", err)
			}
			defer runtime.Close()
			failingStore.onUpdate = func() {
				if got := runtime.agent.Describe().ApprovalPolicy; got != agent.ApprovalPolicyAuto {
					t.Errorf("runtime policy at persistence = %q, want safer auto", got)
				}
			}

			err = runtime.setApprovalPolicy(context.Background(), "command-1", &approvalPolicyCommandPayload{Policy: tt.target})
			if err == nil || !strings.Contains(err.Error(), "approval policy persistence unavailable") {
				t.Fatalf("set policy error = %v", err)
			}
			if got := runtime.agent.Describe().ApprovalPolicy; got != agent.ApprovalPolicyAuto {
				t.Fatalf("runtime policy = %q, want safer auto", got)
			}
			stored, err := store.Get(context.Background(), sessionID)
			if err != nil {
				t.Fatalf("load stored session: %v", err)
			}
			if stored.ApprovalPolicy != tt.initial {
				t.Fatalf("stored policy = %q, want unchanged %q", stored.ApprovalPolicy, tt.initial)
			}
		})
	}
}
