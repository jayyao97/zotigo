package acp

import (
	"context"
	"fmt"
	"sync"

	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/transport"
)

// Transport adapts an ACP Server into the zotigo Transport interface.
// It bridges the ACP JSON-RPC protocol with the internal agent event system.
type Transport struct {
	server    *Server
	sessionID string

	inputCh  chan transport.UserInput
	closedCh chan struct{}

	mu     sync.Mutex
	closed bool
}

// NewTransport creates a Transport backed by an ACP server for a specific session.
func NewTransport(server *Server, sessionID string) *Transport {
	return &Transport{
		server:    server,
		sessionID: sessionID,
		inputCh:   make(chan transport.UserInput, 32),
		closedCh:  make(chan struct{}),
	}
}

// Send converts internal protocol events to ACP session/update notifications.
func (t *Transport) Send(ctx context.Context, event protocol.Event) error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return transport.ErrTransportClosed
	}
	t.mu.Unlock()

	switch event.Type {
	case protocol.EventTypeContentDelta:
		if event.ContentPartDelta != nil && event.ContentPartDelta.Type != protocol.ContentTypeReasoning {
			return t.server.SendTextChunk(ctx, t.sessionID, event.ContentPartDelta.Text)
		}
		// Skip reasoning deltas for now (ACP doesn't have a standard for this)

	case protocol.EventTypeToolCallEnd:
		if event.ToolCall != nil {
			return t.server.SendToolCallUpdate(ctx, t.sessionID, ToolCallUpdate{
				ID:     event.ToolCall.ID,
				Name:   event.ToolCall.Name,
				Status: "running",
			})
		}

	case protocol.EventTypeToolResultDone:
		if event.ToolResult != nil {
			status := "completed"
			if event.ToolResult.IsError {
				status = "failed"
			}
			return t.server.SendToolCallUpdate(ctx, t.sessionID, ToolCallUpdate{
				ID:     event.ToolResult.ToolCallID,
				Name:   event.ToolResult.ToolName,
				Status: status,
				Result: event.ToolResult.Text,
			})
		}

	case protocol.EventTypeFinish:
		// Send a final empty chunk to signal completion
		return t.server.SendUpdate(ctx, t.sessionID, SessionUpdate{
			Type: "agent_message_chunk",
			MessageChunk: &MessageChunk{
				Role:    "assistant",
				Content: "",
			},
		})

	case protocol.EventTypeError:
		if event.Error != nil {
			return t.server.SendTextChunk(ctx, t.sessionID, fmt.Sprintf("Error: %v", event.Error))
		}
	}

	return nil
}

// Receive returns a channel for receiving user inputs (from ACP prompts).
func (t *Transport) Receive(_ context.Context) <-chan transport.UserInput {
	return t.inputCh
}

// RequestApproval asks the editor client for permission via ACP request_permission.
func (t *Transport) RequestApproval(ctx context.Context, pending []transport.PendingToolCall) ([]transport.ApprovalResult, error) {
	perms := make([]PermissionDetail, len(pending))
	for i, p := range pending {
		perms[i] = PermissionDetail{
			ID:          p.ID,
			Title:       fmt.Sprintf("%s: %s", p.Name, p.Description),
			Description: p.Arguments,
		}
	}

	decisions, err := t.server.RequestPermission(ctx, t.sessionID, perms)
	if err != nil {
		return nil, err
	}

	results := make([]transport.ApprovalResult, len(decisions))
	for i, d := range decisions {
		results[i] = transport.ApprovalResult{
			ToolCallID: d.ID,
			Approved:   d.Kind == "allow_once" || d.Kind == "allow_always",
		}
	}
	return results, nil
}

// EnqueueInput pushes a user input into the transport's input channel.
// Called by the ACP server when it receives a session/prompt.
func (t *Transport) EnqueueInput(input transport.UserInput) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.mu.Unlock()

	select {
	case t.inputCh <- input:
	default:
		// drop if full
	}
}

// Close closes the transport.
func (t *Transport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return nil
	}
	t.closed = true
	close(t.closedCh)
	close(t.inputCh)
	return nil
}

// Ensure Transport implements transport.Transport.
var _ transport.Transport = (*Transport)(nil)
