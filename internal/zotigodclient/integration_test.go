package zotigodclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/testutil/servicetest"
	"github.com/jayyao97/zotigo/internal/zotigod"
)

// TestMain lets the embedded service launch session workers from this test
// binary, exactly as zotigo --no-daemon launches them from zotigo.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == zotigod.WorkerSubcommand {
		os.Exit(zotigod.Run(os.Args[2:]))
	}
	os.Exit(m.Run())
}

func connectEmbedded(t *testing.T, stateDir string) *Connection {
	t.Helper()
	conn, err := Connect(context.Background(), ConnectOptions{NoDaemon: true, StateDir: stateDir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeConnection(conn) })
	return conn
}

func closeConnection(conn *Connection) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = conn.Close(ctx)
}

// waitItem consumes events until match returns true.
func waitItem(t *testing.T, events <-chan Event, match func(session.DisplayItem) bool) session.DisplayItem {
	t.Helper()
	timeout := time.After(30 * time.Second)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatal("event stream closed")
			}
			if event.Err != nil && !event.Reconnecting {
				t.Fatalf("event stream failed: %v", event.Err)
			}
			if event.Item != nil && match(*event.Item) {
				return *event.Item
			}
		case <-timeout:
			t.Fatal("timed out waiting for session item")
		}
	}
}

func ofType(itemType session.DisplayItemType) func(session.DisplayItem) bool {
	return func(item session.DisplayItem) bool { return item.Type == itemType }
}

func lastSequence(t *testing.T, client *Client, id string) uint64 {
	t.Helper()
	page, err := client.Items(context.Background(), id, 0, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) == 0 {
		return 0
	}
	return page.Items[len(page.Items)-1].Sequence
}

func assistantText(items []session.DisplayItem) string {
	var text strings.Builder
	for _, item := range items {
		if item.Type != session.DisplayItemAssistantMessage {
			continue
		}
		for _, part := range item.Content {
			if part.Type == "text" {
				text.WriteString(part.Text)
			}
		}
	}
	return text.String()
}

