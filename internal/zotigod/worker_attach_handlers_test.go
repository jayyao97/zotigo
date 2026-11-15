package zotigod

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/internal/testutil/sessionlogtest"

	"github.com/bytedance/sonic"
	"github.com/gorilla/websocket"
	"github.com/jayyao97/zotigo/core/config"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
	"github.com/jayyao97/zotigo/internal/zotigod/workerbase"
)

func TestWorkerDisconnectClearsRunningActivity(t *testing.T) {
	registry := newSessionRegistry()
	registry.Add(Session{ID: "sess-disconnect", State: SessionStateRunning, Working: true, ActiveTool: "shell"})
	if err := registry.SetWorkerGeneration("sess-disconnect", "worker-1"); err != nil {
		t.Fatal(err)
	}
	handler := &handler{
		registry:   registry,
		items:      &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}},
		events:     sessionlog.NewEventBroker(),
		sessionOps: newSessionOperationLocks(),
	}

	handler.handleWorkerDisconnect("sess-disconnect", "worker-1")
	session, _ := registry.Get("sess-disconnect")
	if session.Working || session.ActiveTool != "" {
		t.Fatalf("disconnected activity = %#v", session)
	}
}

type recordingWorkerStopper struct{ stopped []string }

func (s *recordingWorkerStopper) Start(context.Context, string, string) error { return nil }
func (s *recordingWorkerStopper) Stop(_ context.Context, sessionID string) error {
	s.stopped = append(s.stopped, sessionID)
	return nil
}

// A worker that dies mid-turn must not leave the session looking busy until
// someone sends the next message.
func TestWorkerDisconnectInterruptsItsOpenTurn(t *testing.T) {
	turnStarted := zotigosession.DisplayItem{Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}}
	approvalRequest := zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemApprovalRequest, Turn: &zotigosession.DisplayTurn{ID: "turn-1"},
		Approval: &zotigosession.DisplayApproval{ID: "apr-1", TurnID: "turn-1", Pending: []zotigosession.DisplayPendingApproval{{ToolCallID: "call-1", ToolName: "shell"}}},
	}
	for _, tc := range []struct {
		name            string
		agent           string
		items           []zotigosession.DisplayItem
		staleGeneration bool
		noStopper       bool
		wantInterrupted bool
	}{
		{name: "open turn", items: []zotigosession.DisplayItem{turnStarted}, wantInterrupted: true},
		{name: "no open turn", items: nil},
		{name: "turn waiting for approval is left to the next worker", items: []zotigosession.DisplayItem{turnStarted, approvalRequest}},
		{name: "a replaced worker is not the session's worker any more", items: []zotigosession.DisplayItem{turnStarted}, staleGeneration: true},
		{name: "codex sessions close their own turns", agent: "codex", items: []zotigosession.DisplayItem{turnStarted}},
		{name: "a worker the daemon cannot stop keeps its turn", items: []zotigosession.DisplayItem{turnStarted}, noStopper: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const id = "sess-lost-worker"
			registry := newSessionRegistry()
			registry.Add(Session{ID: id, Agent: tc.agent, State: SessionStateRunning, Working: true})
			if err := registry.SetWorkerGeneration(id, "worker-1"); err != nil {
				t.Fatal(err)
			}
			if tc.staleGeneration {
				if _, err := registry.RestartWorker(id); err != nil {
					t.Fatal(err)
				}
			}
			source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{id: tc.items}}
			stopper := &recordingWorkerStopper{}
			handler := &handler{
				registry: registry, items: source, launcher: stopper,
				events: sessionlog.NewEventBroker(), sessionOps: newSessionOperationLocks(),
			}
			if tc.noStopper {
				handler.launcher = nil
			}

			handler.handleWorkerDisconnect(id, "worker-1")

			items, _, err := source.LoadItems(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			var interrupted []zotigosession.DisplayItem
			for _, item := range items {
				if item.Type == zotigosession.DisplayItemTurnInterrupted {
					interrupted = append(interrupted, item)
				}
			}
			if !tc.wantInterrupted {
				if len(interrupted) != 0 {
					t.Fatalf("unexpected turn_interrupted items: %#v", interrupted)
				}
				return
			}
			if len(interrupted) != 1 || interrupted[0].Turn == nil || interrupted[0].Turn.ID != "turn-1" || interrupted[0].Turn.Reason != sessionlog.WorkerRestartedReason {
				t.Fatalf("turn_interrupted items = %#v, want one for turn-1 with reason %q", interrupted, sessionlog.WorkerRestartedReason)
			}
			if sessionlog.LastOpenTurnID(items) != "" {
				t.Fatal("turn is still open after the worker was lost")
			}
			if len(stopper.stopped) != 1 || stopper.stopped[0] != id {
				t.Fatalf("worker process stops = %v, want the lost worker stopped before its turn is closed", stopper.stopped)
			}
		})
	}
}

