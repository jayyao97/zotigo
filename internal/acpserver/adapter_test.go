package acpserver

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/acp"
	"github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/testutil/servicetest"
	"github.com/jayyao97/zotigo/internal/zotigod"
	"github.com/jayyao97/zotigo/internal/zotigodclient"
	"go.lsp.dev/jsonrpc2"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == zotigod.WorkerSubcommand {
		os.Exit(zotigod.Run(os.Args[2:]))
	}
	os.Exit(m.Run())
}

type pipeRWC struct {
	io.Reader
	io.Writer
	closeFn func() error
}

func (p *pipeRWC) Close() error { return p.closeFn() }

// testClient is a minimal ACP client: it records session/update
// notifications and answers permission requests with a configurable option.
type testClient struct {
	conn jsonrpc2.Conn
	done chan struct{} // closed after the adapter shut down

	mu         sync.Mutex
	updates    []map[string]any
	permission string // option id to select, or "cancelled"
	asked      []string
	// beforeAnswer runs before a permission answer is sent.
	beforeAnswer func(params acp.RequestPermissionParams)
}

func (c *testClient) handle(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
	switch req.Method() {
	case acp.MethodSessionUpdate:
		var params struct {
			Update map[string]any `json:"update"`
		}
		_ = json.Unmarshal(req.Params(), &params)
		c.mu.Lock()
		c.updates = append(c.updates, params.Update)
		c.mu.Unlock()
		return nil
	case acp.MethodRequestPerm:
		var params acp.RequestPermissionParams
		_ = json.Unmarshal(req.Params(), &params)
		c.mu.Lock()
		c.asked = append(c.asked, params.ToolCall.ToolCallID)
		choice := c.permission
		before := c.beforeAnswer
		c.mu.Unlock()
		if before != nil {
			before(params)
		}
		if choice == "block" {
			// Never answer: the editor is about to disconnect.
			return nil
		}
		outcome := acp.PermissionOutcome{Outcome: "selected", OptionID: choice}
		if choice == "cancelled" {
			outcome = acp.PermissionOutcome{Outcome: "cancelled"}
		}
		return reply(ctx, acp.RequestPermissionResult{Outcome: outcome}, nil)
	default:
		return reply(ctx, nil, jsonrpc2.ErrMethodNotFound)
	}
}

func (c *testClient) text(kind string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out strings.Builder
	for _, update := range c.updates {
		if update["sessionUpdate"] != kind {
			continue
		}
		if content, ok := update["content"].(map[string]any); ok {
			text, _ := content["text"].(string)
			out.WriteString(text)
		}
	}
	return out.String()
}

func (c *testClient) sawUpdate(kind string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, update := range c.updates {
		if update["sessionUpdate"] == kind {
			return true
		}
	}
	return false
}

func (c *testClient) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updates = nil
	c.asked = nil
}

// startACP connects an adapter for service to a fresh in-memory client.
func startACP(t *testing.T, service *zotigodclient.Connection, workspaceID string) *testClient {
	t.Helper()
	return startACPMode(t, service, workspaceID, service.Embedded)
}

// startACPMode lets a test drive the adapter's daemon-mode semantics
// (embedded=false) against the in-process test service.
func startACPMode(t *testing.T, service *zotigodclient.Connection, workspaceID string, embedded bool) *testClient {
	t.Helper()
	return startACPClient(t, service.Client, workspaceID, embedded)
}

