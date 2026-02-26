package tools

import (
	"context"

	"github.com/jayyao97/zotigo/core/executor"
)

// ToolSafety declares a tool's safety attributes for automatic approval decisions.
type ToolSafety struct {
	ReadOnly bool     // true = no side effects (no file writes, no command execution)
	PathArgs []string // JSON argument names that contain file paths (for scope checking)
}

// Tool defines an executable capability available to the agent.
type Tool interface {
	// Name returns the unique name of the tool (e.g., "read_file").
	Name() string

	// Description returns a short description of what the tool does.
	Description() string

	// Schema returns the JSON schema definition for the tool's arguments.
	// This is used to inform the LLM about how to call the tool.
	// It should return a struct or map compatible with JSON marshaling.
	Schema() any

	// Execute runs the tool with the provided arguments (as a JSON string).
	// The executor parameter provides access to file operations and command execution.
	// It returns the result (which will be serialized to JSON/Text) or an error.
	Execute(ctx context.Context, exec executor.Executor, argsJSON string) (any, error)

	// Safety declares the tool's safety attributes.
	// The agent uses this to decide whether a tool call can be auto-approved.
	Safety() ToolSafety
}
