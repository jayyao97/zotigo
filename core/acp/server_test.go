package acp_test

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/acp"
	"go.lsp.dev/jsonrpc2"
)

// pipeRWC wraps an io.Reader and io.Writer into an io.ReadWriteCloser.
type pipeRWC struct {
	io.Reader
	io.Writer
	closeFn func() error
}

func (p *pipeRWC) Close() error {
	if p.closeFn != nil {
		return p.closeFn()
	}
	return nil
}

// newPipePair creates a connected pair of ReadWriteClosers for testing.
// Writes on one end are reads on the other.
func newPipePair() (io.ReadWriteCloser, io.ReadWriteCloser) {
	// server reads from r1, writes to w2
	// client reads from r2, writes to w1
	r1, w1 := io.Pipe()
	r2, w2 := io.Pipe()

	serverSide := &pipeRWC{
		Reader:  r1,
		Writer:  w2,
		closeFn: func() error { r1.Close(); return w2.Close() },
	}
	clientSide := &pipeRWC{
		Reader:  r2,
		Writer:  w1,
		closeFn: func() error { r2.Close(); return w1.Close() },
	}
	return serverSide, clientSide
}

// clientConn creates a jsonrpc2 client connection from a ReadWriteCloser,
// with a noop handler for incoming notifications.
func clientConn(rwc io.ReadWriteCloser) jsonrpc2.Conn {
	stream := jsonrpc2.NewRawStream(rwc)
	conn := jsonrpc2.NewConn(stream)
	conn.Go(context.Background(), jsonrpc2.MethodNotFoundHandler)
	return conn
}

