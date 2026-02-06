// Package transport provides an abstraction layer for communication between
// the agent and different client interfaces (CLI, WebSocket, HTTP API).
package transport

import (
	"context"

	"github.com/jayyao97/zotigo/core/protocol"
)

// Transport defines the interface for bidirectional communication
// between the agent and client interfaces.
type Transport interface {
	// Send sends an event to the client (streaming output)
	Send(ctx context.Context, event protocol.Event) error

	// Receive returns a channel for receiving user inputs
	// The channel is closed when the transport is closed
	Receive(ctx context.Context) <-chan UserInput

	// RequestApproval requests user approval for pending tool calls
	// Returns approved tool call IDs (empty means all denied)
	RequestApproval(ctx context.Context, pending []PendingToolCall) ([]ApprovalResult, error)

	// Close closes the transport connection
	Close() error
}

// UserInput represents input from the user
type UserInput struct {
	// Type of input
	Type UserInputType

	// Text content (for MessageInput)
	Text string

	// Images attached to the message
	Images []ImageData

	// Command for special inputs (e.g., "/clear", "/model")
	Command string

	// Args for command inputs
	Args []string
}

// UserInputType defines the type of user input
type UserInputType int

const (
	// UserInputMessage is a regular chat message
	UserInputMessage UserInputType = iota
	// UserInputCommand is a slash command (e.g., /help, /clear)
	UserInputCommand
	// UserInputCancel requests cancellation of current operation
	UserInputCancel
	// UserInputQuit requests session termination
	UserInputQuit
)

// ImageData represents an attached image
type ImageData struct {
	// MimeType (e.g., "image/png", "image/jpeg")
	MimeType string
	// Data is the raw image bytes
	Data []byte
}

// PendingToolCall represents a tool call awaiting approval
type PendingToolCall struct {
	// ID is the unique identifier for this tool call
	ID string
	// Name is the tool name
	Name string
	// Arguments is the JSON arguments string
	Arguments string
	// Description is a human-readable description of what the tool will do
	Description string
}

// ApprovalResult represents the user's decision on a tool call
type ApprovalResult struct {
	// ToolCallID is the ID of the tool call
	ToolCallID string
	// Approved is true if the user approved this tool call
	Approved bool
	// ModifiedArgs allows the user to modify arguments (optional)
	ModifiedArgs string
}

// EventHandler is a callback for handling events
// Used by transports that need to process events asynchronously
type EventHandler func(event protocol.Event)

// InputHandler is a callback for handling user inputs
// Used by transports that need to process inputs asynchronously
type InputHandler func(input UserInput)
