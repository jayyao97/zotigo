package ws

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/transport"
)

func newTestPair(t *testing.T) (*Transport, *websocket.Conn, func()) {
	t.Helper()

	upgrader := websocket.Upgrader{}
	transportCh := make(chan *Transport, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		transportCh <- NewTransport(conn)
	}))

	url := "ws" + strings.TrimPrefix(server.URL, "http")
	client, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		server.Close()
		t.Fatalf("dial failed: %v", err)
	}

	var tr *Transport
	select {
	case tr = <-transportCh:
	case <-time.After(time.Second):
		client.Close()
		server.Close()
		t.Fatal("timeout waiting for server transport")
	}

	cleanup := func() {
		_ = tr.Close()
		_ = client.Close()
		server.Close()
	}
	return tr, client, cleanup
}

func TestTransportSendWritesEvent(t *testing.T) {
	tr, client, cleanup := newTestPair(t)
	defer cleanup()

	if err := tr.Send(context.Background(), protocol.NewTextDeltaEvent("hello")); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	var msg Message
	if err := client.ReadJSON(&msg); err != nil {
		t.Fatalf("ReadJSON failed: %v", err)
	}
	if msg.Type != MessageTypeEvent {
		t.Fatalf("expected event message, got %q", msg.Type)
	}
	if msg.Event == nil || msg.Event.ContentPartDelta == nil || msg.Event.ContentPartDelta.Text != "hello" {
		t.Fatalf("unexpected event payload: %#v", msg.Event)
	}
}

func TestTransportSendHonorsWriteDeadline(t *testing.T) {
	tr, _, cleanup := newTestPair(t)
	defer cleanup()
	tr.writeWait = -time.Second

	err := tr.Send(context.Background(), protocol.NewTextDeltaEvent("hello"))
	if err == nil {
		t.Fatal("expected Send to fail")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("expected timeout error, got %v", err)
	}
	if err := tr.Send(context.Background(), protocol.NewTextDeltaEvent("again")); err != transport.ErrTransportClosed {
		t.Fatalf("expected ErrTransportClosed after timeout, got %v", err)
	}
}