func TestStaleWorkerDisconnectDoesNotResetRestartedSession(t *testing.T) {
	registry := newSessionRegistry()
	registry.Add(Session{ID: "sess-stale-disconnect", State: SessionStateRunning})
	if err := registry.SetWorkerGeneration("sess-stale-disconnect", "worker-old"); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.RestartWorker("sess-stale-disconnect"); err != nil {
		t.Fatal(err)
	}
	if err := registry.SetWorkerGeneration("sess-stale-disconnect", "worker-new"); err != nil {
		t.Fatal(err)
	}
	handler := &handler{
		registry: registry, items: &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}},
		events: sessionlog.NewEventBroker(), sessionOps: newSessionOperationLocks(),
	}

	handler.handleWorkerDisconnect("sess-stale-disconnect", "worker-old")
	session, _ := registry.Get("sess-stale-disconnect")
	if session.State != SessionStateStarting || !registry.WorkerGenerationMatches(session.ID, "worker-new") {
		t.Fatalf("stale disconnect changed restarted session: %#v", session)
	}
}

func TestDisconnectedRunningSessionRejectsRemovedProfile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
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
	handler := newHandler(registry, sessionlog.StoredItemSource{Store: store}, handlerOptions{
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

func TestWorkerConnectTimeoutCanRetryWithMessage(t *testing.T) {
	source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}
	var server *httptest.Server
	var launches atomic.Int32
	workerReady := make(chan *websocket.Conn, 1)
	launcher := workerLauncherFunc(func(_ context.Context, sessionID string, _ string) error {
		if launches.Add(1) == 1 {
			return nil
		}
		go func() {
			conn, err := connectReadyWorker(server, sessionID)
			if err != nil {
				workerReady <- nil
				return
			}
			serveTestWorkerInputs(t, conn, source, sessionID)
			workerReady <- conn
		}()
		return nil
	})
	handler := newHandler(newSessionRegistry(), source, handlerOptions{
		launcher:             launcher,
		workerConnectTimeout: 250 * time.Millisecond,
	})
	server = httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/start", nil))
	assertAPIError(t, rec, http.StatusServiceUnavailable, "service_unavailable", errWorkerConnectTimeout.Error())

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(`{"text":"retry this session"}`))
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("message status = %d, want %d: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	if launches.Load() != 2 {
		t.Fatalf("worker launches = %d, want 2", launches.Load())
	}
	worker := <-workerReady
	if worker == nil {
		t.Fatal("expected retried worker websocket connection")
	}
	defer worker.Close()
}

func TestWorkerConnectTimeoutDoesNotFailNewActivation(t *testing.T) {
	for _, test := range []struct {
		name              string
		launchErr         error
		releaseBeforeWait bool
	}{
		{name: "old timeout"},
		{name: "old launcher error", launchErr: errors.New("old launch failed"), releaseBeforeWait: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			workDir := t.TempDir()
			sessionlogtest.WriteProfileConfig(t, workDir)
			launchStarted := make(chan struct{})
			releaseLaunch := make(chan struct{})
			launcher := workerLauncherFunc(func(context.Context, string, string) error {
				close(launchStarted)
				<-releaseLaunch
				return test.launchErr
			})
			registry := newSessionRegistry()
			const connectTimeout = 20 * time.Millisecond
			handler := newHandler(registry, &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}, handlerOptions{
				launcher:             launcher,
				workerConnectTimeout: connectTimeout,
			})
			created := registry.Add(newSession(workDir, "test"))
			ownerCtx, cancelOwner := context.WithCancel(context.Background())
			ownerDone := make(chan struct{})
			go func() {
				defer close(ownerDone)
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/start", nil).WithContext(ownerCtx)
				handler.ServeHTTP(rec, req)
			}()
			<-launchStarted
			cancelOwner()
			<-ownerDone
			first, ok := registry.Get(created.ID)
			if !ok {
				t.Fatal("session missing after first activation")
			}
			if _, err := registry.MarkRunning(created.ID); err != nil {
				t.Fatal(err)
			}
			second, err := registry.RestartWorker(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if second.workerActivation == first.workerActivation {
				t.Fatal("worker activation did not advance")
			}
			if test.releaseBeforeWait {
				close(releaseLaunch)
			}

			time.Sleep(4 * connectTimeout)
			if got := getSession(t, handler, created.ID); got.State != SessionStateStarting {
				t.Fatalf("new activation state = %q, want %q", got.State, SessionStateStarting)
			}
			if !test.releaseBeforeWait {
				close(releaseLaunch)
			}
		})
	}
}

