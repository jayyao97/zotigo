package zotigod

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jayyao97/zotigo/core/agent"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
	"github.com/jayyao97/zotigo/internal/codexapp"
	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
)

type fakeCodexRuntime struct {
	mu              sync.Mutex
	workspace       *zotigoruntime.ExternalWorkspace
	createCount     int
	missing         bool
	createTombstone bool
	launches        []zotigoruntime.WorkerLaunchSpec
	server          *httptest.Server
	connected       chan *websocket.Conn
}

func TestE2ECodexSessionStartAutomaticallyCreatesProject(t *testing.T) {
	if os.Getenv("ZOTIGO_CODEX_E2E") != "1" {
		t.Skip("set ZOTIGO_CODEX_E2E=1 to run against the installed codex binary")
	}
	binaryPath, _, err := codexapp.Discover()
	if err != nil {
		t.Skipf("codex is not installed: %v", err)
	}
	root := t.TempDir()
	codexHome := filepath.Join(root, "codex-home")
	if err := os.Mkdir(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)
	runtimeDir, err := os.MkdirTemp("/tmp", "zotigo-session-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	host, err := codexapp.NewHost(binaryPath, runtimeDir, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })

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
	workspace, err := catalog.CreateWorkspace(ctx, project.ID, "Workspace E2E")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = catalog.ProvisionWorkspace(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}

	connected := make(chan *websocket.Conn, 1)
	var server *httptest.Server
	startWorker := func(_ context.Context, spec zotigoruntime.WorkerLaunchSpec, _ string) error {
		go func() {
			url := "ws" + server.URL[len("http"):] + "/internal/workers/connect?session_id=" + spec.SessionID
			headers := http.Header{}
			headers.Set(workerWorkspaceBindingRevisionHeader, strconv.FormatUint(spec.WorkspaceBinding.Revision, 10))
			conn, response, dialErr := websocket.DefaultDialer.Dial(url, headers)
			if dialErr != nil {
				return
			}
			generation := response.Header.Get(workerGenerationHeader)
			ready, readyErr := postWorkerReady(server, spec.SessionID, generation)
			if readyErr == nil {
				_ = ready.Body.Close()
			}
			connected <- conn
		}()
		return nil
	}
	adapter := codexapp.NewAdapter(host, startWorker)
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{
		store: store, catalog: catalog, runtimes: newRuntimeRegistry(nativeRuntimeAdapter{}, adapter),
		workerConnectTimeout: 2 * time.Second,
	})
	server = httptest.NewServer(handler)
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
	binding, err := catalog.GetRuntimeWorkspaceBinding(ctx, workspace.ID, "codex")
	if err != nil || binding.State != zotigoworkspace.RuntimeWorkspaceBindingBound {
		t.Fatalf("Project was not bound during session creation: binding=%#v err=%v", binding, err)
	}
	started := requestCatalog(t, handler, http.MethodPost, "/sessions/"+created.ID+"/start", "")
	if started.Code != http.StatusOK {
		t.Fatalf("start status = %d: %s", started.Code, started.Body.String())
	}
	workerConn := <-connected
	t.Cleanup(func() { _ = workerConn.Close() })

	if binding.State != zotigoworkspace.RuntimeWorkspaceBindingBound || binding.ExternalID == "" {
		t.Fatalf("runtime binding = %#v", binding)
	}
	client, _, err := host.Ensure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var readResponse struct {
		Project struct {
			ID       string            `json:"id"`
			Name     string            `json:"name"`
			Metadata map[string]string `json:"metadata"`
		} `json:"project"`
	}
	if err := client.Call(ctx, "project/read", map[string]any{"projectId": binding.ExternalID}, &readResponse); err != nil {
		t.Fatal(err)
	}
	if readResponse.Project.ID != binding.ExternalID || readResponse.Project.Name != workspace.Title || readResponse.Project.Metadata["zotigod.workspace_id"] != workspace.ID {
		t.Fatalf("codex project = %#v, binding=%#v", readResponse.Project, binding)
	}
}

func (*fakeCodexRuntime) Kind() zotigoruntime.AgentKind { return zotigoruntime.AgentCodex }

func (*fakeCodexRuntime) Probe(context.Context, zotigoruntime.ProbeRequest) (zotigoruntime.Capabilities, error) {
	return zotigoruntime.Capabilities{Installed: true, Models: []zotigoruntime.Model{{
		ID: "gpt-5.6-luna", DisplayName: "Luna", Default: true,
		SupportedReasoningEfforts: []string{"medium"},
	}}}, nil
}

