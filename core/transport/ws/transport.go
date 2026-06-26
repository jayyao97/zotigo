package ws

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/transport"
)

const defaultWriteWait = 10 * time.Second

type approvalResponse struct {
	results []transport.ApprovalResult
	err     error
}

type approvalWaiter struct {
	expected map[string]struct{}
	response chan approvalResponse
}

// Transport implements transport.Transport over one established WebSocket
// connection. It owns message framing and per-write deadlines only; callers
// own dialing, listening, auth, session routing, and long-lived keepalive.
type Transport struct {
	conn      *websocket.Conn
	writeWait time.Duration

	inputCh  chan transport.UserInput
	closedCh chan struct{}

	writeMu sync.Mutex
	mu      sync.Mutex
	closed  bool
	waiters map[string]approvalWaiter
	nextID  atomic.Uint64
}

func NewTransport(conn *websocket.Conn) *Transport {
	t := &Transport{
		conn:      conn,
		writeWait: defaultWriteWait,
		inputCh:   make(chan transport.UserInput, 32),
		closedCh:  make(chan struct{}),
		waiters:   make(map[string]approvalWaiter),
	}
	go t.readLoop()
	return t
}

func (t *Transport) Send(ctx context.Context, event protocol.Event) error {
	return t.write(ctx, Message{
		Type:  MessageTypeEvent,
		Event: &event,
	})
}

func (t *Transport) Receive(context.Context) <-chan transport.UserInput {
	return t.inputCh
}

func (t *Transport) RequestApproval(ctx context.Context, pending []transport.PendingToolCall) ([]transport.ApprovalResult, error) {
	id := strconv.FormatUint(t.nextID.Add(1), 10)
	waiter := make(chan approvalResponse, 1)

	if err := t.addWaiter(id, pending, waiter); err != nil {
		return nil, err
	}
	if err := t.write(ctx, Message{
		Type:    MessageTypeApprovalRequest,
		ID:      id,
		Pending: pending,
	}); err != nil {
		t.removeWaiter(id)
		return nil, err
	}

	select {
	case <-ctx.Done():
		t.removeWaiter(id)
		return nil, ctx.Err()
	case <-t.closedCh:
		select {
		case resp := <-waiter:
			return resp.results, resp.err
		default:
		}
		return nil, transport.ErrTransportClosed
	case resp := <-waiter:
		return resp.results, resp.err
	}
}

func (t *Transport) Close() error {
	return t.close()
}

func (t *Transport) readLoop() {
	defer close(t.inputCh)

	for {
		_, data, err := t.conn.ReadMessage()
		if err != nil {
			_ = t.close()
			return
		}

		msg, err := Decode(data)
		if err != nil {
			t.failAll(fmt.Errorf("decode websocket message: %w", err))
			_ = t.close()
			return
		}
		if err := t.handleMessage(msg); err != nil {
			t.failAll(err)
			_ = t.close()
			return
		}
	}
}

func (t *Transport) handleMessage(msg Message) error {
	switch msg.Type {
	case MessageTypeInput:
		if msg.Input == nil {
			return fmt.Errorf("input websocket message missing input payload")
		}
		select {
		case <-t.closedCh:
			return nil
		case t.inputCh <- *msg.Input:
			return nil
		default:
			return fmt.Errorf("websocket input queue full")
		}

	case MessageTypeApprovalResult:
		return t.completeApproval(msg)

	case MessageTypeClose:
		return t.close()

	default:
		return fmt.Errorf("unsupported websocket message type %q", msg.Type)
	}
}

func (t *Transport) completeApproval(msg Message) error {
	if msg.ID == "" {
		return fmt.Errorf("approval_result websocket message missing id")
	}

	t.mu.Lock()
	waiter, ok := t.waiters[msg.ID]
	if ok {
		delete(t.waiters, msg.ID)
	}
	t.mu.Unlock()

	if !ok {
		return nil
	}
	if err := validateApprovals(waiter.expected, msg.Approvals); err != nil {
		waiter.response <- approvalResponse{err: err}
		return nil
	}

	resp := approvalResponse{results: msg.Approvals}
	if msg.Error != "" {
		resp.err = fmt.Errorf("%s", msg.Error)
	}
	waiter.response <- resp
	return nil
}

func (t *Transport) write(ctx context.Context, msg Message) error {
	if err := t.isOpen(); err != nil {
		return err
	}
	data, err := Encode(msg)
	if err != nil {
		return err
	}

	t.writeMu.Lock()
	defer t.writeMu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.closedCh:
		return transport.ErrTransportClosed
	default:
	}

	if err := t.conn.SetWriteDeadline(time.Now().Add(t.writeWait)); err != nil {
		_ = t.close()
		return err
	}
	if err := t.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		_ = t.close()
		return err
	}
	return nil
}

func (t *Transport) addWaiter(id string, pending []transport.PendingToolCall, response chan approvalResponse) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return transport.ErrTransportClosed
	}
	t.waiters[id] = approvalWaiter{
		expected: expectedApprovals(pending),
		response: response,
	}
	return nil
}

func (t *Transport) removeWaiter(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.waiters, id)
}

func (t *Transport) failAll(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, waiter := range t.waiters {
		delete(t.waiters, id)
		waiter.response <- approvalResponse{err: err}
	}
}

func (t *Transport) isOpen() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return transport.ErrTransportClosed
	}
	return nil
}

func (t *Transport) close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	close(t.closedCh)
	waiters := t.waiters
	t.waiters = make(map[string]approvalWaiter)
	t.mu.Unlock()

	for _, waiter := range waiters {
		waiter.response <- approvalResponse{err: transport.ErrTransportClosed}
	}
	return t.conn.Close()
}

func expectedApprovals(pending []transport.PendingToolCall) map[string]struct{} {
	expected := make(map[string]struct{}, len(pending))
	for _, call := range pending {
		expected[call.ID] = struct{}{}
	}
	return expected
}

func validateApprovals(expected map[string]struct{}, approvals []transport.ApprovalResult) error {
	if len(approvals) != len(expected) {
		return fmt.Errorf("approval_result contains %d results for %d pending tool calls", len(approvals), len(expected))
	}
	seen := make(map[string]struct{}, len(approvals))
	for _, approval := range approvals {
		if _, ok := expected[approval.ToolCallID]; !ok {
			return fmt.Errorf("approval_result contains unknown tool call id %q", approval.ToolCallID)
		}
		if _, ok := seen[approval.ToolCallID]; ok {
			return fmt.Errorf("approval_result contains duplicate tool call id %q", approval.ToolCallID)
		}
		seen[approval.ToolCallID] = struct{}{}
	}
	return nil
}

var _ transport.Transport = (*Transport)(nil)