func TestConnectedWorkerDisconnectBeforeReadyCanRetry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workDir := t.TempDir()
	sessionlogtest.WriteProfileConfig(t, workDir)
	source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}
	connected := make(chan *websocket.Conn, 2)
	var launches atomic.Int32
	var server *httptest.Server
	launcher := workerLauncherFunc(func(_ context.Context, sessionID string, _ string) error {
		go func() {
			if launches.Add(1) == 1 {
				url := "ws" + strings.TrimPrefix(server.URL, "http") + "/internal/workers/connect?session_id=" + sessionID
				conn, _, _ := websocket.DefaultDialer.Dial(url, nil)
				connected <- conn
				return
			}
			conn, _ := connectReadyWorker(server, sessionID)
			connected <- conn
		}()
		return nil
	})
	const connectTimeout = 25 * time.Millisecond
	registry := newSessionRegistry()
	handler := newHandler(registry, source, handlerOptions{
		launcher:             launcher,
		workerConnectTimeout: connectTimeout,
	})
	server = httptest.NewServer(handler)
	defer server.Close()
	created := registry.Add(newSession(workDir, "test"))
	waitForConnection := func(label string) *websocket.Conn {
		t.Helper()
		select {
		case conn := <-connected:
			if conn == nil {
				t.Fatalf("%s worker did not connect", label)
			}
			return conn
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s worker connection", label)
			return nil
		}
	}

	startDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/start", nil))
		startDone <- rec
	}()
	worker := waitForConnection("first")
	<-time.After(4 * connectTimeout)
	if got := getSession(t, handler, created.ID); got.State != SessionStateStarting {
		t.Fatalf("session state after connect timeout = %q, want %q", got.State, SessionStateStarting)
	}
	select {
	case rec := <-startDone:
		t.Fatalf("start completed before worker ready: status=%d body=%s", rec.Code, rec.Body.String())
	default:
	}

	_ = worker.Close()
	select {
	case rec := <-startDone:
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), errWorkerDisconnectedBeforeReady.Error()) {
			t.Fatalf("start after disconnect status = %d: %s", rec.Code, rec.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("start did not complete after worker disconnected")
	}
	if got := getSession(t, handler, created.ID); got.State != SessionStateCreated {
		t.Fatalf("session state after boot disconnect = %q, want %q", got.State, SessionStateCreated)
	}

	retryRec := httptest.NewRecorder()
	handler.ServeHTTP(retryRec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/start", nil))
	if retryRec.Code != http.StatusOK {
		t.Fatalf("retry start status = %d: %s", retryRec.Code, retryRec.Body.String())
	}
	retriedWorker := waitForConnection("retry")
	defer retriedWorker.Close()
}

// unmatchedWorkerAttachRequest builds an attach request for a generation that
// never connects. handleWorkerAttach waits for the matching worker under the
// request context, so a short request deadline reaches the same "no matching
// connection" rejection without sitting out the full workerRegistrationWait.
func unmatchedWorkerAttachRequest(t *testing.T, sessionID string) *http.Request {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	t.Cleanup(cancel)
	return httptest.NewRequest(http.MethodPost, "/internal/sessions/"+sessionID+"/worker/attach", workerReadyBody("stale")).WithContext(ctx)
}

func TestWorkerAttachRequiresActiveConnection(t *testing.T) {
	registry := newSessionRegistry()
	handler := newHandler(registry, &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}, handlerOptions{
		launcher: workerLauncherFunc(func(context.Context, string, string) error { return nil }),
	})
	created := registry.Add(newSession("", ""))
	if _, err := registry.Start(created.ID); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, unmatchedWorkerAttachRequest(t, created.ID))
	if rec.Code != http.StatusConflict {
		t.Fatalf("ready without connection status = %d, want %d: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}
	if got := getSession(t, handler, created.ID); got.State != SessionStateStarting {
		t.Fatalf("session state after rejected ready = %q, want %q", got.State, SessionStateStarting)
	}
}

func TestWorkerAttachRejectsStaleGeneration(t *testing.T) {
	registry := newSessionRegistry()
	workers := newWorkerRegistry()
	handler := newHandler(registry, &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}, handlerOptions{workers: workers})
	server := httptest.NewServer(handler)
	defer server.Close()
	created := registry.Add(Session{ID: "sess-stale-ready", State: SessionStateStarting})

	first, staleGeneration := connectWorker(t, server, created.ID)
	_ = first.Close()
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		if session, _ := registry.Get(created.ID); session.State == SessionStateCreated {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if session, _ := registry.Get(created.ID); session.State != SessionStateCreated {
		t.Fatalf("session state after first worker disconnected = %q, want %q", session.State, SessionStateCreated)
	}
	if _, err := registry.Start(created.ID); err != nil {
		t.Fatal(err)
	}
	second, currentGeneration := connectWorker(t, server, created.ID)
	defer second.Close()

	staleResp, err := postWorkerReady(server, created.ID, staleGeneration)
	if err != nil {
		t.Fatal(err)
	}
	defer staleResp.Body.Close()
	if staleResp.StatusCode != http.StatusConflict {
		t.Fatalf("stale ready status = %d, want %d", staleResp.StatusCode, http.StatusConflict)
	}
	if session, _ := registry.Get(created.ID); session.State != SessionStateStarting {
		t.Fatalf("session state after stale ready = %q, want %q", session.State, SessionStateStarting)
	}
	finishBody, err := sonic.Marshal(wire.FinishSessionRequest{Generation: staleGeneration, Error: "old worker failed"})
	if err != nil {
		t.Fatal(err)
	}
	finishResp, err := server.Client().Post(server.URL+"/internal/sessions/"+created.ID+"/worker/finish", "application/json", bytes.NewReader(finishBody))
	if err != nil {
		t.Fatal(err)
	}
	defer finishResp.Body.Close()
	if finishResp.StatusCode != http.StatusConflict {
		t.Fatalf("stale finish status = %d, want %d", finishResp.StatusCode, http.StatusConflict)
	}
	if session, _ := registry.Get(created.ID); session.State != SessionStateStarting {
		t.Fatalf("session state after stale finish = %q, want %q", session.State, SessionStateStarting)
	}

	markWorkerReady(t, server, created.ID, currentGeneration)
	if session, _ := registry.Get(created.ID); session.State != SessionStateRunning {
		t.Fatalf("session state after current ready = %q, want %q", session.State, SessionStateRunning)
	}
}

func TestWorkerAttachTransitionsToRunning(t *testing.T) {
	handler := newTestHandler()
	server := httptest.NewServer(handler)
	defer server.Close()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker, generation := connectWorker(t, server, created.ID)
	defer worker.Close()

	resp, err := postWorkerReady(server, created.ID, generation)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var running Session
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := decodeAPIData(t, data, &running); err != nil {
		t.Fatalf("decode attach response: %v", err)
	}
	if running.State != SessionStateRunning {
		t.Fatalf("expected state %q, got %q", SessionStateRunning, running.State)
	}

	repeated, err := postWorkerReady(server, created.ID, generation)
	if err != nil {
		t.Fatal(err)
	}
	defer repeated.Body.Close()
	if repeated.StatusCode != http.StatusOK {
		t.Fatalf("expected repeated ready status %d, got %d", http.StatusOK, repeated.StatusCode)
	}
}

func TestWorkerConnectTransitionsToRunningAndReceivesCommands(t *testing.T) {
	source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()

	created := createSession(t, handler)
	startSession(t, handler, created.ID)

	worker, generation := connectWorker(t, server, created.ID)
	defer worker.Close()
	if got := getSession(t, handler, created.ID); got.State != SessionStateStarting {
		t.Fatalf("expected state %q before ready, got %q", SessionStateStarting, got.State)
	}
	markWorkerReady(t, server, created.ID, generation)
	requests := serveTestWorkerInputs(t, worker, source, created.ID)

	if got := getSession(t, handler, created.ID); got.State != SessionStateRunning {
		t.Fatalf("expected state %q, got %q", SessionStateRunning, got.State)
	}
	appendTurnStarted(t, source, created.ID, "turn-1")

	postSteering(t, handler, created.ID, "use the smaller fix")
	request := <-requests
	if !request.SteeringOnly || request.Command.Steering == nil || request.Command.Steering.Text != "use the smaller fix" {
		t.Fatalf("unexpected steering input request: %#v", request)
	}

}

func TestWorkerHeartbeatUnregistersUnresponsiveConnection(t *testing.T) {
	source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}
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

func TestWorkerConnectRejectsNonLiveSession(t *testing.T) {
	handler := newTestHandler()
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
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatalf("create file store: %v", err)
	}
	defer store.Close()

	unlock, err := workerbase.AcquireSessionLock(context.Background(), store, "sess-lock")
	if err != nil {
		t.Fatalf("acquire first lock: %v", err)
	}
	if _, err := workerbase.AcquireSessionLock(context.Background(), store, "sess-lock"); err == nil {
		t.Fatal("expected second lock to fail")
	}
	if err := unlock(); err != nil {
		t.Fatalf("release first lock: %v", err)
	}

	unlock, err = workerbase.AcquireSessionLock(context.Background(), store, "sess-lock")
	if err != nil {
		t.Fatalf("expected lock after unlock: %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatalf("release reacquired lock: %v", err)
	}
}

func TestWorkerFinishSerializesWithMessageAppend(t *testing.T) {
	source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}
	handler := newHandler(newSessionRegistry(), source)
	server := httptest.NewServer(handler)
	defer server.Close()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	defer worker.Close()
	serveTestWorkerInputs(t, worker, source, created.ID)

	appendStarted := make(chan struct{})
	releaseAppend := make(chan struct{})
	source.AppendHook = func(sessionID string, item zotigosession.DisplayItem) {
		if sessionID == created.ID && item.Command != nil && item.Command.Type == wire.CommandMessage {
			close(appendStarted)
			<-releaseAppend
		}
	}
	messageDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/"+created.ID+"/messages", strings.NewReader(`{"text":"before finish"}`)))
		messageDone <- rec
	}()
	<-appendStarted
	finishDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/internal/sessions/"+created.ID+"/worker/finish", nil))
		finishDone <- rec
	}()
	select {
	case finish := <-finishDone:
		close(releaseAppend)
		t.Fatalf("finish bypassed message append lock: %d: %s", finish.Code, finish.Body.String())
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseAppend)
	if message := <-messageDone; message.Code != http.StatusCreated {
		t.Fatalf("message status = %d: %s", message.Code, message.Body.String())
	}
	if finish := <-finishDone; finish.Code != http.StatusConflict {
		t.Fatalf("finish status = %d: %s", finish.Code, finish.Body.String())
	}
	if session := getSession(t, handler, created.ID); session.State != SessionStateRunning {
		t.Fatalf("session state = %q, want %q", session.State, SessionStateRunning)
	}
	commands := getCommands(t, handler, "/internal/sessions/"+created.ID+"/commands?after=0")
	if len(commands.Commands) != 1 || commands.Commands[0].Message == nil || commands.Commands[0].Message.Text != "before finish" {
		t.Fatalf("expected pending message command after rejected finish, got %#v", commands)
	}
}