func startACPClient(t *testing.T, client *zotigodclient.Client, workspaceID string, embedded bool) *testClient {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	adapter, err := newAdapter(ctx, client, embedded, workspaceID, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	serverRead, clientWrite := io.Pipe()
	clientRead, serverWrite := io.Pipe()
	server := adapter.newServer(&pipeRWC{Reader: serverRead, Writer: serverWrite, closeFn: func() error { _ = serverRead.Close(); return serverWrite.Close() }})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = server.Run(ctx)
		adapter.shutdown()
	}()
	editor := &testClient{permission: "allow_once", done: done}
	editor.conn = jsonrpc2.NewConn(jsonrpc2.NewRawStream(&pipeRWC{Reader: clientRead, Writer: clientWrite, closeFn: func() error { _ = clientRead.Close(); return clientWrite.Close() }}))
	editor.conn.Go(ctx, editor.handle)
	t.Cleanup(func() {
		cancel()
		_ = editor.conn.Close()
		<-done
	})
	var init acp.InitializeResult
	if _, err := editor.conn.Call(ctx, acp.MethodInitialize, acp.InitializeParams{ProtocolVersion: 1}, &init); err != nil {
		t.Fatal(err)
	}
	if !init.AgentCapabilities.LoadSession {
		t.Fatal("adapter must advertise session/load")
	}
	return editor
}

func (c *testClient) newSession(t *testing.T, cwd string, mcp []acp.MCPServer) (string, error) {
	t.Helper()
	result, err := c.newSessionResult(cwd, mcp)
	return result.SessionID, err
}

func (c *testClient) newSessionResult(cwd string, mcp []acp.MCPServer) (acp.SessionNewResult, error) {
	var result acp.SessionNewResult
	_, err := c.conn.Call(context.Background(), acp.MethodSessionNew, acp.SessionNewParams{Cwd: cwd, MCPServers: mcp}, &result)
	return result, err
}

// disconnect closes the editor side and waits for the adapter to shut down.
func (c *testClient) disconnect(t *testing.T) {
	t.Helper()
	_ = c.conn.Close()
	select {
	case <-c.done:
	case <-time.After(20 * time.Second):
		t.Fatal("adapter did not shut down after disconnect")
	}
}

