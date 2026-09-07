package zotigod

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jayyao97/zotigo/core/agent"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
)

type fakeCodexRuntime struct {
	launches    chan zotigoruntime.WorkerLaunchSpec
	server      *httptest.Server
	connected   chan *websocket.Conn
	idleTimeout time.Duration
}

func (*fakeCodexRuntime) Kind() zotigoruntime.AgentKind { return zotigoruntime.AgentCodex }

func (*fakeCodexRuntime) Probe(context.Context, zotigoruntime.ProbeRequest) (zotigoruntime.Capabilities, error) {
	return zotigoruntime.Capabilities{Installed: true, Models: []zotigoruntime.Model{{
		ID: "gpt-5.6-luna", DisplayName: "Luna", Default: true,
		SupportedReasoningEfforts: []string{"medium"},
	}}}, nil
}

func (f *fakeCodexRuntime) StartWorker(_ context.Context, spec zotigoruntime.WorkerLaunchSpec) error {
	f.launches <- spec
	go func() {
		url := "ws" + f.server.URL[len("http"):] + "/internal/workers/connect?session_id=" + spec.SessionID
		conn, response, err := websocket.DefaultDialer.Dial(url, nil)
		if err != nil {
			return
		}
		generation := response.Header.Get(workerGenerationHeader)
		ready, err := postWorkerReady(f.server, spec.SessionID, generation)
		if err == nil {
			_ = ready.Body.Close()
		}
		f.connected <- conn
	}()
	return nil
}

func (f *fakeCodexRuntime) WorkerLifecycle() zotigoruntime.WorkerLifecycle {
	timeout := f.idleTimeout
	if timeout == 0 {
		timeout = 25 * time.Millisecond
	}
	return zotigoruntime.WorkerLifecycle{IdleTimeout: timeout}
}

func TestCodexWorkerReleasesWhenIdleAndNewerCommandCancelsRelease(t *testing.T) {
	createdAt := time.Now().UTC()
	registry := newSessionRegistry()
	session := registry.Add(Session{
		ID: "sess-codex-idle", State: SessionStateStarting, Agent: string(zotigoruntime.AgentCodex),
		WorkingDirectory: t.TempDir(), CreatedAt: createdAt,
	})
	workers := newWorkerRegistry()
	fakeRuntime := &fakeCodexRuntime{}
	handler := newHandler(registry, &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}, handlerOptions{
		workers: workers, runtimes: newRuntimeRegistry(nativeRuntimeAdapter{}, fakeRuntime), workerConnectTimeout: time.Second,
	})
	server := httptest.NewServer(handler)
	fakeRuntime.server = server
	t.Cleanup(server.Close)

	worker, generation := connectWorker(t, server, session.ID)
	t.Cleanup(func() { _ = worker.Close() })
	markWorkerReady(t, server, session.ID, generation)
	command := commandResponse{ID: "command-1", Sequence: 1, Type: sessionCommandMessage, Message: &messageCommandPayload{Text: "hello"}}
	if !workers.Send(session.ID, command) {
		t.Fatal("send command to ready worker")
	}
	_ = readWorkerMessage(t, worker)
	time.Sleep(50 * time.Millisecond)
	if !workers.Has(session.ID) {
		t.Fatal("ready-worker idle timer closed a worker after a newer command")
	}
	if err := worker.WriteJSON(workerMessage{Type: workerMessageIdle, Idle: &workerIdle{CommandSequence: command.Sequence}}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Second); workers.Has(session.ID) && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if workers.Has(session.ID) {
		t.Fatal("Codex worker remained connected after reporting idle")
	}
}