func TestTransportReceiveReadsInput(t *testing.T) {
	tr, client, cleanup := newTestPair(t)
	defer cleanup()

	if err := client.WriteJSON(Message{
		Type:  MessageTypeInput,
		Input: &transport.UserInput{Type: transport.UserInputMessage, Text: "run tests"},
	}); err != nil {
		t.Fatalf("WriteJSON failed: %v", err)
	}

	select {
	case input := <-tr.Receive(context.Background()):
		if input.Type != transport.UserInputMessage || input.Text != "run tests" {
			t.Fatalf("unexpected input: %#v", input)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for input")
	}
}

func TestTransportRequestApprovalRoundTrip(t *testing.T) {
	tr, client, cleanup := newTestPair(t)
	defer cleanup()

	done := make(chan struct{})
	var results []transport.ApprovalResult
	var requestErr error
	go func() {
		results, requestErr = tr.RequestApproval(context.Background(), []transport.PendingToolCall{
			{ID: "call_1", Name: "write_file", Arguments: `{"path":"a.txt"}`},
		})
		close(done)
	}()

	var request Message
	if err := client.ReadJSON(&request); err != nil {
		t.Fatalf("ReadJSON failed: %v", err)
	}
	if request.Type != MessageTypeApprovalRequest || request.ID == "" {
		t.Fatalf("unexpected approval request: %#v", request)
	}
	if len(request.Pending) != 1 || request.Pending[0].ID != "call_1" {
		t.Fatalf("unexpected pending calls: %#v", request.Pending)
	}

	if err := client.WriteJSON(Message{
		Type: MessageTypeApprovalResult,
		ID:   request.ID,
		Approvals: []transport.ApprovalResult{
			{ToolCallID: "call_1", Approved: true},
		},
	}); err != nil {
		t.Fatalf("WriteJSON failed: %v", err)
	}

	select {
	case <-done:
		if requestErr != nil {
			t.Fatalf("RequestApproval failed: %v", requestErr)
		}
		if len(results) != 1 || !results[0].Approved {
			t.Fatalf("unexpected approval results: %#v", results)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for approval result")
	}
}

func TestTransportRequestApprovalRejectsMalformedApprovalResults(t *testing.T) {
	tests := []struct {
		name      string
		pending   []transport.PendingToolCall
		approval  []transport.ApprovalResult
		wantError string
	}{
		{
			name:      "missing approval",
			pending:   []transport.PendingToolCall{{ID: "call_1", Name: "write_file", Arguments: `{"path":"a.txt"}`}},
			approval:  nil,
			wantError: "approval_result contains 0 results for 1 pending tool calls",
		},
		{
			name: "unknown tool call id",
			pending: []transport.PendingToolCall{
				{ID: "call_1", Name: "write_file", Arguments: `{"path":"a.txt"}`},
			},
			approval: []transport.ApprovalResult{
				{ToolCallID: "call_2", Approved: true},
			},
			wantError: `approval_result contains unknown tool call id "call_2"`,
		},
		{
			name: "duplicate tool call id",
			pending: []transport.PendingToolCall{
				{ID: "call_1", Name: "write_file", Arguments: `{"path":"a.txt"}`},
				{ID: "call_2", Name: "shell", Arguments: `{"cmd":"go test ./..."}`},
			},
			approval: []transport.ApprovalResult{
				{ToolCallID: "call_1", Approved: true},
				{ToolCallID: "call_1", Approved: true},
			},
			wantError: `approval_result contains duplicate tool call id "call_1"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr, client, cleanup := newTestPair(t)
			defer cleanup()

			done := make(chan error, 1)
			go func() {
				_, err := tr.RequestApproval(context.Background(), tt.pending)
				done <- err
			}()

			var request Message
			if err := client.ReadJSON(&request); err != nil {
				t.Fatalf("ReadJSON approval request failed: %v", err)
			}

			if err := client.WriteJSON(Message{
				Type:      MessageTypeApprovalResult,
				ID:        request.ID,
				Approvals: tt.approval,
			}); err != nil {
				t.Fatalf("WriteJSON approval result failed: %v", err)
			}

			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("expected error containing %q, got %v", tt.wantError, err)
				}
			case <-time.After(time.Second):
				t.Fatal("timeout waiting for approval request to fail")
			}
		})
	}
}

func TestTransportMalformedApprovalResultDoesNotCloseConnection(t *testing.T) {
	tr, client, cleanup := newTestPair(t)
	defer cleanup()

	firstDone := make(chan error, 1)
	go func() {
		_, err := tr.RequestApproval(context.Background(), []transport.PendingToolCall{
			{ID: "call_1", Name: "write_file", Arguments: `{"path":"a.txt"}`},
		})
		firstDone <- err
	}()

	secondDone := make(chan error, 1)
	go func() {
		_, err := tr.RequestApproval(context.Background(), []transport.PendingToolCall{
			{ID: "call_2", Name: "shell", Arguments: `{"cmd":"go test ./..."}`},
		})
		secondDone <- err
	}()

	requests := readApprovalRequests(t, client, 2)
	first := requests["call_1"]
	second := requests["call_2"]

	if err := client.WriteJSON(Message{
		Type:      MessageTypeApprovalResult,
		ID:        first.ID,
		Approvals: nil,
	}); err != nil {
		t.Fatalf("WriteJSON malformed approval result failed: %v", err)
	}

	select {
	case err := <-firstDone:
		if err == nil || !strings.Contains(err.Error(), "approval_result contains 0 results for 1 pending tool calls") {
			t.Fatalf("expected validation error, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for malformed approval request to fail")
	}

	if err := client.WriteJSON(Message{
		Type: MessageTypeApprovalResult,
		ID:   second.ID,
		Approvals: []transport.ApprovalResult{
			{ToolCallID: "call_2", Approved: true},
		},
	}); err != nil {
		t.Fatalf("WriteJSON valid approval result failed: %v", err)
	}

	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("expected unrelated approval request to succeed, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for unrelated approval request")
	}
}

func TestTransportRequestApprovalDoesNotHangWhenInputQueueFull(t *testing.T) {
	tr, client, cleanup := newTestPair(t)
	defer cleanup()

	for i := 0; i < cap(tr.inputCh); i++ {
		if err := client.WriteJSON(Message{
			Type:  MessageTypeInput,
			Input: &transport.UserInput{Type: transport.UserInputMessage, Text: "queued"},
		}); err != nil {
			t.Fatalf("WriteJSON input %d failed: %v", i, err)
		}
	}
	waitForInputQueueLen(t, tr, cap(tr.inputCh))

	done := make(chan error, 1)
	go func() {
		_, err := tr.RequestApproval(context.Background(), []transport.PendingToolCall{
			{ID: "call_1", Name: "write_file", Arguments: `{"path":"a.txt"}`},
		})
		done <- err
	}()

	var request Message
	if err := client.ReadJSON(&request); err != nil {
		t.Fatalf("ReadJSON approval request failed: %v", err)
	}
	if request.Type != MessageTypeApprovalRequest || request.ID == "" {
		t.Fatalf("unexpected approval request: %#v", request)
	}

	if err := client.WriteJSON(Message{
		Type:  MessageTypeInput,
		Input: &transport.UserInput{Type: transport.UserInputMessage, Text: "over capacity"},
	}); err != nil {
		t.Fatalf("WriteJSON over-capacity input failed: %v", err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected approval request to fail")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for approval request to unblock")
	}
}

func TestTransportRequestApprovalUnblocksOnClose(t *testing.T) {
	tr, client, cleanup := newTestPair(t)
	defer cleanup()

	done := make(chan error, 1)
	go func() {
		_, err := tr.RequestApproval(context.Background(), []transport.PendingToolCall{
			{ID: "call_1", Name: "write_file", Arguments: `{"path":"a.txt"}`},
		})
		done <- err
	}()

	var request Message
	if err := client.ReadJSON(&request); err != nil {
		t.Fatalf("ReadJSON approval request failed: %v", err)
	}

	if err := tr.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	select {
	case err := <-done:
		if !errors.Is(err, transport.ErrTransportClosed) {
			t.Fatalf("expected ErrTransportClosed, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for approval request to unblock")
	}
}

func TestTransportClosePreventsSend(t *testing.T) {
	tr, _, cleanup := newTestPair(t)
	defer cleanup()

	if err := tr.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if err := tr.Send(context.Background(), protocol.NewFinishEvent(protocol.FinishReasonStop)); err != transport.ErrTransportClosed {
		t.Fatalf("expected ErrTransportClosed, got %v", err)
	}
}

func TestTransportCloseClosesReceive(t *testing.T) {
	tr, _, cleanup := newTestPair(t)
	defer cleanup()

	if err := tr.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("second Close failed: %v", err)
	}

	select {
	case _, ok := <-tr.Receive(context.Background()):
		if ok {
			t.Fatal("expected receive channel to close")
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for receive channel to close")
	}
}

func TestCodecRejectsMalformedMessages(t *testing.T) {
	if _, err := Encode(Message{}); err == nil {
		t.Fatal("expected missing type encode error")
	}
	if _, err := Decode([]byte(`{`)); err == nil {
		t.Fatal("expected invalid JSON decode error")
	}
	if _, err := Decode([]byte(`{}`)); err == nil {
		t.Fatal("expected missing type decode error")
	}
}

func TestTransportRejectsMalformedMessages(t *testing.T) {
	tr, _, cleanup := newTestPair(t)
	defer cleanup()

	if err := tr.handleMessage(Message{Type: MessageTypeInput}); err == nil {
		t.Fatal("expected missing input payload error")
	}
	if err := tr.handleMessage(Message{Type: MessageTypeApprovalResult}); err == nil {
		t.Fatal("expected missing approval id error")
	}
}

func readApprovalRequests(t *testing.T, client *websocket.Conn, count int) map[string]Message {
	t.Helper()

	requests := make(map[string]Message, count)
	for len(requests) < count {
		var request Message
		if err := client.ReadJSON(&request); err != nil {
			t.Fatalf("ReadJSON approval request failed: %v", err)
		}
		if request.Type != MessageTypeApprovalRequest || request.ID == "" {
			t.Fatalf("unexpected approval request: %#v", request)
		}
		if len(request.Pending) != 1 {
			t.Fatalf("expected one pending call, got %#v", request.Pending)
		}
		requests[request.Pending[0].ID] = request
	}
	return requests
}

func waitForInputQueueLen(t *testing.T, tr *Transport, want int) {
	t.Helper()

	deadline := time.After(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()

	for {
		if len(tr.inputCh) == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for input queue length %d, got %d", want, len(tr.inputCh))
		case <-ticker.C:
		}
	}
}