func (c *testClient) waitAsked(t *testing.T, count int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		c.mu.Lock()
		asked := len(c.asked)
		c.mu.Unlock()
		if asked >= count {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("permission was asked %d times, want %d", asked, count)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (c *testClient) prompt(t *testing.T, sessionID string, text string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var result acp.SessionPromptResult
	if _, err := c.conn.Call(ctx, acp.MethodSessionPrompt, acp.SessionPromptParams{
		SessionID: sessionID,
		Prompt:    []acp.ContentBlock{{Type: "text", Text: text}},
	}, &result); err != nil {
		t.Fatalf("prompt %q: %v", text, err)
	}
	return result.StopReason
}

func connectService(t *testing.T, fixture *servicetest.Fixture) *zotigodclient.Connection {
	t.Helper()
	conn, err := zotigodclient.Connect(context.Background(), zotigodclient.ConnectOptions{NoDaemon: true, StateDir: fixture.StateDir, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = conn.Close(ctx)
	})
	return conn
}

func TestACPPromptsApprovalsCancelAndLoad(t *testing.T) {
	fixture := servicetest.New(t)
	service := connectService(t, fixture)
	cwd := t.TempDir()
	client := startACP(t, service, "")

	if _, err := client.newSession(t, "relative/dir", nil); err == nil {
		t.Fatal("relative cwd must be rejected")
	}
	sessionID, err := client.newSession(t, cwd, []acp.MCPServer{{Name: "docs", Command: "docs-mcp"}})
	if err != nil {
		t.Fatal(err)
	}

	if stop := client.prompt(t, sessionID, "hello"); stop != acp.StopReasonEndTurn {
		t.Fatalf("stop = %q", stop)
	}
	agentText := client.text("agent_message_chunk")
	if strings.Count(agentText, "hi there") != 1 {
		t.Fatalf("agent text = %q (want exactly one reply, no duplicates)", agentText)
	}
	if !strings.Contains(agentText, "MCP") || !strings.Contains(agentText, "docs") {
		t.Fatalf("MCP configuration was dropped silently: %q", agentText)
	}

	client.reset()
	if stop := client.prompt(t, sessionID, "approve-touch"); stop != acp.StopReasonEndTurn {
		t.Fatalf("approval turn stop = %q", stop)
	}
	if len(client.asked) != 1 || !client.sawUpdate("tool_call") || !client.sawUpdate("tool_call_update") {
		t.Fatalf("asked=%v updates=%v", client.asked, client.updates)
	}
	if _, err := os.Stat(filepath.Join(cwd, "created-by-agent.txt")); err != nil {
		t.Fatalf("approved tool did not run in the session cwd: %v", err)
	}

	// Cancelling the permission prompt denies the call and ends the turn.
	client.reset()
	client.permission = "cancelled"
	_ = os.Remove(filepath.Join(cwd, "created-by-agent.txt"))
	if stop := client.prompt(t, sessionID, "approve-touch"); stop != acp.StopReasonCancelled {
		t.Fatalf("cancelled approval stop = %q", stop)
	}
	if _, err := os.Stat(filepath.Join(cwd, "created-by-agent.txt")); !os.IsNotExist(err) {
		t.Fatalf("cancelled approval executed the tool: %v", err)
	}

	// session/cancel interrupts a streaming turn.
	client.reset()
	stopCh := make(chan string, 1)
	go func() { stopCh <- client.prompt(t, sessionID, "slow") }()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(client.text("agent_message_chunk"), "chunk") {
		if time.Now().After(deadline) {
			t.Fatal("slow turn never streamed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := client.conn.Notify(context.Background(), acp.MethodSessionCancel, acp.SessionCancelParams{SessionID: sessionID}); err != nil {
		t.Fatal(err)
	}
	select {
	case stop := <-stopCh:
		if stop != acp.StopReasonCancelled {
			t.Fatalf("cancel stop = %q", stop)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("cancelled prompt did not return")
	}

	// A second editor connection reattaches with session/load.
	other := startACP(t, service, "")
	if _, err := other.conn.Call(context.Background(), acp.MethodSessionLoad, acp.SessionLoadParams{SessionID: sessionID, Cwd: t.TempDir()}, nil); err == nil {
		t.Fatal("loading with a different cwd must fail instead of switching directories")
	}
	if _, err := other.conn.Call(context.Background(), acp.MethodSessionLoad, acp.SessionLoadParams{SessionID: sessionID, Cwd: cwd}, nil); err != nil {
		t.Fatal(err)
	}
	if user := other.text("user_message_chunk"); !strings.Contains(user, "hello") || !strings.Contains(user, "approve-touch") {
		t.Fatalf("replayed user messages = %q", user)
	}
	if !strings.Contains(other.text("agent_message_chunk"), "hi there") {
		t.Fatalf("replayed agent text = %q", other.text("agent_message_chunk"))
	}
	if stop := other.prompt(t, sessionID, "continue"); stop != acp.StopReasonEndTurn {
		t.Fatalf("loaded session prompt stop = %q", stop)
	}
	// The first connection sees the other editor's input as a user message.
	deadline = time.Now().Add(10 * time.Second)
	for !strings.Contains(client.text("user_message_chunk"), "continue") {
		if time.Now().After(deadline) {
			t.Fatalf("first client did not see foreign input: %q", client.text("user_message_chunk"))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestACPWorkspaceBinding(t *testing.T) {
	fixture := servicetest.New(t)
	service := connectService(t, fixture)
	backend := filepath.Join(fixture.Workspace.RootPath, "code", "backend")
	if err := os.MkdirAll(backend, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := newAdapter(context.Background(), service.Client, true, "workspace_missing", log.New(io.Discard, "", 0)); err == nil {
		t.Fatal("unknown --workspace must fail at startup")
	}
	client := startACP(t, service, fixture.Workspace.ID)
	if _, err := client.newSession(t, t.TempDir(), nil); err == nil {
		t.Fatal("cwd outside the bound workspace must be rejected")
	}
	sessionID, err := client.newSession(t, backend, nil)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := service.CatalogSession(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.WorkspaceID() != fixture.Workspace.ID || projection.Runtime.WorkingDirectory != backend {
		t.Fatalf("ownership=%q cwd=%q", projection.WorkspaceID(), projection.Runtime.WorkingDirectory)
	}

	// A directory session (unbound) cannot be loaded through a
	// workspace-bound adapter.
	plain := t.TempDir()
	unbound, err := service.CreateSession(context.Background(), zotigodclient.CreateSessionRequest{WorkingDirectory: plain})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.conn.Call(context.Background(), acp.MethodSessionLoad, acp.SessionLoadParams{SessionID: unbound.ID, Cwd: plain}, nil); err == nil {
		t.Fatal("workspace-bound adapter loaded a session from outside its workspace")
	}
}

func waitTurnEnd(t *testing.T, service *zotigodclient.Connection, sessionID string, turnID string) session.DisplayItem {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		items, err := service.AllItems(context.Background(), sessionID)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			switch item.Type {
			case session.DisplayItemTurnCompleted, session.DisplayItemTurnFailed, session.DisplayItemTurnInterrupted:
				if item.Turn != nil && (turnID == "" || item.Turn.ID == turnID) {
					return item
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn %q did not end", turnID)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func lastTurnID(t *testing.T, service *zotigodclient.Connection, sessionID string) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		items, err := service.AllItems(context.Background(), sessionID)
		if err != nil {
			t.Fatal(err)
		}
		for index := len(items) - 1; index >= 0; index-- {
			if items[index].Type == session.DisplayItemTurnStarted && items[index].Turn != nil {
				return items[index].Turn.ID
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no turn started")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestACPModesMapToApprovalPolicy(t *testing.T) {
	fixture := servicetest.New(t)
	service := connectService(t, fixture)
	client := startACP(t, service, "")
	result, err := client.newSessionResult(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Modes == nil || result.Modes.CurrentModeID != "manual" || len(result.Modes.AvailableModes) != 3 {
		t.Fatalf("modes = %+v", result.Modes)
	}
	stored, err := service.GetSession(context.Background(), result.SessionID)
	if err != nil || stored.ApprovalPolicy != "manual" {
		t.Fatalf("new ACP session policy = %q err=%v", stored.ApprovalPolicy, err)
	}
	// Start the worker so policy changes are recorded in the session stream.
	if stop := client.prompt(t, result.SessionID, "hello"); stop != acp.StopReasonEndTurn {
		t.Fatalf("stop = %q", stop)
	}
	if _, err := client.conn.Call(context.Background(), acp.MethodSessionSetMode, acp.SessionSetModeParams{SessionID: result.SessionID, ModeID: "yolo"}, nil); err == nil {
		t.Fatal("unknown mode must be rejected")
	}
	if _, err := client.conn.Call(context.Background(), acp.MethodSessionSetMode, acp.SessionSetModeParams{SessionID: result.SessionID, ModeID: "auto"}, nil); err != nil {
		t.Fatal(err)
	}
	// A change from another client is reported as the current mode.
	if err := service.SetApprovalPolicy(context.Background(), result.SessionID, "bypass_permissions"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		client.mu.Lock()
		var modes []string
		for _, update := range client.updates {
			if update["sessionUpdate"] == "current_mode_update" {
				modes = append(modes, update["currentModeId"].(string))
			}
		}
		client.mu.Unlock()
		if strings.Join(modes, ",") == "auto,bypass_permissions" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("mode updates = %v", modes)
		}
		time.Sleep(20 * time.Millisecond)
	}
	other := startACP(t, service, "")
	var loaded acp.SessionLoadResult
	if _, err := other.conn.Call(context.Background(), acp.MethodSessionLoad, acp.SessionLoadParams{SessionID: result.SessionID, Cwd: stored.WorkingDirectory}, &loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.Modes == nil || loaded.Modes.CurrentModeID != "bypass_permissions" {
		t.Fatalf("loaded modes = %+v", loaded.Modes)
	}
}

// With the shared daemon, an editor disconnect stops only the turns its
// own prompts were waiting on; other clients' turns and approvals are left
// to those clients.
func TestACPDisconnectStopsOnlyOwnTurns(t *testing.T) {
	fixture := servicetest.New(t)
	service := connectService(t, fixture)
	ctx := context.Background()

	t.Run("streaming prompt", func(t *testing.T) {
		client := startACPMode(t, service, "", false)
		sessionID, err := client.newSession(t, t.TempDir(), nil)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			var result acp.SessionPromptResult
			_, _ = client.conn.Call(ctx, acp.MethodSessionPrompt, acp.SessionPromptParams{SessionID: sessionID, Prompt: []acp.ContentBlock{{Type: "text", Text: "slow"}}}, &result)
		}()
		turnID := lastTurnID(t, service, sessionID)
		client.disconnect(t)
		if end := waitTurnEnd(t, service, sessionID, turnID); end.Type != session.DisplayItemTurnInterrupted {
			t.Fatalf("own turn ended with %s", end.Type)
		}
	})

	t.Run("pending approval", func(t *testing.T) {
		client := startACPMode(t, service, "", false)
		client.permission = "block"
		cwd := t.TempDir()
		sessionID, err := client.newSession(t, cwd, nil)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			var result acp.SessionPromptResult
			_, _ = client.conn.Call(ctx, acp.MethodSessionPrompt, acp.SessionPromptParams{SessionID: sessionID, Prompt: []acp.ContentBlock{{Type: "text", Text: "approve-touch"}}}, &result)
		}()
		client.waitAsked(t, 1)
		turnID := lastTurnID(t, service, sessionID)
		client.disconnect(t)
		if end := waitTurnEnd(t, service, sessionID, turnID); end.Type != session.DisplayItemTurnInterrupted {
			t.Fatalf("own approval turn ended with %s", end.Type)
		}
		if _, err := os.Stat(filepath.Join(cwd, "created-by-agent.txt")); !os.IsNotExist(err) {
			t.Fatalf("tool ran after disconnect: %v", err)
		}
	})

	t.Run("other client's turn", func(t *testing.T) {
		cwd := t.TempDir()
		created, err := service.CreateSession(ctx, zotigodclient.CreateSessionRequest{WorkingDirectory: cwd, ApprovalPolicy: "manual"})
		if err != nil {
			t.Fatal(err)
		}
		client := startACPMode(t, service, "", false)
		client.permission = "block"
		if _, err := client.conn.Call(ctx, acp.MethodSessionLoad, acp.SessionLoadParams{SessionID: created.ID, Cwd: cwd}, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := service.SubmitMessage(ctx, created.ID, zotigodclient.MessageRequest{Text: "approve-touch"}); err != nil {
			t.Fatal(err)
		}
		client.waitAsked(t, 1)
		client.disconnect(t)
		// The approval is still pending for the client that started the turn.
		items, err := service.AllItems(ctx, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		var request *session.DisplayApproval
		for _, item := range items {
			switch item.Type {
			case session.DisplayItemApprovalRequest:
				request = item.Approval
			case session.DisplayItemApprovalDecision, session.DisplayItemTurnInterrupted:
				t.Fatalf("disconnect resolved another client's turn: %s", item.Type)
			}
		}
		if request == nil {
			t.Fatal("approval request missing")
		}
		if err := service.DecideApproval(ctx, created.ID, request.ID, []zotigodclient.ApprovalDecision{{ToolCallID: request.Pending[0].ToolCallID, Approved: true}}, false); err != nil {
			t.Fatal(err)
		}
		if end := waitTurnEnd(t, service, created.ID, request.TurnID); end.Type != session.DisplayItemTurnCompleted {
			t.Fatalf("other client's turn ended with %s", end.Type)
		}
		if _, err := os.Stat(filepath.Join(cwd, "created-by-agent.txt")); err != nil {
			t.Fatalf("approved tool did not run: %v", err)
		}
	})
}

// The editor's event stream lags the service; cancel and disconnect must
// still stop the turn the prompt started.
func TestACPStopsTurnBeforeStreamReportsIt(t *testing.T) {
	fixture := servicetest.New(t)
	service := connectService(t, fixture)
	lagging := service.Via(servicetest.DelayEvents(t, service.BaseURL(), 3*time.Second))
	ctx := context.Background()

	t.Run("session/cancel", func(t *testing.T) {
		client := startACPClient(t, lagging, "", false)
		sessionID, err := client.newSession(t, t.TempDir(), nil)
		if err != nil {
			t.Fatal(err)
		}
		stopCh := make(chan string, 1)
		go func() { stopCh <- client.prompt(t, sessionID, "slow") }()
		lastTurnID(t, service, sessionID)
		if err := client.conn.Notify(ctx, acp.MethodSessionCancel, acp.SessionCancelParams{SessionID: sessionID}); err != nil {
			t.Fatal(err)
		}
		if end := waitTurnEnd(t, service, sessionID, ""); end.Type != session.DisplayItemTurnInterrupted {
			t.Fatalf("turn ended with %s", end.Type)
		}
		select {
		case stop := <-stopCh:
			if stop != acp.StopReasonCancelled {
				t.Fatalf("stop = %q", stop)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("prompt did not return")
		}
	})

	t.Run("disconnect", func(t *testing.T) {
		client := startACPClient(t, lagging, "", false)
		sessionID, err := client.newSession(t, t.TempDir(), nil)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			var result acp.SessionPromptResult
			_, _ = client.conn.Call(ctx, acp.MethodSessionPrompt, acp.SessionPromptParams{SessionID: sessionID, Prompt: []acp.ContentBlock{{Type: "text", Text: "slow"}}}, &result)
		}()
		lastTurnID(t, service, sessionID)
		client.disconnect(t)
		if end := waitTurnEnd(t, service, sessionID, ""); end.Type != session.DisplayItemTurnInterrupted {
			t.Fatalf("turn ended with %s", end.Type)
		}
	})
}

// Cancelling a permission request that another client approved first must
// still stop the turn: the 409 means "resolved", not "stopped".
func TestACPCancelAfterApprovalResolvedElsewhereStopsTurn(t *testing.T) {
	fixture := servicetest.New(t)
	service := connectService(t, fixture)
	client := startACP(t, service, "")
	cwd := t.TempDir()
	sessionID, err := client.newSession(t, cwd, nil)
	if err != nil {
		t.Fatal(err)
	}
	client.permission = "cancelled"
	client.beforeAnswer = func(acp.RequestPermissionParams) {
		items, err := service.AllItems(context.Background(), sessionID)
		if err != nil {
			t.Error(err)
			return
		}
		for index := len(items) - 1; index >= 0; index-- {
			if items[index].Type == session.DisplayItemApprovalRequest {
				approval := items[index].Approval
				if err := service.DecideApproval(context.Background(), sessionID, approval.ID, []zotigodclient.ApprovalDecision{{ToolCallID: approval.Pending[0].ToolCallID, Approved: true}}, false); err != nil {
					t.Error(err)
				}
				return
			}
		}
	}
	if stop := client.prompt(t, sessionID, "approve-touch-then-slow"); stop != acp.StopReasonCancelled {
		t.Fatalf("stop = %q", stop)
	}
	if end := waitTurnEnd(t, service, sessionID, ""); end.Type != session.DisplayItemTurnInterrupted {
		t.Fatalf("approved turn ended with %s", end.Type)
	}
}

// Review round 3: the submission's response is lost and the stream lags.
// The prompt fails, but the input stays this connection's: disconnecting
// still stops the turn it started.
func TestACPLostSubmitResponseThenDisconnectStopsTurn(t *testing.T) {
	fixture := servicetest.New(t)
	service := connectService(t, fixture)
	lossy := service.Via(servicetest.Proxy(t, service.BaseURL(), servicetest.ProxyOptions{EventDelay: 8 * time.Second, DropMessageResponses: true}))
	client := startACPClient(t, lossy, "", false)
	sessionID, err := client.newSession(t, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	client.prompt(t, sessionID, "slow")
	if text := client.text("agent_message_chunk"); !strings.Contains(text, "may still be running") {
		t.Fatalf("prompt did not report the unconfirmed submission: %q", text)
	}
	client.disconnect(t)
	if end := waitTurnEnd(t, service, sessionID, ""); end.Type != session.DisplayItemTurnInterrupted {
		t.Fatalf("turn ended with %s", end.Type)
	}
}
