package agent

import (
	"time"

	"github.com/jayyao97/zotigo/core/protocol"
)

type State string

const (
	StateIdle   State = "idle"
	StateRunning State = "running"
	StatePaused  State = "paused" // Waiting for user input/tool approval
)

// Snapshot represents the serializable state of the agent.
type Snapshot struct {
	State          State              `json:"state"`
	History        []protocol.Message `json:"history"`
	PendingActions []*PendingAction   `json:"pending_actions,omitempty"`
	CreatedAt      time.Time          `json:"created_at"`
}

// PendingAction represents a tool call that needs approval or execution result.
type PendingAction struct {
	ToolCallID string             `json:"tool_call_id"`
	Name       string             `json:"name"`
	Arguments  string             `json:"arguments"`
	ToolCall   *protocol.ToolCall `json:"-"` // Internal reference
}

// ApprovalPolicy defines how tools should be approved.
type ApprovalPolicy string

const (
	ApprovalPolicyAuto   ApprovalPolicy = "auto"   // Auto-execute everything
	ApprovalPolicyManual ApprovalPolicy = "manual" // Ask for everything
)
