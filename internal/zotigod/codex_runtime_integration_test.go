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
	launches  chan zotigoruntime.WorkerLaunchSpec
	server    *httptest.Server
	connected chan *websocket.Conn
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

func (*fakeCodexRuntime) WorkerLifecycle() zotigoruntime.WorkerLifecycle {
	return zotigoruntime.WorkerLifecycle{IdleTimeout: 25 * time.Millisecond}
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
	stored, err := store.Get(ctx, created.ID)
	if err != nil || stored.ApprovalPolicy != agent.ApprovalPolicyBypass {
		t.Fatalf("stored Codex approval policy = %v, err=%v", stored, err)
	}
}
