package acp_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/acp"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/transport"
	"go.lsp.dev/jsonrpc2"
)

// collectingClient creates a client connection that collects session/update
// notifications and responds to request_permission with allow_once.
func collectingClient(clientSide interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Close() error
}, ctx context.Context) (jsonrpc2.Conn, *notificationCollector) {
	collector := &notificationCollector{}

	stream := jsonrpc2.NewRawStream(clientSide)
	conn := jsonrpc2.NewConn(stream)
	conn.Go(ctx, jsonrpc2.Handler(func(ctx context.Context, reply jsonrpc2.Replier, req jsonrpc2.Request) error {
		switch req.Method() {
		case "session/update":
			raw := make(json.RawMessage, len(req.Params()))
			copy(raw, req.Params())
			collector.add(raw)
			return reply(ctx, nil, nil)
		case "session/request_permission":
			return reply(ctx, acp.RequestPermissionResult{
				Outcome: acp.PermissionOutcome{
					Outcome:  "selected",
					OptionID: "allow_once",
				},
			}, nil)
		default:
			return reply(ctx, nil, nil)
		}
	}))

	return conn, collector
}

type notificationCollector struct {
	mu    sync.Mutex
	items []json.RawMessage
}

func (c *notificationCollector) add(raw json.RawMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = append(c.items, raw)
}

func (c *notificationCollector) get() []json.RawMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]json.RawMessage, len(c.items))
	copy(out, c.items)
	return out
}