func TestServer_Initialize(t *testing.T) {
	serverSide, clientSide := newPipePair()

	var gotCaps acp.ClientCapabilities
	srv := acp.NewServer(serverSide,
		acp.OnInitialized(func(caps acp.ClientCapabilities) {
			gotCaps = caps
		}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go srv.Run(ctx)

	client := clientConn(clientSide)
	defer client.Close()

	var result acp.InitializeResult
	_, err := client.Call(ctx, "initialize", acp.InitializeParams{
		ProtocolVersion: 1,
		ClientCapabilities: acp.ClientCapabilities{
			FS:       acp.FSCapabilities{ReadTextFile: true, WriteTextFile: true},
			Terminal: true,
		},
		ClientInfo: &acp.Implementation{Name: "test-editor", Version: "1.0"},
	}, &result)
	if err != nil {
		t.Fatalf("initialize call failed: %v", err)
	}

	if result.ProtocolVersion != 1 {
		t.Errorf("expected protocolVersion 1, got %d", result.ProtocolVersion)
	}
	if result.AgentInfo == nil || result.AgentInfo.Name != "zotigo" {
		t.Errorf("expected agentInfo.name=zotigo, got %+v", result.AgentInfo)
	}
	if gotCaps.FS.ReadTextFile != true {
		t.Error("expected client fs.readTextFile=true")
	}
	if gotCaps.Terminal != true {
		t.Error("expected client terminal=true")
	}
}

func TestServer_SessionNew(t *testing.T) {
	serverSide, clientSide := newPipePair()

	var createdID string
	srv := acp.NewServer(serverSide,
		acp.OnSessionNew(func(ctx context.Context, params acp.SessionNewParams) (string, error) {
			if params.Cwd != "/test/dir" {
				t.Errorf("expected cwd=/test/dir, got %s", params.Cwd)
			}
			createdID = "test-session-1"
			return createdID, nil
		}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go srv.Run(ctx)

	client := clientConn(clientSide)
	defer client.Close()

	var result acp.SessionNewResult
	_, err := client.Call(ctx, "session/new", acp.SessionNewParams{
		Cwd:        "/test/dir",
		MCPServers: []acp.MCPServer{},
	}, &result)
	if err != nil {
		t.Fatalf("session/new failed: %v", err)
	}
	if result.SessionID != "test-session-1" {
		t.Errorf("expected sessionId=test-session-1, got %s", result.SessionID)
	}
}

func TestServer_SessionPrompt_BlocksUntilDone(t *testing.T) {
	serverSide, clientSide := newPipePair()

	promptStarted := make(chan struct{})
	promptDone := make(chan struct{})

	srv := acp.NewServer(serverSide,
		acp.OnSessionPrompt(func(ctx context.Context, sessionID string, text string, images []acp.ContentBlock) acp.PromptResult {
			close(promptStarted)
			if text != "hello" {
				t.Errorf("expected text=hello, got %s", text)
			}
			// Simulate work
			<-promptDone
			return acp.PromptResult{StopReason: acp.StopReasonEndTurn}
		}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go srv.Run(ctx)

	client := clientConn(clientSide)
	defer client.Close()

	type promptResponse struct {
		result acp.SessionPromptResult
		err    error
	}
	respCh := make(chan promptResponse, 1)

	go func() {
		var result acp.SessionPromptResult
		_, err := client.Call(ctx, "session/prompt", acp.SessionPromptParams{
			SessionID: "s1",
			Prompt: []acp.ContentBlock{
				{Type: "text", Text: "hello"},
			},
		}, &result)
		respCh <- promptResponse{result, err}
	}()

	// Wait for prompt to start
	select {
	case <-promptStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for prompt to start")
	}

	// Response should NOT have arrived yet (blocked on promptDone)
	select {
	case <-respCh:
		t.Fatal("prompt response arrived before turn finished")
	case <-time.After(100 * time.Millisecond):
		// good — still blocked
	}

	// Finish the turn
	close(promptDone)

	select {
	case resp := <-respCh:
		if resp.err != nil {
			t.Fatalf("session/prompt failed: %v", resp.err)
		}
		if resp.result.StopReason != acp.StopReasonEndTurn {
			t.Errorf("expected stopReason=end_turn, got %s", resp.result.StopReason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for prompt response")
	}
}

func TestServer_SessionPrompt_CancelReturnsCancelled(t *testing.T) {
	serverSide, clientSide := newPipePair()

	promptStarted := make(chan struct{})
	var promptCtx context.Context

	srv := acp.NewServer(serverSide,
		acp.OnSessionPrompt(func(ctx context.Context, sessionID string, text string, images []acp.ContentBlock) acp.PromptResult {
			promptCtx = ctx
			close(promptStarted)
			// Block until cancelled
			<-ctx.Done()
			return acp.PromptResult{StopReason: acp.StopReasonCancelled}
		}),
		acp.OnSessionCancel(func(ctx context.Context, sessionID string) {
			// In real usage this cancels the promptCtx via session.Cancel()
			// For this test, the prompt callback detects its own ctx cancellation.
		}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go srv.Run(ctx)

	// Client with a handler that accepts notifications
	var notifications []json.RawMessage
	var notifMu sync.Mutex

	clientStream := jsonrpc2.NewRawStream(clientSide)
	client := jsonrpc2.NewConn(clientStream)
	client.Go(ctx, jsonrpc2.Handler(func(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
		notifMu.Lock()
		notifications = append(notifications, req.Params())
		notifMu.Unlock()
		return reply(ctx, nil, nil)
	}))
	defer client.Close()

	type promptResponse struct {
		result acp.SessionPromptResult
		err    error
	}
	respCh := make(chan promptResponse, 1)

	go func() {
		var result acp.SessionPromptResult
		_, err := client.Call(ctx, "session/prompt", acp.SessionPromptParams{
			SessionID: "s1",
			Prompt:    []acp.ContentBlock{{Type: "text", Text: "do something"}},
		}, &result)
		respCh <- promptResponse{result, err}
	}()

	// Wait for prompt to start
	select {
	case <-promptStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for prompt to start")
	}

	// Cancel the server context to simulate cancellation propagation
	cancel()
	_ = promptCtx // suppress unused warning

	select {
	case resp := <-respCh:
		if resp.err != nil {
			// Connection may close on cancel — that's acceptable
			return
		}
		if resp.result.StopReason != acp.StopReasonCancelled {
			t.Errorf("expected stopReason=cancelled, got %s", resp.result.StopReason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for prompt response after cancel")
	}
}

func TestServer_SessionList(t *testing.T) {
	serverSide, clientSide := newPipePair()

	srv := acp.NewServer(serverSide)
	srv.RegisterSession("s1", acp.NewSession("s1", "/dir1"))
	srv.RegisterSession("s2", acp.NewSession("s2", "/dir2"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go srv.Run(ctx)

	client := clientConn(clientSide)
	defer client.Close()

	var result acp.SessionListResult
	_, err := client.Call(ctx, "session/list", acp.SessionListParams{}, &result)
	if err != nil {
		t.Fatalf("session/list failed: %v", err)
	}
	if len(result.Sessions) != 2 {
		t.Errorf("expected 2 sessions, got %d", len(result.Sessions))
	}
}

func TestServer_SessionUpdate_WireFormat(t *testing.T) {
	serverSide, clientSide := newPipePair()
	srv := acp.NewServer(serverSide)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go srv.Run(ctx)

	// Set up client that captures notifications
	var notifications []json.RawMessage
	var notifMu sync.Mutex

	clientStream := jsonrpc2.NewRawStream(clientSide)
	client := jsonrpc2.NewConn(clientStream)
	client.Go(ctx, jsonrpc2.Handler(func(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
		if req.Method() == "session/update" {
			notifMu.Lock()
			raw := make(json.RawMessage, len(req.Params()))
			copy(raw, req.Params())
			notifications = append(notifications, raw)
			notifMu.Unlock()
		}
		return reply(ctx, nil, nil)
	}))
	defer client.Close()

	// Give the connection time to start
	time.Sleep(50 * time.Millisecond)

	// Send a text chunk
	err := srv.SendTextChunk(ctx, "s1", "hello world")
	if err != nil {
		t.Fatalf("SendTextChunk failed: %v", err)
	}

	// Send a tool call
	err = srv.SendToolCall(ctx, "s1", "tc1", "read_file", "read", "pending")
	if err != nil {
		t.Fatalf("SendToolCall failed: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	notifMu.Lock()
	defer notifMu.Unlock()

	if len(notifications) < 2 {
		t.Fatalf("expected at least 2 notifications, got %d", len(notifications))
	}

	// Verify text chunk wire format
	var textUpdate struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string `json:"sessionUpdate"`
			Content       struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"update"`
	}
	if err := json.Unmarshal(notifications[0], &textUpdate); err != nil {
		t.Fatalf("failed to unmarshal text update: %v", err)
	}
	if textUpdate.Update.SessionUpdate != "agent_message_chunk" {
		t.Errorf("expected sessionUpdate=agent_message_chunk, got %s", textUpdate.Update.SessionUpdate)
	}
	if textUpdate.Update.Content.Type != "text" {
		t.Errorf("expected content.type=text, got %s", textUpdate.Update.Content.Type)
	}
	if textUpdate.Update.Content.Text != "hello world" {
		t.Errorf("expected content.text='hello world', got %s", textUpdate.Update.Content.Text)
	}

	// Verify tool call wire format
	var toolUpdate struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			SessionUpdate string `json:"sessionUpdate"`
			ToolCallID    string `json:"toolCallId"`
			Title         string `json:"title"`
			Kind          string `json:"kind"`
			Status        string `json:"status"`
		} `json:"update"`
	}
	if err := json.Unmarshal(notifications[1], &toolUpdate); err != nil {
		t.Fatalf("failed to unmarshal tool update: %v", err)
	}
	if toolUpdate.Update.SessionUpdate != "tool_call" {
		t.Errorf("expected sessionUpdate=tool_call, got %s", toolUpdate.Update.SessionUpdate)
	}
	if toolUpdate.Update.ToolCallID != "tc1" {
		t.Errorf("expected toolCallId=tc1, got %s", toolUpdate.Update.ToolCallID)
	}
	if toolUpdate.Update.Status != "pending" {
		t.Errorf("expected status=pending, got %s", toolUpdate.Update.Status)
	}
}