func TestWorkerDisplayLogInterruptsOpenTurnAfterRestart(t *testing.T) {
	source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}
	if _, err := source.AppendItem(context.Background(), "sess-restart", zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnStarted,
		Turn: &zotigosession.DisplayTurn{ID: "turn-stale"},
	}); err != nil {
		t.Fatalf("append stale turn: %v", err)
	}
	display := sessionlog.NewLog("sess-restart", source)
	if err := display.InterruptOpenTurn(context.Background(), sessionlog.WorkerRestartedReason); err != nil {
		t.Fatalf("interrupt open turn: %v", err)
	}

	items := source.Items["sess-restart"]
	if len(items) != 2 {
		t.Fatalf("expected start and interrupt items, got %#v", items)
	}
	if items[1].Type != zotigosession.DisplayItemTurnInterrupted || items[1].Turn == nil {
		t.Fatalf("expected interrupted turn, got %#v", items[1])
	}
	if items[1].Turn.ID != "turn-stale" || items[1].Turn.Reason != sessionlog.WorkerRestartedReason {
		t.Fatalf("unexpected interrupted turn: %#v", items[1].Turn)
	}
}

func TestWorkerAttachDoesNotResumePausedApproval(t *testing.T) {
	source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}
	registry := newSessionRegistry()
	handler := newHandler(registry, source)
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	attachWorker(t, handler, created.ID)
	approval := createApprovalRequestForTest(t, registry, source, created.ID)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, unmatchedWorkerAttachRequest(t, created.ID))
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d", http.StatusConflict, rec.Code)
	}
	if got := getSession(t, handler, created.ID); got.State != SessionStatePaused {
		t.Fatalf("expected state %q, got %q", SessionStatePaused, got.State)
	}

	_ = approval
}

