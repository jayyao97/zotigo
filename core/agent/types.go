package agent

import (
	"time"

	"github.com/jayyao97/zotigo/core/agent/prompt"
	"github.com/jayyao97/zotigo/core/protocol"
)

type State string

const (
	StateIdle    State = "idle"
	StateRunning State = "running"
	StatePaused  State = "paused" // Waiting for user input/tool approval
	// StateDurabilityFailed prevents a tool with an uncertain durable result
	// from being executed again in the same process.
	StateDurabilityFailed State = "durability_failed"
)

// Snapshot represents the serializable state of the agent.
type Snapshot struct {
	State            State                    `json:"state"`
	History          []protocol.Message       `json:"history"`
	PendingActions   []*PendingAction         `json:"pending_actions,omitempty"`
	DeferredActions  []*PendingAction         `json:"deferred_actions,omitempty"`
	TurnSafety       TurnSafetyState          `json:"turn_safety,omitempty"`
	Turns            []TurnAudit              `json:"turns,omitempty"`
	UserContextState *prompt.UserContextState `json:"user_context_state,omitempty"`
	CreatedAt        time.Time                `json:"created_at"`
}

// PendingAction represents a tool call that needs approval or execution result.
type PendingAction struct {
	ToolCallID string             `json:"tool_call_id"`
	Name       string             `json:"name"`
	Arguments  string             `json:"arguments"`
	Decision   ActionDecision     `json:"decision,omitempty"`
	Order      int                `json:"order,omitempty"`
	ToolCall   *protocol.ToolCall `json:"-"` // Internal reference
}

// ApprovalPolicy defines how tools should be approved.
type ApprovalPolicy string

const (
	ApprovalPolicyAuto   ApprovalPolicy = "auto"   // Apply tool policy and classifier rules
	ApprovalPolicyManual ApprovalPolicy = "manual" // Ask before every non-safe action
	// ApprovalPolicyBypass skips tool safety classification, classifier calls,
	// snapshots, and user approval for every registered tool.
	ApprovalPolicyBypass ApprovalPolicy = "bypass_permissions"
)