func TestEmbeddedServiceRunsDirectorySessionAcrossRestart(t *testing.T) {
	fixture := servicetest.New(t)
	plain := t.TempDir()
	ctx := context.Background()

	conn := connectEmbedded(t, fixture.StateDir)
	created, err := conn.CreateSession(ctx, CreateSessionRequest{WorkingDirectory: plain})
	if err != nil {
		t.Fatal(err)
	}
	if created.WorkingDirectory != plain {
		t.Fatalf("cwd = %q", created.WorkingDirectory)
	}
	projection, err := conn.CatalogSession(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.WorkspaceID() != "" {
		t.Fatalf("plain directory was bound to %q", projection.WorkspaceID())
	}

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := conn.Subscribe(streamCtx, created.ID, 0, StreamOptions{})
	request := MessageRequest{Text: "hello", ClientMessageID: "client-1"}
	first, err := conn.SubmitMessage(ctx, created.ID, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Type != "message" || first.ID != "client-1" {
		t.Fatalf("first command = %+v", first)
	}
	// A retried submission with the same client id is deduplicated.
	again, err := conn.SubmitMessage(ctx, created.ID, request)
	if err != nil || again.ID != first.ID || again.Sequence != first.Sequence {
		t.Fatalf("retried command = %+v err=%v", again, err)
	}
	waitItem(t, events, ofType(session.DisplayItemTurnCompleted))

	if _, err := conn.SubmitMessage(ctx, created.ID, MessageRequest{Text: "second turn"}); err != nil {
		t.Fatal(err)
	}
	waitItem(t, events, ofType(session.DisplayItemTurnCompleted))
	if !fixture.Provider.LastRequestContains("hi there") {
		t.Fatal("second turn did not include the first turn's history")
	}
	cancel()

	items, err := conn.AllItems(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	userMessages := 0
	for _, item := range items {
		if item.Type == session.DisplayItemUserMessage {
			userMessages++
		}
	}
	if userMessages != 2 || strings.Count(assistantText(items), "hi there") != 2 {
		t.Fatalf("history users=%d assistant=%q", userMessages, assistantText(items))
	}

	// Restart the in-process service: history and runtime state recover from
	// the shared store and the next turn continues the same conversation.
	closeConnection(conn)
	restarted := connectEmbedded(t, fixture.StateDir)
	stored, err := restarted.GetSession(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Live || stored.WorkingDirectory != plain {
		t.Fatalf("restored session = %+v", stored)
	}
	cursor := lastSequence(t, restarted.Client, created.ID)
	resumeCtx, resumeCancel := context.WithCancel(ctx)
	defer resumeCancel()
	resumed := restarted.Subscribe(resumeCtx, created.ID, cursor, StreamOptions{})
	if _, err := restarted.SubmitMessage(ctx, created.ID, MessageRequest{Text: "after restart"}); err != nil {
		t.Fatal(err)
	}
	user := waitItem(t, resumed, ofType(session.DisplayItemUserMessage))
	if user.Sequence <= cursor {
		t.Fatalf("stream replayed an old item: %d <= %d", user.Sequence, cursor)
	}
	waitItem(t, resumed, ofType(session.DisplayItemTurnCompleted))
	if !fixture.Provider.LastRequestContains("second turn") {
		t.Fatal("resumed turn lost earlier history")
	}
}

func TestEmbeddedServiceWorkspaceSubdirectoryApprovalAndPause(t *testing.T) {
	fixture := servicetest.New(t)
	backend := filepath.Join(fixture.Workspace.RootPath, "code", "backend")
	if err := os.MkdirAll(backend, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	conn := connectEmbedded(t, fixture.StateDir)

	created, err := conn.CreateSession(ctx, CreateSessionRequest{WorkingDirectory: backend})
	if err != nil {
		t.Fatal(err)
	}
	projection, err := conn.CatalogSession(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.WorkspaceID() != fixture.Workspace.ID || created.WorkingDirectory != backend {
		t.Fatalf("ownership=%q cwd=%q", projection.WorkspaceID(), created.WorkingDirectory)
	}

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	events := conn.Subscribe(streamCtx, created.ID, 0, StreamOptions{})

	// The worker executes in the session cwd, not the Workspace root.
	if _, err := conn.SubmitMessage(ctx, created.ID, MessageRequest{Text: "where-am-i"}); err != nil {
		t.Fatal(err)
	}
	result := waitItem(t, events, func(item session.DisplayItem) bool {
		return item.Type == session.DisplayItemAssistantMessage && len(item.Content) > 0 && item.Content[0].ToolResult != nil
	})
	if !strings.Contains(result.Content[0].ToolResult.Text, backend) {
		t.Fatalf("pwd result = %q, want %s", result.Content[0].ToolResult.Text, backend)
	}
	waitItem(t, events, ofType(session.DisplayItemTurnCompleted))

	// A mutating command needs approval; exactly one decision is accepted.
	if _, err := conn.SubmitMessage(ctx, created.ID, MessageRequest{Text: "approve-touch"}); err != nil {
		t.Fatal(err)
	}
	request := waitItem(t, events, ofType(session.DisplayItemApprovalRequest))
	if request.Approval == nil || len(request.Approval.Pending) != 1 {
		t.Fatalf("approval = %+v", request.Approval)
	}
	decision := []ApprovalDecision{{ToolCallID: request.Approval.Pending[0].ToolCallID, Approved: true}}
	if err := conn.DecideApproval(ctx, created.ID, request.Approval.ID, decision, false); err != nil {
		t.Fatal(err)
	}
	second := New(conn.BaseURL(), conn.token)
	if err := second.DecideApproval(ctx, created.ID, request.Approval.ID, decision, false); !IsStatus(err, http.StatusConflict) {
		t.Fatalf("second decision err = %v, want conflict", err)
	}
	waitItem(t, events, ofType(session.DisplayItemTurnCompleted))
	if _, err := os.Stat(filepath.Join(backend, "created-by-agent.txt")); err != nil {
		t.Fatalf("approved command did not run in session cwd: %v", err)
	}

	// Denying with stop_turn ends the turn without another model call.
	if err := os.Remove(filepath.Join(backend, "created-by-agent.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.SubmitMessage(ctx, created.ID, MessageRequest{Text: "approve-touch"}); err != nil {
		t.Fatal(err)
	}
	stopRequest := waitItem(t, events, ofType(session.DisplayItemApprovalRequest))
	callID := stopRequest.Approval.Pending[0].ToolCallID
	if err := conn.DecideApproval(ctx, created.ID, stopRequest.Approval.ID, []ApprovalDecision{{ToolCallID: callID, Approved: true}}, true); !IsStatus(err, http.StatusBadRequest) {
		t.Fatalf("stop_turn with an approval err = %v, want 400", err)
	}
	requestsBefore := fixture.Provider.RequestCount()
	if err := conn.DecideApproval(ctx, created.ID, stopRequest.Approval.ID, []ApprovalDecision{{ToolCallID: callID, Reason: "stop"}}, true); err != nil {
		t.Fatal(err)
	}
	stopped := waitItem(t, events, func(item session.DisplayItem) bool {
		return item.Type == session.DisplayItemTurnInterrupted || item.Type == session.DisplayItemTurnCompleted
	})
	if stopped.Type != session.DisplayItemTurnInterrupted {
		t.Fatalf("stop_turn ended turn with %s", stopped.Type)
	}
	if _, err := os.Stat(filepath.Join(backend, "created-by-agent.txt")); !os.IsNotExist(err) {
		t.Fatalf("denied tool ran: %v", err)
	}
	// The interrupt follows the release in the same worker step; at most an
	// immediately cancelled model request can start in between.
	if got := fixture.Provider.RequestCount(); got > requestsBefore+1 {
		t.Fatalf("agent continued after stop_turn: %d model requests, want <= %d", got, requestsBefore+1)
	}

	// Pause interrupts the running turn.
	if _, err := conn.SubmitMessage(ctx, created.ID, MessageRequest{Text: "slow"}); err != nil {
		t.Fatal(err)
	}
	started := waitItem(t, events, ofType(session.DisplayItemTurnStarted))
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := conn.Pause(ctx, created.ID, started.Turn.ID); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("pause: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	interrupted := waitItem(t, events, func(item session.DisplayItem) bool {
		return item.Type == session.DisplayItemTurnInterrupted || item.Type == session.DisplayItemTurnCompleted
	})
	if interrupted.Type != session.DisplayItemTurnInterrupted || interrupted.Turn.ID != started.Turn.ID {
		t.Fatalf("pause ended turn with %s %+v", interrupted.Type, interrupted.Turn)
	}
	// The interrupted model stream is cancelled, so the next input starts
	// promptly instead of waiting for the old stream to drain.
	began := time.Now()
	if _, err := conn.SubmitMessage(ctx, created.ID, MessageRequest{Text: "after pause"}); err != nil {
		t.Fatal(err)
	}
	waitItem(t, events, ofType(session.DisplayItemTurnCompleted))
	if elapsed := time.Since(began); elapsed > 5*time.Second {
		t.Fatalf("turn after pause took %s", elapsed)
	}
}

func TestEmbeddedServiceRejectsSecondCatalogOwner(t *testing.T) {
	fixture := servicetest.New(t)
	connectEmbedded(t, fixture.StateDir)
	if _, err := Connect(context.Background(), ConnectOptions{NoDaemon: true, StateDir: fixture.StateDir}); err == nil || !strings.Contains(err.Error(), "catalog") {
		t.Fatalf("second embedded service err = %v", err)
	}
}

func TestConnectDaemonFailsWithoutSilentFallback(t *testing.T) {
	t.Setenv("ZOTIGOD_URL", "http://127.0.0.1:1")
	t.Setenv("ZOTIGOD_BINARY", filepath.Join(t.TempDir(), "missing-zotigod"))
	_, err := Connect(context.Background(), ConnectOptions{})
	if err == nil || !strings.Contains(err.Error(), "--no-daemon") {
		t.Fatalf("err = %v, want failure with --no-daemon hint", err)
	}
}

func TestConnectUsesRunningDaemon(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" || r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "unexpected", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"code":"ok","data":{"status":"ok","protocol_version":"1"}}`))
	}))
	defer server.Close()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZOTIGOD_URL", server.URL)
	t.Setenv("ZOTIGOD_AUTH_TOKEN_FILE", tokenFile)
	conn, err := Connect(context.Background(), ConnectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if conn.Embedded || conn.BaseURL() != server.URL {
		t.Fatalf("connection = %+v", conn)
	}
}

// Sessions written by the previous in-process CLI (session.Manager plus a
// TUI-written display log, no profile or policy metadata) resume through the
// service with their history intact.
func TestEmbeddedServiceResumesLegacyCLISession(t *testing.T) {
	fixture := servicetest.New(t)
	dir := t.TempDir()
	// The legacy CLI wrote to the same store the service now serves.
	t.Setenv(config.StateDirEnv, fixture.StateDir)
	manager, err := session.NewManager()
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := manager.CreateNew(dir)
	if err != nil {
		t.Fatal(err)
	}
	legacy.AgentSnapshot.History = []protocol.Message{
		protocol.NewUserMessage("legacy question"),
		protocol.NewAssistantMessage("legacy answer"),
	}
	legacy.LastPrompt = "legacy question"
	if err := manager.Save(legacy); err != nil {
		t.Fatal(err)
	}
	for _, item := range []session.DisplayItem{
		{Type: session.DisplayItemTurnStarted, Turn: &session.DisplayTurn{ID: "turn_legacy"}},
		{Type: session.DisplayItemUserMessage, Role: "user", Content: []session.DisplayContentPart{{Type: "text", Text: "legacy question"}}},
		{Type: session.DisplayItemAssistantMessage, Role: "assistant", Content: []session.DisplayContentPart{{Type: "text", Text: "legacy answer"}}},
		{Type: session.DisplayItemTurnCompleted, Turn: &session.DisplayTurn{ID: "turn_legacy", Status: "completed"}},
	} {
		if _, err := manager.AppendDisplayItem(legacy.ID, item); err != nil {
			t.Fatal(err)
		}
	}
	_ = manager.Close()

	ctx := context.Background()
	conn := connectEmbedded(t, fixture.StateDir)
	listed, err := conn.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range listed {
		if entry.ID == legacy.ID {
			found = entry.WorkingDirectory == dir && entry.LastPrompt == "legacy question" && !entry.Live
		}
	}
	if !found {
		t.Fatalf("legacy session not listed correctly: %+v", listed)
	}
	items, err := conn.AllItems(ctx, legacy.ID)
	if err != nil || len(items) != 4 || assistantText(items) != "legacy answer" {
		t.Fatalf("legacy history = %+v err=%v", items, err)
	}
	events := conn.Subscribe(ctx, legacy.ID, items[len(items)-1].Sequence, StreamOptions{})
	if _, err := conn.SubmitMessage(ctx, legacy.ID, MessageRequest{Text: "continue legacy"}); err != nil {
		t.Fatal(err)
	}
	waitItem(t, events, ofType(session.DisplayItemTurnCompleted))
	if !fixture.Provider.LastRequestContains("legacy answer") {
		t.Fatal("resumed legacy session lost its agent history")
	}
}

// Two clients submitting to the same idle session at once never start two
// turns: the service admits one message and attaches or rejects the other.
func TestEmbeddedServiceConcurrentSubmissions(t *testing.T) {
	fixture := servicetest.New(t)
	ctx := context.Background()
	conn := connectEmbedded(t, fixture.StateDir)
	created, err := conn.CreateSession(ctx, CreateSessionRequest{WorkingDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	other := New(conn.BaseURL(), conn.token)
	events := conn.Subscribe(ctx, created.ID, 0, StreamOptions{})

	type outcome struct {
		command Command
		err     error
	}
	results := make(chan outcome, 2)
	for index, client := range []*Client{conn.Client, other} {
		go func(client *Client, text string) {
			command, err := client.SubmitMessage(ctx, created.ID, MessageRequest{Text: text, ClientMessageID: "concurrent-" + text})
			results <- outcome{command, err}
		}(client, fmt.Sprintf("hello %d", index))
	}
	messages := 0
	for range 2 {
		result := <-results
		if result.err != nil {
			var apiErr *APIError
			if !errors.As(result.err, &apiErr) || apiErr.Status != http.StatusConflict {
				t.Fatalf("unexpected submit error: %v", result.err)
			}
			continue
		}
		if result.command.Type == "message" {
			messages++
		}
	}
	if messages < 1 {
		t.Fatal("no submission started a turn")
	}
	waitItem(t, events, ofType(session.DisplayItemTurnCompleted))
	deadline := time.Now().Add(15 * time.Second)
	for {
		current, err := conn.GetSession(ctx, created.ID)
		if err == nil && !current.Working {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("session did not become idle")
		}
		time.Sleep(50 * time.Millisecond)
	}
	items, err := conn.AllItems(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	turns := map[string]bool{}
	open := 0
	for _, item := range items {
		switch item.Type {
		case session.DisplayItemUserMessage, session.DisplayItemSteeringMessage:
			seen[item.ID]++
		case session.DisplayItemTurnStarted:
			open++
			if open > 1 {
				t.Fatal("two turns were open at once")
			}
			turns[item.Turn.ID] = true
		case session.DisplayItemTurnCompleted, session.DisplayItemTurnFailed, session.DisplayItemTurnInterrupted:
			open--
		}
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("input %s recorded %d times", id, count)
		}
	}
	if len(turns) != messages {
		t.Fatalf("turns=%d messages=%d", len(turns), messages)
	}
}

func TestNoDaemonRequiresIsolatedStateDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(config.StateDirEnv, "")
	if _, err := Connect(context.Background(), ConnectOptions{NoDaemon: true}); err == nil || !strings.Contains(err.Error(), "--state-dir") {
		t.Fatalf("missing state dir err = %v", err)
	}
	if _, err := Connect(context.Background(), ConnectOptions{NoDaemon: true, StateDir: filepath.Join(home, ".zotigo")}); err == nil || !strings.Contains(err.Error(), "belongs to zotigod") {
		t.Fatalf("default state dir err = %v", err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.MkdirAll(filepath.Join(home, ".zotigo"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, ".zotigo"), link); err != nil {
		t.Fatal(err)
	}
	if _, err := Connect(context.Background(), ConnectOptions{NoDaemon: true, StateDir: link}); err == nil {
		t.Fatal("a symlink to ~/.zotigo must be rejected")
	}
	if _, err := Connect(context.Background(), ConnectOptions{StateDir: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "--no-daemon") {
		t.Fatalf("state dir without --no-daemon err = %v", err)
	}
}

// The embedded service, its workers and the catalog all live in the state
// dir; ~/.zotigo only provides configuration.
func TestNoDaemonKeepsStateOutOfDefaultDir(t *testing.T) {
	fixture := servicetest.New(t)
	ctx := context.Background()
	conn := connectEmbedded(t, fixture.StateDir)
	created, err := conn.CreateSession(ctx, CreateSessionRequest{WorkingDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	events := conn.Subscribe(ctx, created.ID, 0, StreamOptions{})
	if _, err := conn.SubmitMessage(ctx, created.ID, MessageRequest{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	waitItem(t, events, ofType(session.DisplayItemTurnCompleted))
	if _, err := os.Stat(filepath.Join(fixture.StateDir, "sessions", created.ID+".json")); err != nil {
		matches, _ := filepath.Glob(filepath.Join(fixture.StateDir, "sessions", created.ID+"*"))
		if len(matches) == 0 {
			t.Fatalf("session not stored in state dir: %v", err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(fixture.Home, ".zotigo"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != config.ConfigFileName {
			t.Fatalf("embedded service wrote %s into ~/.zotigo", entry.Name())
		}
	}
}

// stopRaceControl loses the approval race on purpose: before its denial
// reaches the service another client approves the call.
type stopRaceControl struct {
	*Client
	other *Client
	raced bool
}

func (c *stopRaceControl) DecideApproval(ctx context.Context, id string, approvalID string, decisions []ApprovalDecision, stopTurn bool) error {
	if !c.raced {
		c.raced = true
		approved := make([]ApprovalDecision, 0, len(decisions))
		for _, decision := range decisions {
			approved = append(approved, ApprovalDecision{ToolCallID: decision.ToolCallID, Approved: true})
		}
		if err := c.other.DecideApproval(ctx, id, approvalID, approved, false); err != nil {
			return err
		}
	}
	return c.Client.DecideApproval(ctx, id, approvalID, decisions, stopTurn)
}

func turnEndFor(t *testing.T, client *Client, id string) session.DisplayItem {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		items, err := client.AllItems(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			switch item.Type {
			case session.DisplayItemTurnCompleted, session.DisplayItemTurnFailed, session.DisplayItemTurnInterrupted:
				return item
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("turn did not end")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestStopTurnTargetsSubmittedInputBeforeTurnIsSeen(t *testing.T) {
	fixture := servicetest.New(t)
	ctx := context.Background()
	conn := connectEmbedded(t, fixture.StateDir)
	created, err := conn.CreateSession(ctx, CreateSessionRequest{WorkingDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	submissions := NewSubmissions()
	submissions.Begin("stop-me")
	submitted := make(chan error, 1)
	go func() {
		_, err := conn.SubmitMessage(ctx, created.ID, MessageRequest{Text: "slow", ClientMessageID: "stop-me"})
		submissions.Finish("stop-me", err)
		submitted <- err
	}()
	stopCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	result, err := StopTurn(stopCtx, conn.Client, created.ID, StopTarget{InputIDs: []string{"stop-me"}, Submissions: submissions})
	if err != nil || result != StopRequested {
		t.Fatalf("result=%v err=%v", result, err)
	}
	if err := <-submitted; err != nil {
		t.Fatal(err)
	}
	if end := turnEndFor(t, conn.Client, created.ID); end.Type != session.DisplayItemTurnInterrupted {
		t.Fatalf("turn ended with %s", end.Type)
	}
}

func TestStopTurnPausesTurnApprovedByAnotherClient(t *testing.T) {
	fixture := servicetest.New(t)
	ctx := context.Background()
	conn := connectEmbedded(t, fixture.StateDir)
	cwd := t.TempDir()
	created, err := conn.CreateSession(ctx, CreateSessionRequest{WorkingDirectory: cwd, ApprovalPolicy: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	events := conn.Subscribe(ctx, created.ID, 0, StreamOptions{})
	if _, err := conn.SubmitMessage(ctx, created.ID, MessageRequest{Text: "approve-touch-then-slow"}); err != nil {
		t.Fatal(err)
	}
	request := waitItem(t, events, ofType(session.DisplayItemApprovalRequest))
	control := &stopRaceControl{Client: conn.Client, other: New(conn.BaseURL(), conn.token)}
	stopCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	result, err := StopTurn(stopCtx, control, created.ID, StopTarget{TurnID: request.Approval.TurnID})
	if err != nil || result != StopRequested || !control.raced {
		t.Fatalf("result=%v err=%v raced=%v", result, err, control.raced)
	}
	if end := turnEndFor(t, conn.Client, created.ID); end.Type != session.DisplayItemTurnInterrupted {
		t.Fatalf("approved turn kept running and ended with %s", end.Type)
	}
}

// staleSnapshotControl returns a history snapshot taken before the
// submission completed, after the submission completed: review round 2.
type staleSnapshotControl struct {
	*Client
	release  chan struct{}
	finished chan struct{}
	once     sync.Once
}

func (c *staleSnapshotControl) Items(ctx context.Context, id string, after uint64, before uint64, limit int) (ItemsPage, error) {
	page, err := c.Client.Items(ctx, id, after, before, limit)
	c.once.Do(func() {
		close(c.release)
		<-c.finished
	})
	return page, err
}

func TestStopTurnIgnoresStaleSnapshotAfterSubmissionCompletes(t *testing.T) {
	fixture := servicetest.New(t)
	ctx := context.Background()
	conn := connectEmbedded(t, fixture.StateDir)
	created, err := conn.CreateSession(ctx, CreateSessionRequest{WorkingDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	control := &staleSnapshotControl{Client: conn.Client, release: make(chan struct{}), finished: make(chan struct{})}
	submissions := NewSubmissions()
	submissions.Begin("late")
	go func() {
		<-control.release
		_, err := conn.SubmitMessage(ctx, created.ID, MessageRequest{Text: "slow", ClientMessageID: "late"})
		submissions.Finish("late", err)
		close(control.finished)
	}()
	stopCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	result, err := StopTurn(stopCtx, control, created.ID, StopTarget{InputIDs: []string{"late"}, Submissions: submissions})
	if err != nil || result != StopRequested {
		t.Fatalf("result=%v err=%v", result, err)
	}
	if end := turnEndFor(t, conn.Client, created.ID); end.Type != session.DisplayItemTurnInterrupted {
		t.Fatalf("turn ended with %s", end.Type)
	}
}

func TestStopTurnIgnoresRejectedInput(t *testing.T) {
	fixture := servicetest.New(t)
	ctx := context.Background()
	conn := connectEmbedded(t, fixture.StateDir)
	created, err := conn.CreateSession(ctx, CreateSessionRequest{WorkingDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	submissions := NewSubmissions()
	submissions.Begin("rejected")
	submissions.Finish("rejected", &APIError{Status: http.StatusConflict, Code: "active_turn"})
	stopCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	result, err := StopTurn(stopCtx, conn.Client, created.ID, StopTarget{InputIDs: []string{"rejected"}, Submissions: submissions})
	if err != nil || result != StopNothing {
		t.Fatalf("result=%v err=%v", result, err)
	}
}

// An accepted input that is not in the history (yet) is never taken as
// "nothing is running": the stop keeps looking and reports a timeout.
func TestStopTurnDoesNotGuessForUnlocatedAcceptedInput(t *testing.T) {
	fixture := servicetest.New(t)
	ctx := context.Background()
	conn := connectEmbedded(t, fixture.StateDir)
	created, err := conn.CreateSession(ctx, CreateSessionRequest{WorkingDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	submissions := NewSubmissions()
	submissions.Begin("unknown-outcome")
	submissions.Finish("unknown-outcome", errors.New("connection reset"))
	stopCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if result, err := StopTurn(stopCtx, conn.Client, created.ID, StopTarget{InputIDs: []string{"unknown-outcome"}, Submissions: submissions}); err == nil {
		t.Fatalf("result=%v, want an error instead of a guess", result)
	}
}

// Review round 3: the response to an accepted submission is lost. The
// caller must learn the outcome is unknown, not "rejected", and a stop
// that targets the input must still interrupt its turn.
func TestLostSubmitResponseStaysStoppable(t *testing.T) {
	fixture := servicetest.New(t)
	ctx := context.Background()
	conn := connectEmbedded(t, fixture.StateDir)
	created, err := conn.CreateSession(ctx, CreateSessionRequest{WorkingDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	lossy := conn.Via(servicetest.Proxy(t, conn.BaseURL(), servicetest.ProxyOptions{DropMessageResponses: true}))
	submissions := NewSubmissions()
	_, err = SubmitTracked(ctx, lossy, created.ID, MessageRequest{Text: "slow", ClientMessageID: "lost"}, submissions)
	if !errors.Is(err, ErrSubmissionUnconfirmed) || SubmissionRejectedErr(err) {
		t.Fatalf("err = %v, want unconfirmed", err)
	}
	if state := submissions.State("lost"); state != SubmissionAccepted {
		t.Fatalf("state = %v", state)
	}
	stopCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	result, err := StopTurn(stopCtx, lossy, created.ID, StopTarget{InputIDs: []string{"lost"}, Submissions: submissions})
	if err != nil || result != StopRequested {
		t.Fatalf("result=%v err=%v", result, err)
	}
	if end := turnEndFor(t, conn.Client, created.ID); end.Type != session.DisplayItemTurnInterrupted {
		t.Fatalf("turn ended with %s", end.Type)
	}
	items, err := conn.AllItems(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	starts := 0
	for _, item := range items {
		if item.Type == session.DisplayItemTurnStarted {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("resubmissions started %d turns, want 1", starts)
	}
}

// A 4xx on the first attempt is a rejection; nothing is retried.
func TestSubmitTrackedRejectionIsFinal(t *testing.T) {
	fixture := servicetest.New(t)
	conn := connectEmbedded(t, fixture.StateDir)
	submissions := NewSubmissions()
	_, err := SubmitTracked(context.Background(), conn.Client, "sess_missing", MessageRequest{Text: "x", ClientMessageID: "r"}, submissions)
	if !SubmissionRejectedErr(err) || submissions.State("r") != SubmissionRejected {
		t.Fatalf("err=%v state=%v", err, submissions.State("r"))
	}
}

// A stop already confirmed for one turn is not withheld while another
// input is still being located.
func TestStopTurnStopsKnownTurnWhileLocatingOtherInput(t *testing.T) {
	fixture := servicetest.New(t)
	ctx := context.Background()
	conn := connectEmbedded(t, fixture.StateDir)
	created, err := conn.CreateSession(ctx, CreateSessionRequest{WorkingDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.SubmitMessage(ctx, created.ID, MessageRequest{Text: "slow", ClientMessageID: "known"}); err != nil {
		t.Fatal(err)
	}
	submissions := NewSubmissions()
	submissions.Finish("never-recorded", errors.New("connection reset"))
	stopCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	result, err := StopTurn(stopCtx, conn.Client, created.ID, StopTarget{InputIDs: []string{"known", "never-recorded"}, Submissions: submissions})
	if err == nil || result != StopRequested {
		t.Fatalf("result=%v err=%v, want the known turn stopped and an error for the other input", result, err)
	}
	if end := turnEndFor(t, conn.Client, created.ID); end.Type != session.DisplayItemTurnInterrupted {
		t.Fatalf("turn ended with %s", end.Type)
	}
}