func TestTransport_SendTextDelta(t *testing.T) {
	serverSide, clientSide := newPipePair()
	srv := acp.NewServer(serverSide)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go srv.Run(ctx)

	client, collector := collectingClient(clientSide, ctx)
	defer client.Close()

	time.Sleep(50 * time.Millisecond)

	tr := acp.NewTransport(srv, "session-1")

	// Send a text delta event
	err := tr.Send(ctx, protocol.Event{
		Type: protocol.EventTypeContentDelta,
		ContentPartDelta: &protocol.ContentPartDelta{
			Type: protocol.ContentTypeText,
			Text: "hello from agent",
		},
	})
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	items := collector.get()
	if len(items) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(items))
	}

	var update struct {
		Update struct {
			SessionUpdate string `json:"sessionUpdate"`
			Content       struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"update"`
	}
	if err := json.Unmarshal(items[0], &update); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if update.Update.SessionUpdate != "agent_message_chunk" {
		t.Errorf("expected agent_message_chunk, got %s", update.Update.SessionUpdate)
	}
	if update.Update.Content.Text != "hello from agent" {
		t.Errorf("expected 'hello from agent', got %s", update.Update.Content.Text)
	}
}

func TestTransport_SendReasoningDelta(t *testing.T) {
	serverSide, clientSide := newPipePair()
	srv := acp.NewServer(serverSide)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go srv.Run(ctx)

	client, collector := collectingClient(clientSide, ctx)
	defer client.Close()

	time.Sleep(50 * time.Millisecond)

	tr := acp.NewTransport(srv, "session-1")

	// Send a reasoning delta
	err := tr.Send(ctx, protocol.Event{
		Type: protocol.EventTypeContentDelta,
		ContentPartDelta: &protocol.ContentPartDelta{
			Type: protocol.ContentTypeReasoning,
			Text: "thinking...",
		},
	})
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	items := collector.get()
	if len(items) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(items))
	}

	var update struct {
		Update struct {
			SessionUpdate string `json:"sessionUpdate"`
		} `json:"update"`
	}
	if err := json.Unmarshal(items[0], &update); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if update.Update.SessionUpdate != "agent_thought_chunk" {
		t.Errorf("expected agent_thought_chunk, got %s", update.Update.SessionUpdate)
	}
}

func TestTransport_ToolCallStatusFlow(t *testing.T) {
	serverSide, clientSide := newPipePair()
	srv := acp.NewServer(serverSide)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go srv.Run(ctx)

	client, collector := collectingClient(clientSide, ctx)
	defer client.Close()

	time.Sleep(50 * time.Millisecond)

	tr := acp.NewTransport(srv, "session-1")

	// Step 1: Tool call proposed → should be "pending"
	err := tr.Send(ctx, protocol.Event{
		Type: protocol.EventTypeToolCallEnd,
		ToolCall: &protocol.ToolCall{
			ID:        "tc-1",
			Name:      "read_file",
			Arguments: `{"path": "/foo"}`,
		},
	})
	if err != nil {
		t.Fatalf("Send ToolCallEnd failed: %v", err)
	}

	// Step 2: Simulate approval → RequestApproval sends in_progress
	results, err := tr.RequestApproval(ctx, []transport.PendingToolCall{
		{ID: "tc-1", Name: "read_file", Arguments: `{"path": "/foo"}`},
	})
	if err != nil {
		t.Fatalf("RequestApproval failed: %v", err)
	}
	if len(results) != 1 || !results[0].Approved {
		t.Fatalf("expected approved, got %+v", results)
	}

	// Step 3: Tool result → "completed"
	err = tr.Send(ctx, protocol.Event{
		Type: protocol.EventTypeToolResultDone,
		ToolResult: &protocol.ToolResult{
			ToolCallID: "tc-1",
			ToolName:   "read_file",
			Text:       "file contents here",
			IsError:    false,
		},
	})
	if err != nil {
		t.Fatalf("Send ToolResultDone failed: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	items := collector.get()
	// We expect: tool_call(pending), tool_call_update(in_progress), tool_call_update(completed)
	if len(items) < 3 {
		t.Fatalf("expected at least 3 notifications, got %d", len(items))
	}

	// Parse statuses
	type updateMsg struct {
		Update struct {
			SessionUpdate string `json:"sessionUpdate"`
			Status        string `json:"status"`
			ToolCallID    string `json:"toolCallId"`
		} `json:"update"`
	}

	var statuses []string
	for _, item := range items {
		var msg updateMsg
		if err := json.Unmarshal(item, &msg); err != nil {
			continue
		}
		if msg.Update.ToolCallID == "tc-1" {
			statuses = append(statuses, msg.Update.Status)
		}
	}

	expected := []string{"pending", "in_progress", "completed"}
	if len(statuses) != len(expected) {
		t.Fatalf("expected statuses %v, got %v", expected, statuses)
	}
	for i, s := range expected {
		if statuses[i] != s {
			t.Errorf("status[%d]: expected %s, got %s", i, s, statuses[i])
		}
	}
}

func TestTransport_ClosePreventsSubsequentSend(t *testing.T) {
	serverSide, clientSide := newPipePair()
	srv := acp.NewServer(serverSide)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go srv.Run(ctx)

	client, _ := collectingClient(clientSide, ctx)
	defer client.Close()

	time.Sleep(50 * time.Millisecond)

	tr := acp.NewTransport(srv, "session-1")
	_ = tr.Close()

	err := tr.Send(ctx, protocol.Event{
		Type: protocol.EventTypeContentDelta,
		ContentPartDelta: &protocol.ContentPartDelta{
			Type: protocol.ContentTypeText,
			Text: "should fail",
		},
	})
	if err == nil {
		t.Fatal("expected error sending on closed transport")
	}
}

func TestTransport_EnqueueAndReceive(t *testing.T) {
	serverSide, clientSide := newPipePair()
	srv := acp.NewServer(serverSide)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go srv.Run(ctx)

	client, _ := collectingClient(clientSide, ctx)
	defer client.Close()

	tr := acp.NewTransport(srv, "session-1")
	defer tr.Close()

	tr.EnqueueInput(transport.UserInput{
		Type: transport.UserInputMessage,
		Text: "test input",
	})

	ch := tr.Receive(ctx)
	select {
	case input := <-ch:
		if input.Text != "test input" {
			t.Errorf("expected 'test input', got %s", input.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout receiving input")
	}
}
