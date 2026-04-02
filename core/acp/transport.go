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
		if event.ContentPartDelta != nil {
			if event.ContentPartDelta.Type == protocol.ContentTypeReasoning {
				return t.server.SendThoughtChunk(ctx, t.sessionID, event.ContentPartDelta.Text)
			}
			return t.server.SendTextChunk(ctx, t.sessionID, event.ContentPartDelta.Text)
		}

	case protocol.EventTypeToolCallEnd:
		if event.ToolCall != nil {
			// Tool has been proposed by the model but not yet approved/executed.
			return t.server.SendToolCall(ctx, t.sessionID,
				event.ToolCall.ID,
				event.ToolCall.Name,
				"other",
				ToolCallStatusPending,
			)
		}

	case protocol.EventTypeToolResultDone:
		if event.ToolResult != nil {
			status := ToolCallStatusCompleted
			if event.ToolResult.IsError {
				status = ToolCallStatusFailed
			}
			return t.server.SendToolCallUpdate(ctx, t.sessionID, event.ToolResult.ToolCallID, map[string]any{
				"title":  event.ToolResult.ToolName,
				"status": status,
				"content": []ToolCallContent{{
					Type: "content",
					ContentBlock: &ContentBlock{
						Type: "text",
						Text: event.ToolResult.Text,
					},
				}},
			})
		}

	case protocol.EventTypeError:
		if event.Error != nil {
			return t.server.SendTextChunk(ctx, t.sessionID, fmt.Sprintf("Error: %v", event.Error))
		}

	case protocol.EventTypeFinish:
		// No special notification needed — the prompt response carries the stopReason.
	}

	return nil
}

// Receive returns a channel for receiving user inputs (from ACP prompts).
func (t *Transport) Receive(_ context.Context) <-chan transport.UserInput {
	return t.inputCh
}

// RequestApproval asks the editor client for permission via ACP request_permission.
func (t *Transport) RequestApproval(ctx context.Context, pending []transport.PendingToolCall) ([]transport.ApprovalResult, error) {
	results := make([]transport.ApprovalResult, 0, len(pending))

	for _, p := range pending {
		toolCall := ToolCallData{
			ToolCallID: p.ID,
			Title:      fmt.Sprintf("%s: %s", p.Name, p.Description),
			Kind:       "other",
			Status:     ToolCallStatusPending,
			RawInput:   p.Arguments,
		}

		options := []PermissionOption{
			{OptionID: "allow_once", Name: "Allow once", Kind: "allow_once"},
			{OptionID: "allow_always", Name: "Allow always", Kind: "allow_always"},
			{OptionID: "reject_once", Name: "Reject once", Kind: "reject_once"},
		}

		outcome, err := t.server.RequestPermission(ctx, t.sessionID, toolCall, options)
		if err != nil {
			return nil, err
		}

		approved := false
		if outcome.Outcome == "selected" {
			approved = outcome.OptionID == "allow_once" || outcome.OptionID == "allow_always"
		}

		// Transition tool call to in_progress once approved, so the client
		// sees pending → in_progress → completed/failed without regression.
		if approved {
			_ = t.server.SendToolCallUpdate(ctx, t.sessionID, p.ID, map[string]any{
				"status": ToolCallStatusInProgress,
			})
		}

		results = append(results, transport.ApprovalResult{
			ToolCallID: p.ID,
			Approved:   approved,
		})
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
