package transport

import (
	"context"
	"sync"

	"github.com/jayyao97/zotigo/core/protocol"
)

// ChannelTransport is a basic transport implementation using Go channels.
// It's suitable for in-process communication and can be embedded in other transports.
type ChannelTransport struct {
	eventCh    chan protocol.Event
	inputCh    chan UserInput
	approvalCh chan []ApprovalResult

	mu       sync.Mutex
	closed   bool
	closedCh chan struct{}
}

// NewChannelTransport creates a new channel-based transport
func NewChannelTransport(bufferSize int) *ChannelTransport {
	if bufferSize <= 0 {
		bufferSize = 100
	}
	return &ChannelTransport{
		eventCh:    make(chan protocol.Event, bufferSize),
		inputCh:    make(chan UserInput, bufferSize),
		approvalCh: make(chan []ApprovalResult, 1),
		closedCh:   make(chan struct{}),
	}
}

// Send sends an event to the client
func (t *ChannelTransport) Send(ctx context.Context, event protocol.Event) error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return ErrTransportClosed
	}
	t.mu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.closedCh:
		return ErrTransportClosed
	case t.eventCh <- event:
		return nil
	}
}

// Receive returns the channel for receiving user inputs
func (t *ChannelTransport) Receive(ctx context.Context) <-chan UserInput {
	return t.inputCh
}

// RequestApproval requests user approval for pending tool calls
func (t *ChannelTransport) RequestApproval(ctx context.Context, pending []PendingToolCall) ([]ApprovalResult, error) {
	// Send approval request event
	event := protocol.Event{
		Type:         protocol.EventTypeFinish,
		FinishReason: "need_approval",
	}
	if err := t.Send(ctx, event); err != nil {
		return nil, err
	}

	// Wait for approval response
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.closedCh:
		return nil, ErrTransportClosed
	case results := <-t.approvalCh:
		return results, nil
	}
}

// Close closes the transport
func (t *ChannelTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return nil
	}

	t.closed = true
	close(t.closedCh)
	close(t.eventCh)
	close(t.inputCh)
	return nil
}

// --- Methods for the other side of the transport ---

// Events returns the channel for receiving events (for the client side)
func (t *ChannelTransport) Events() <-chan protocol.Event {
	return t.eventCh
}

// SendInput sends a user input to the agent
func (t *ChannelTransport) SendInput(ctx context.Context, input UserInput) error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return ErrTransportClosed
	}
	t.mu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.closedCh:
		return ErrTransportClosed
	case t.inputCh <- input:
		return nil
	}
}

// SendApproval sends approval results for pending tool calls
func (t *ChannelTransport) SendApproval(ctx context.Context, results []ApprovalResult) error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return ErrTransportClosed
	}
	t.mu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.closedCh:
		return ErrTransportClosed
	case t.approvalCh <- results:
		return nil
	}
}

// IsClosed returns true if the transport is closed
func (t *ChannelTransport) IsClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

// Ensure ChannelTransport implements Transport
var _ Transport = (*ChannelTransport)(nil)