func TestWorkerDisconnectReconcilesDurablePendingApproval(t *testing.T) {
	source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}
	registry := newSessionRegistry()
	handler := newHandler(registry, source)
	server := httptest.NewServer(handler)
	defer server.Close()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	if _, err := source.AppendItem(context.Background(), created.ID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemApprovalRequest,
		Approval: &zotigosession.DisplayApproval{
			ID:      "apr-notification-lost",
			TurnID:  "turn-1",
			Pending: []zotigosession.DisplayPendingApproval{{ToolCallID: "call-1", ToolName: "shell"}},
		},
	}); err != nil {
		t.Fatalf("append durable approval request: %v", err)
	}
	if _, err := source.AppendItem(context.Background(), created.ID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnPaused,
		Turn: &zotigosession.DisplayTurn{ID: "turn-1", Reason: "need_approval"},
	}); err != nil {
		t.Fatalf("append turn paused: %v", err)
	}
	if err := worker.Close(); err != nil {
		t.Fatalf("close worker before pause notification: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := getSession(t, handler, created.ID); got.State == SessionStatePaused {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected state %q, got %q", SessionStatePaused, getSession(t, handler, created.ID).State)
}

func TestWorkerDisconnectReconcilesResolvedApproval(t *testing.T) {
	source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}
	registry := newSessionRegistry()
	handler := newHandler(registry, source)
	server := httptest.NewServer(handler)
	defer server.Close()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	for _, item := range []zotigosession.DisplayItem{
		{Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-historical"}},
		{Type: zotigosession.DisplayItemApprovalRequest, Approval: &zotigosession.DisplayApproval{
			ID:      "apr-historical",
			TurnID:  "turn-historical",
			Pending: []zotigosession.DisplayPendingApproval{{ToolCallID: "call-historical", ToolName: "shell"}},
		}},
		{Type: zotigosession.DisplayItemTurnInterrupted, Turn: &zotigosession.DisplayTurn{ID: "turn-historical", Reason: sessionlog.WorkerRestartedReason}},
	} {
		if _, err := source.AppendItem(context.Background(), created.ID, item); err != nil {
			t.Fatalf("append historical display item: %v", err)
		}
	}
	approval := createApprovalRequestForTest(t, registry, source, created.ID)
	if _, err := source.AppendItem(context.Background(), created.ID, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemApprovalDecision,
		Approval: &zotigosession.DisplayApproval{
			ID:        approval.ID,
			TurnID:    approval.TurnID,
			Decisions: []zotigosession.DisplayApprovalDecision{{ToolCallID: "call-1", Approved: true}},
		},
	}); err != nil {
		t.Fatalf("append durable approval decision: %v", err)
	}
	if err := worker.Close(); err != nil {
		t.Fatalf("close worker before acknowledgement: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if got := getSession(t, handler, created.ID); got.State == SessionStateRunning {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected state %q, got %q", SessionStateRunning, getSession(t, handler, created.ID).State)
}

