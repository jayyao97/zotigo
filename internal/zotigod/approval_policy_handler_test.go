package zotigod

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/internal/testutil/sessionlogtest"

	"github.com/jayyao97/zotigo/core/agent"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
	"github.com/jayyao97/zotigo/internal/zotigod/workerbase"
)

func TestSessionsCreatePersistsApprovalPolicy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()
	handler := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{store: store})
	workDir := t.TempDir()
	sessionlogtest.WriteProfileConfig(t, workDir)

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

	restarted := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{store: store})
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
	sessionlogtest.WriteProfileConfig(t, workDir)
	handler := newTestHandler()
	for _, policy := range []string{"never", "unknown"} {
		rec := httptest.NewRecorder()
		body := fmt.Sprintf(`{"working_directory":%q,"approval_policy":%q}`, workDir, policy)
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(body)))
		assertAPIError(t, rec, http.StatusBadRequest, "invalid_request", "approval_policy must be")
	}
}

func TestSessionApprovalPolicyChangeAppliesOfflineAndRejectsBusySession(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()
	workDir := t.TempDir()
	sessionlogtest.PutStoredSession(t, store, "sess-policy", workDir)
	source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}
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

	source.Items["sess-policy"] = []zotigosession.DisplayItem{{
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
	source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}
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
	if msg.Command == nil || msg.Command.Type != wire.CommandApprovalPolicy || msg.Command.ApprovalPolicy == nil || msg.Command.ApprovalPolicy.Policy != agent.ApprovalPolicyBypass {
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
	commands, err := buildCommandsResponse(items, commandQuery{Limit: wire.MaxCommandsLimit}, "")
	if err != nil {
		t.Fatalf("build durable commands: %v", err)
	}
	if len(commands.Commands) != 2 || commands.Commands[0].ApprovalPolicy == nil || commands.Commands[1].ApprovalPolicy == nil || commands.Commands[0].ApprovalPolicy.Policy != agent.ApprovalPolicyBypass || commands.Commands[1].ApprovalPolicy.Policy != agent.ApprovalPolicyAuto {
		t.Fatalf("unexpected durable policy order: %#v", commands.Commands)
	}
}

func TestSessionApprovalPolicyChangeAppliesToOfflineCodexSession(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	defer store.Close()
	workDir := t.TempDir()
	sessionlogtest.PutStoredSession(t, store, "sess-codex-policy", workDir)
	stored, err := store.Get(context.Background(), "sess-codex-policy")
	if err != nil {
		t.Fatal(err)
	}
	stored.Agent = string(zotigoruntime.AgentCodex)
	stored.ApprovalPolicy = agent.ApprovalPolicyAuto
	if err := store.Put(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	handler := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{store: store})

	for _, policy := range []agent.ApprovalPolicy{agent.ApprovalPolicyBypass, agent.ApprovalPolicyAuto} {
		rec := httptest.NewRecorder()
		body := fmt.Sprintf(`{"approval_policy":%q}`, policy)
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/sessions/sess-codex-policy/approval-policy", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("change to %q status = %d: %s", policy, rec.Code, rec.Body.String())
		}
		stored, err = store.Get(context.Background(), "sess-codex-policy")
		if err != nil || stored.ApprovalPolicy != policy {
			t.Fatalf("stored Codex policy = %#v, err=%v, want %q", stored, err, policy)
		}
	}
}

func TestSessionApprovalPolicyChangeQueuesRunningCodexWorkerCommands(t *testing.T) {
	source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}
	registry := newSessionRegistry()
	registry.Add(Session{
		ID: "sess-running-codex-policy", State: SessionStateRunning, Live: true,
		Agent: string(zotigoruntime.AgentCodex), ApprovalPolicy: agent.ApprovalPolicyAuto,
	})
	handler := newHandler(registry, source)
	server := httptest.NewServer(handler)
	defer server.Close()
	worker, _ := connectWorkerForActivation(t, server, "sess-running-codex-policy", "0")
	defer worker.Close()

	for _, policy := range []agent.ApprovalPolicy{agent.ApprovalPolicyBypass, agent.ApprovalPolicyAuto} {
		rec := httptest.NewRecorder()
		body := fmt.Sprintf(`{"approval_policy":%q}`, policy)
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/sessions/sess-running-codex-policy/approval-policy", strings.NewReader(body)))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("change to %q status = %d: %s", policy, rec.Code, rec.Body.String())
		}
		msg := readWorkerMessage(t, worker)
		if msg.Command == nil || msg.Command.Type != wire.CommandApprovalPolicy || msg.Command.ApprovalPolicy == nil || msg.Command.ApprovalPolicy.Policy != policy {
			t.Fatalf("unexpected Codex worker command for %q: %#v", policy, msg)
		}
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
	source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}
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

func TestLowersPermissionsOrdersPolicies(t *testing.T) {
	cases := []struct {
		from, to agent.ApprovalPolicy
		want     bool
	}{
		{agent.ApprovalPolicyBypass, agent.ApprovalPolicyAuto, true},
		{agent.ApprovalPolicyBypass, agent.ApprovalPolicyManual, true},
		{agent.ApprovalPolicyAuto, agent.ApprovalPolicyManual, true},
		{agent.ApprovalPolicyManual, agent.ApprovalPolicyAuto, false},
		{agent.ApprovalPolicyAuto, agent.ApprovalPolicyBypass, false},
		{agent.ApprovalPolicyAuto, agent.ApprovalPolicyAuto, false},
	}
	for _, tc := range cases {
		if got := workerbase.LowersPermissions(tc.from, tc.to); got != tc.want {
			t.Errorf("workerbase.LowersPermissions(%s, %s) = %v", tc.from, tc.to, got)
		}
	}
}