func (f *fakeCodexRuntime) StartWorker(_ context.Context, spec zotigoruntime.WorkerLaunchSpec) error {
	f.mu.Lock()
	f.launches = append(f.launches, spec)
	f.mu.Unlock()
	go func() {
		url := "ws" + f.server.URL[len("http"):] + "/internal/workers/connect?session_id=" + spec.SessionID
		headers := http.Header{}
		headers.Set(workerWorkspaceBindingRevisionHeader, strconv.FormatUint(spec.WorkspaceBinding.Revision, 10))
		conn, response, err := websocket.DefaultDialer.Dial(url, headers)
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

func (f *fakeCodexRuntime) ReadWorkspace(context.Context, string) (zotigoruntime.ExternalWorkspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.missing || f.workspace == nil {
		return zotigoruntime.ExternalWorkspace{}, zotigoruntime.ErrWorkspaceNotFound
	}
	return *f.workspace, nil
}

func (f *fakeCodexRuntime) FindWorkspace(context.Context, zotigoruntime.WorkspaceSpec) (*zotigoruntime.ExternalWorkspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.workspace == nil {
		return nil, nil
	}
	copy := *f.workspace
	return &copy, nil
}

func (f *fakeCodexRuntime) CreateWorkspace(_ context.Context, intent zotigoruntime.WorkspaceCreateIntent) (zotigoruntime.ExternalWorkspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCount++
	if f.createTombstone {
		f.createTombstone = false
		return zotigoruntime.ExternalWorkspace{}, zotigoruntime.ErrWorkspaceCreateTombstone
	}
	f.missing = false
	f.workspace = &zotigoruntime.ExternalWorkspace{
		ID: "codex-project-" + strconv.Itoa(f.createCount), Name: intent.Name, RootPath: intent.RootPath,
		Metadata: map[string]string{"zotigod.workspace_id": intent.WorkspaceID},
	}
	return *f.workspace, nil
}

func TestCodexSessionStartCreatesProjectBindingAndLaunchesBoundWorker(t *testing.T) {
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

	fake := &fakeCodexRuntime{connected: make(chan *websocket.Conn, 1)}
	workers := newWorkerRegistry()
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{
		store: store, catalog: catalog, runtimes: newRuntimeRegistry(nativeRuntimeAdapter{}, fake),
		workerConnectTimeout: time.Second, workers: workers,
	})
	server := httptest.NewServer(handler)
	fake.server = server
	t.Cleanup(server.Close)

	body := `{"workspace_id":` + quotedJSON(t, workspace.ID) + `,"agent":"codex","model":"gpt-5.6-luna","reasoning_effort":"medium"}`
	responses := make([]*httptest.ResponseRecorder, 2)
	var creates sync.WaitGroup
	for i := range responses {
		creates.Add(1)
		go func(index int) {
			defer creates.Done()
			request := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			responses[index] = httptest.NewRecorder()
			handler.ServeHTTP(responses[index], request)
		}(i)
	}
	creates.Wait()
	for _, response := range responses {
		if response.Code != http.StatusCreated {
			t.Fatalf("create status = %d: %s", response.Code, response.Body.String())
		}
	}
	var created Session
	if err := decodeAPIData(t, responses[0].Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	binding, err := catalog.GetRuntimeWorkspaceBinding(ctx, workspace.ID, "codex")
	if err != nil || binding.State != zotigoworkspace.RuntimeWorkspaceBindingBound {
		t.Fatalf("Project was not bound during session creation: binding=%#v err=%v", binding, err)
	}
	startedResponse := requestCatalog(t, handler, http.MethodPost, "/sessions/"+created.ID+"/start", "")
	if startedResponse.Code != http.StatusOK {
		t.Fatalf("start status = %d: %s", startedResponse.Code, startedResponse.Body.String())
	}
	workerConn := <-fake.connected
	t.Cleanup(func() { _ = workerConn.Close() })

	if binding.State != zotigoworkspace.RuntimeWorkspaceBindingBound || binding.ExternalID != "codex-project-1" || binding.Revision != 2 {
		t.Fatalf("binding = %#v", binding)
	}
	fake.mu.Lock()
	createCount := fake.createCount
	launchCount := len(fake.launches)
	var launch zotigoruntime.WorkerLaunchSpec
	if launchCount > 0 {
		launch = fake.launches[0]
	}
	fake.mu.Unlock()
	if createCount != 1 || launchCount != 1 {
		t.Fatalf("create count=%d launches=%d", createCount, launchCount)
	}
	if launch.WorkspaceBinding == nil || launch.WorkspaceBinding.ExternalID != binding.ExternalID {
		t.Fatalf("launch binding = %#v", launch.WorkspaceBinding)
	}
	if launch.WorkingDirectory != filepath.Join(workspace.RootPath, "code") {
		t.Fatalf("launch cwd = %q", launch.WorkingDirectory)
	}
	if launch.SessionStoreRoot != store.RootDir() {
		t.Fatalf("launch session store root = %q, want %q", launch.SessionStoreRoot, store.RootDir())
	}
	stored, err := store.Get(ctx, created.ID)
	if err != nil || stored.ApprovalPolicy != agent.ApprovalPolicyBypass {
		t.Fatalf("stored Codex approval policy = %v, err=%v", stored, err)
	}
	rejected := requestCatalog(t, handler, http.MethodPost, "/sessions",
		`{"workspace_id":`+quotedJSON(t, workspace.ID)+`,"agent":"codex","model":"gpt-5.6-luna","reasoning_effort":"medium","approval_policy":"auto"}`)
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("Codex auto approval status = %d: %s", rejected.Code, rejected.Body.String())
	}
	fake.mu.Lock()
	fake.workspace = nil
	fake.missing = true
	fake.mu.Unlock()
	rebuilder := requestCatalog(t, handler, http.MethodPost, "/sessions", body)
	if rebuilder.Code != http.StatusCreated {
		t.Fatalf("rebuild session status = %d: %s", rebuilder.Code, rebuilder.Body.String())
	}
	if workers.Has(created.ID) {
		t.Fatal("stale worker remained online after workspace Project rebinding")
	}
	restarted := requestCatalog(t, handler, http.MethodPost, "/sessions/"+created.ID+"/start", "")
	if restarted.Code != http.StatusOK {
		t.Fatalf("restart stale worker status = %d: %s", restarted.Code, restarted.Body.String())
	}
	replacement := <-fake.connected
	t.Cleanup(func() { _ = replacement.Close() })
	fake.mu.Lock()
	launchCount = len(fake.launches)
	latestLaunch := fake.launches[launchCount-1]
	fake.mu.Unlock()
	latestBinding, err := catalog.GetRuntimeWorkspaceBinding(ctx, workspace.ID, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if launchCount != 2 || latestLaunch.WorkspaceBinding == nil || latestLaunch.WorkspaceBinding.Revision != latestBinding.Revision {
		t.Fatalf("replacement launches=%d launch=%#v binding=%#v", launchCount, latestLaunch, latestBinding)
	}
}

func TestCodexProjectBindingRecoversAfterExternalDeletion(t *testing.T) {
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
	fake := &fakeCodexRuntime{createTombstone: true}
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{
		store: store, catalog: catalog, runtimes: newRuntimeRegistry(nativeRuntimeAdapter{}, fake),
	})
	if _, inserted, err := catalog.BeginRuntimeWorkspaceBinding(ctx, workspace.ID, "codex", "pre-crash-intent", workspace.Title, filepath.Join(workspace.RootPath, "code")); err != nil || !inserted {
		t.Fatalf("seed interrupted create: inserted=%v err=%v", inserted, err)
	}

	created := requestCatalog(t, handler, http.MethodPost, "/sessions",
		`{"workspace_id":`+quotedJSON(t, workspace.ID)+`,"agent":"codex","model":"gpt-5.6-luna","reasoning_effort":"medium"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("first create status = %d: %s", created.Code, created.Body.String())
	}
	first, err := catalog.GetRuntimeWorkspaceBinding(ctx, workspace.ID, "codex")
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.workspace = nil
	fake.missing = true
	fake.mu.Unlock()
	created = requestCatalog(t, handler, http.MethodPost, "/sessions",
		`{"workspace_id":`+quotedJSON(t, workspace.ID)+`,"agent":"codex","model":"gpt-5.6-luna","reasoning_effort":"medium"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("recovery create status = %d: %s", created.Code, created.Body.String())
	}
	recovered, err := catalog.GetRuntimeWorkspaceBinding(ctx, workspace.ID, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ExternalID == first.ExternalID || recovered.Revision <= first.Revision {
		t.Fatalf("recovered binding = %#v, first=%#v", recovered, first)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.createCount != 3 {
		t.Fatalf("create count = %d, want 3", fake.createCount)
	}
}