func TestWorkerDisconnectKeepsPendingApprovalPaused(t *testing.T) {
	source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}
	registry := newSessionRegistry()
	handler := newHandler(registry, source)
	server := httptest.NewServer(handler)
	defer server.Close()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)
	worker := dialWorker(t, server, created.ID)
	createApprovalRequestForTest(t, registry, source, created.ID)
	if err := worker.Close(); err != nil {
		t.Fatalf("close worker with pending approval: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	if got := getSession(t, handler, created.ID); got.State != SessionStatePaused {
		t.Fatalf("expected state %q, got %q", SessionStatePaused, got.State)
	}
}

func TestWorkerFinishTransitionsToEnded(t *testing.T) {
	handler := newTestHandler()
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
	source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}
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
	handler := newTestHandler()
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
	handler := newTestHandler()
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

func TestWorkerAttachRejectsCreatedSession(t *testing.T) {
	handler := newTestHandler()
	created := createSession(t, handler)

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, unmatchedWorkerAttachRequest(t, created.ID))

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected status %d, got %d", http.StatusConflict, rec.Code)
	}
}

func TestWorkerFinishRejectsBadJSON(t *testing.T) {
	handler := newTestHandler()
	created := createSession(t, handler)
	startSession(t, handler, created.ID)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/internal/sessions/"+created.ID+"/worker/finish", strings.NewReader(`{`))
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, rec.Code)
	}
}