func TestCodexWorkerDoesNotReleaseWhileSubmittingInput(t *testing.T) {
	createdAt := time.Now().UTC()
	registry := newSessionRegistry()
	session := registry.Add(Session{
		ID: "sess-codex-input", State: SessionStateStarting, Agent: string(zotigoruntime.AgentCodex),
		WorkingDirectory: t.TempDir(), CreatedAt: createdAt,
	})
	workers := newWorkerRegistry()
	fakeRuntime := &fakeCodexRuntime{}
	handler := newHandler(registry, &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}, handlerOptions{
		workers: workers, runtimes: newRuntimeRegistry(nativeRuntimeAdapter{}, fakeRuntime), workerConnectTimeout: time.Second,
	})
	server := httptest.NewServer(handler)
	fakeRuntime.server = server
	t.Cleanup(server.Close)

	worker, generation := connectWorker(t, server, session.ID)
	t.Cleanup(func() { _ = worker.Close() })
	markWorkerReady(t, server, session.ID, generation)
	registry.MarkWorking(session.ID, "tool")

	resultCh := make(chan error, 1)
	go func() {
		_, err := workers.SubmitInput(context.Background(), session.ID, workerInputRequest{
			Command: commandResponse{ID: "command-1", Type: sessionCommandMessage, Message: &messageCommandPayload{Text: "hello"}},
		})
		resultCh <- err
	}()
	request := readWorkerMessage(t, worker)
	if request.Type != workerMessageInputRequest || request.InputRequest == nil {
		t.Fatalf("expected input request, got %#v", request)
	}
	if err := worker.WriteJSON(workerMessage{Type: workerMessageIdle, Idle: &workerIdle{}}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		current, _ := registry.Get(session.ID)
		if !current.Working && current.ActiveTool == "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	current, _ := registry.Get(session.ID)
	if current.Working || current.ActiveTool != "" {
		t.Fatal("accepted idle notification did not update session state during input submission")
	}
	time.Sleep(50 * time.Millisecond)
	if !workers.Has(session.ID) {
		t.Fatal("idle notification closed a worker while input submission was in progress")
	}
	command := request.InputRequest.Command
	command.Sequence = 1
	if err := worker.WriteJSON(workerMessage{Type: workerMessageInputResult, InputResult: &workerInputResult{
		RequestID: request.InputRequest.RequestID, Command: &command,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := <-resultCh; err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if !workers.Has(session.ID) {
		t.Fatal("stale idle notification closed a worker after it accepted newer input")
	}
}

func TestCodexWorkerKeepsInputLeaseAfterCallerCancellation(t *testing.T) {
	createdAt := time.Now().UTC()
	registry := newSessionRegistry()
	session := registry.Add(Session{
		ID: "sess-codex-canceled-input", State: SessionStateStarting, Agent: string(zotigoruntime.AgentCodex),
		WorkingDirectory: t.TempDir(), CreatedAt: createdAt,
	})
	workers := newWorkerRegistry()
	fakeRuntime := &fakeCodexRuntime{}
	handler := newHandler(registry, &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}, handlerOptions{
		workers: workers, runtimes: newRuntimeRegistry(nativeRuntimeAdapter{}, fakeRuntime), workerConnectTimeout: time.Second,
	})
	server := httptest.NewServer(handler)
	fakeRuntime.server = server
	t.Cleanup(server.Close)

	worker, generation := connectWorker(t, server, session.ID)
	t.Cleanup(func() { _ = worker.Close() })
	markWorkerReady(t, server, session.ID, generation)

	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan error, 1)
	go func() {
		_, err := workers.SubmitInput(ctx, session.ID, workerInputRequest{
			Command: commandResponse{ID: "command-1", Type: sessionCommandMessage, Message: &messageCommandPayload{Text: "hello"}},
		})
		resultCh <- err
	}()
	request := readWorkerMessage(t, worker)
	if request.Type != workerMessageInputRequest || request.InputRequest == nil {
		t.Fatalf("expected input request, got %#v", request)
	}
	cancel()
	if err := <-resultCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled caller, got %v", err)
	}
	if !workers.CloseWhenIdle(session.ID, generation, workerIdle{}, 25*time.Millisecond) {
		t.Fatal("idle notification was not retained while the worker resolved canceled input")
	}
	time.Sleep(50 * time.Millisecond)
	if !workers.Has(session.ID) {
		t.Fatal("worker closed before resolving input from a canceled caller")
	}
	command := request.InputRequest.Command
	command.Sequence = 1
	if err := worker.WriteJSON(workerMessage{Type: workerMessageInputResult, InputResult: &workerInputResult{
		RequestID: request.InputRequest.RequestID, Command: &command,
	}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if !workers.Has(session.ID) {
		t.Fatal("stale idle notification closed worker after canceled input was accepted")
	}
}

func TestCodexWorkerRestoresIdleReleaseAfterRejectedInput(t *testing.T) {
	createdAt := time.Now().UTC()
	registry := newSessionRegistry()
	session := registry.Add(Session{
		ID: "sess-codex-rejected-input", State: SessionStateStarting, Agent: string(zotigoruntime.AgentCodex),
		WorkingDirectory: t.TempDir(), CreatedAt: createdAt,
	})
	workers := newWorkerRegistry()
	fakeRuntime := &fakeCodexRuntime{idleTimeout: 500 * time.Millisecond}
	handler := newHandler(registry, &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}, handlerOptions{
		workers: workers, runtimes: newRuntimeRegistry(nativeRuntimeAdapter{}, fakeRuntime), workerConnectTimeout: time.Second,
	})
	server := httptest.NewServer(handler)
	fakeRuntime.server = server
	t.Cleanup(server.Close)

	worker, generation := connectWorker(t, server, session.ID)
	t.Cleanup(func() { _ = worker.Close() })
	markWorkerReady(t, server, session.ID, generation)
	registry.MarkWorking(session.ID, "tool")

	resultCh := make(chan error, 1)
	go func() {
		_, err := workers.SubmitInput(context.Background(), session.ID, workerInputRequest{
			Command:      commandResponse{ID: "command-1", Type: sessionCommandSteering, Steering: &steeringCommandPayload{Text: "hello"}},
			SteeringOnly: true,
		})
		resultCh <- err
	}()
	request := readWorkerMessage(t, worker)
	if request.Type != workerMessageInputRequest || request.InputRequest == nil {
		t.Fatalf("expected input request, got %#v", request)
	}
	if err := worker.WriteJSON(workerMessage{Type: workerMessageIdle, Idle: &workerIdle{}}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		current, _ := registry.Get(session.ID)
		if !current.Working && current.ActiveTool == "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	current, _ := registry.Get(session.ID)
	if current.Working || current.ActiveTool != "" {
		t.Fatal("rejected input's pending idle did not update session state")
	}
	if !workers.Has(session.ID) {
		t.Fatal("pending idle closed worker before rejected input resolved")
	}
	if err := worker.WriteJSON(workerMessage{Type: workerMessageInputResult, InputResult: &workerInputResult{
		RequestID: request.InputRequest.RequestID, ErrorCode: "no_active_turn", Error: "steering requires an active turn",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := <-resultCh; !errors.Is(err, errNoActiveTurn) {
		t.Fatalf("expected no active turn, got %v", err)
	}
	if current, _ := registry.Get(session.ID); current.Working || current.ActiveTool != "" {
		t.Fatal("session returned to working after rejected input")
	}
	for deadline := time.Now().Add(time.Second); workers.Has(session.ID) && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if workers.Has(session.ID) {
		t.Fatal("Codex worker remained connected after rejected input restored its idle release")
	}
}

func TestRejectedInputResultWinsImmediateIdleDisconnect(t *testing.T) {
	createdAt := time.Now().UTC()
	registry := newSessionRegistry()
	session := registry.Add(Session{
		ID: "sess-codex-immediate-idle", State: SessionStateStarting, Agent: string(zotigoruntime.AgentCodex),
		WorkingDirectory: t.TempDir(), CreatedAt: createdAt,
	})
	workers := newWorkerRegistry()
	fakeRuntime := &fakeCodexRuntime{}
	handler := newHandler(registry, &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}, handlerOptions{
		workers: workers, runtimes: newRuntimeRegistry(nativeRuntimeAdapter{}, fakeRuntime), workerConnectTimeout: time.Second,
	})
	server := httptest.NewServer(handler)
	fakeRuntime.server = server
	t.Cleanup(server.Close)

	worker, generation := connectWorker(t, server, session.ID)
	t.Cleanup(func() { _ = worker.Close() })
	markWorkerReady(t, server, session.ID, generation)

	resultCh := make(chan error, 1)
	go func() {
		_, err := workers.SubmitInput(context.Background(), session.ID, workerInputRequest{
			Command:      commandResponse{ID: "command-1", Type: sessionCommandSteering, Steering: &steeringCommandPayload{Text: "hello"}},
			SteeringOnly: true,
		})
		resultCh <- err
	}()
	request := readWorkerMessage(t, worker)
	if request.Type != workerMessageInputRequest || request.InputRequest == nil {
		t.Fatalf("expected input request, got %#v", request)
	}
	if !workers.CloseWhenIdle(session.ID, generation, workerIdle{}, 0) {
		t.Fatal("defer immediate idle close")
	}
	if err := worker.WriteJSON(workerMessage{Type: workerMessageInputResult, InputResult: &workerInputResult{
		RequestID: request.InputRequest.RequestID, ErrorCode: "no_active_turn", Error: "steering requires an active turn",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := <-resultCh; !errors.Is(err, errNoActiveTurn) {
		t.Fatalf("expected no active turn result before disconnect, got %v", err)
	}
}

func TestCodexWorkerRestoresIdleReleaseAfterDuplicateInput(t *testing.T) {
	createdAt := time.Now().UTC()
	registry := newSessionRegistry()
	session := registry.Add(Session{
		ID: "sess-codex-duplicate-input", State: SessionStateStarting, Agent: string(zotigoruntime.AgentCodex),
		WorkingDirectory: t.TempDir(), CreatedAt: createdAt,
	})
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{
		session.ID: {
			{ID: "command-1", Sequence: 1, Type: zotigosession.DisplayItemUserMessage, Command: &zotigosession.DisplayCommand{Type: sessionCommandMessage, Text: "hello"}},
			{Sequence: 2, Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
			{Sequence: 3, Type: zotigosession.DisplayItemTurnCompleted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
		},
	}}
	workers := newWorkerRegistry()
	fakeRuntime := &fakeCodexRuntime{idleTimeout: 500 * time.Millisecond}
	handler := newHandler(registry, source, handlerOptions{
		workers: workers, runtimes: newRuntimeRegistry(nativeRuntimeAdapter{}, fakeRuntime), workerConnectTimeout: time.Second,
	})
	server := httptest.NewServer(handler)
	fakeRuntime.server = server
	t.Cleanup(server.Close)

	worker, generation := connectWorker(t, server, session.ID)
	t.Cleanup(func() { _ = worker.Close() })
	markWorkerReady(t, server, session.ID, generation)
	registry.MarkWorking(session.ID, "tool")

	resultCh := make(chan error, 1)
	go func() {
		_, err := workers.SubmitInput(context.Background(), session.ID, workerInputRequest{
			Command: commandResponse{ID: "command-1", Type: sessionCommandMessage, Message: &messageCommandPayload{Text: "hello"}},
		})
		resultCh <- err
	}()
	request := readWorkerMessage(t, worker)
	if request.Type != workerMessageInputRequest || request.InputRequest == nil {
		t.Fatalf("expected input request, got %#v", request)
	}
	if err := worker.WriteJSON(workerMessage{Type: workerMessageIdle, Idle: &workerIdle{CommandSequence: 1}}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		current, _ := registry.Get(session.ID)
		if !current.Working && current.ActiveTool == "" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	current, _ := registry.Get(session.ID)
	if current.Working || current.ActiveTool != "" {
		t.Fatal("duplicate input's pending idle did not update session state")
	}
	if !workers.Has(session.ID) {
		t.Fatal("pending idle closed worker before duplicate input resolved")
	}
	existing := request.InputRequest.Command
	existing.Sequence = 1
	if err := worker.WriteJSON(workerMessage{Type: workerMessageInputResult, InputResult: &workerInputResult{
		RequestID: request.InputRequest.RequestID, Command: &existing,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := <-resultCh; err != nil {
		t.Fatal(err)
	}
	if current, _ := registry.Get(session.ID); current.Working || current.ActiveTool != "" {
		t.Fatal("session returned to working after duplicate input")
	}
	for deadline := time.Now().Add(time.Second); workers.Has(session.ID) && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	if workers.Has(session.ID) {
		t.Fatal("Codex worker remained connected after duplicate input restored its idle release")
	}
}

func TestWorkerRegistryTreatsIdleClosingWorkerAsOffline(t *testing.T) {
	workers := newWorkerRegistry()
	worker := &workerConnection{
		sessionID: "sess-closing", generation: "worker-1", registry: workers,
		sendCh: make(chan workerMessage, 1), doneCh: make(chan struct{}), closing: true,
	}
	workers.workers[worker.sessionID] = worker

	if workers.Has(worker.sessionID) {
		t.Fatal("closing worker reported online")
	}
	if workers.Send(worker.sessionID, commandResponse{Sequence: 1}) {
		t.Fatal("command accepted by closing worker")
	}
	done := make(chan struct{})
	close(done)
	if workers.Wait(done, worker.sessionID) {
		t.Fatal("wait returned a closing worker")
	}
}

func TestCodexSessionUsesWorkspaceCWDWithoutRuntimeProjectBinding(t *testing.T) {
	root := t.TempDir()
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	catalog, err := zotigoworkspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = catalog.Close() })
	ctx := context.Background()
	project, err := catalog.CreateProject(ctx, "Project")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := catalog.CreateWorkspace(ctx, project.ID, "Workspace")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = catalog.ProvisionWorkspace(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}

	fake := &fakeCodexRuntime{
		launches: make(chan zotigoruntime.WorkerLaunchSpec, 1), connected: make(chan *websocket.Conn, 1),
	}
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{
		store: store, catalog: catalog, runtimes: newRuntimeRegistry(nativeRuntimeAdapter{}, fake),
		workerConnectTimeout: time.Second,
	})
	server := httptest.NewServer(handler)
	fake.server = server
	t.Cleanup(server.Close)

	createdResponse := requestCatalog(t, handler, http.MethodPost, "/sessions",
		`{"workspace_id":`+quotedJSON(t, workspace.ID)+`,"agent":"codex","model":"gpt-5.6-luna","reasoning_effort":"medium"}`)
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("create status = %d: %s", createdResponse.Code, createdResponse.Body.String())
	}
	var created Session
	if err := decodeAPIData(t, createdResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.GetRuntimeWorkspaceBinding(ctx, workspace.ID, "codex"); !errors.Is(err, zotigoworkspace.ErrNotFound) {
		t.Fatalf("runtime Project binding unexpectedly created: %v", err)
	}

	started := requestCatalog(t, handler, http.MethodPost, "/sessions/"+created.ID+"/start", "")
	if started.Code != http.StatusOK {
		t.Fatalf("start status = %d: %s", started.Code, started.Body.String())
	}
	workerConn := <-fake.connected
	t.Cleanup(func() { _ = workerConn.Close() })
	launch := <-fake.launches
	if launch.WorkingDirectory != workspace.RootPath {
		t.Fatalf("launch cwd = %q", launch.WorkingDirectory)
	}
	for _, directory := range []string{"code", "notes", "artifacts"} {
		if _, err := os.Stat(filepath.Join(launch.WorkingDirectory, directory)); err != nil {
			t.Fatalf("launch cwd does not expose %s: %v", directory, err)
		}
	}
	if launch.SessionStoreRoot != store.RootDir() {
		t.Fatalf("launch session store root = %q, want %q", launch.SessionStoreRoot, store.RootDir())
	}
	if launch.Settings.ApprovalPolicy != string(agent.ApprovalPolicyAuto) {
		t.Fatalf("launch approval policy = %q", launch.Settings.ApprovalPolicy)
	}
	stored, err := store.Get(ctx, created.ID)
	if err != nil || stored.ApprovalPolicy != agent.ApprovalPolicyAuto {
		t.Fatalf("stored Codex approval policy = %v, err=%v", stored, err)
	}
}
