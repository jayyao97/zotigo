package wire

import (
	"time"

	zotigosession "github.com/jayyao97/zotigo/core/session"
)

// Approval payloads: the request a worker raises, the decision the daemon
// relays from a client, and the worker's acknowledgement.

type ApprovalRequest struct {
	ID         string                 `json:"id"`
	SessionID  string                 `json:"session_id"`
	TurnID     string                 `json:"turn_id"`
	Status     string                 `json:"status"`
	Pending    []PendingApproval      `json:"pending"`
	Decisions  []ApprovalToolDecision `json:"decisions,omitempty"`
	CreatedAt  time.Time              `json:"created_at"`
	ResolvedAt *time.Time             `json:"resolved_at,omitempty"`
}

type PendingApproval struct {
	ToolCallID       string `json:"tool_call_id,omitempty"`
	ToolName         string `json:"tool_name,omitempty"`
	Arguments        string `json:"arguments,omitempty"`
	Description      string `json:"description,omitempty"`
	Reason           string `json:"reason,omitempty"`
	RiskLevel        string `json:"risk_level,omitempty"`
	Source           string `json:"source,omitempty"`
	RequiresSnapshot bool   `json:"requires_snapshot,omitempty"`
}

type ApprovalToolDecision struct {
	ToolCallID   string `json:"tool_call_id,omitempty"`
	Approved     bool   `json:"approved"`
	Reason       string `json:"reason,omitempty"`
	ModifiedArgs string `json:"modified_args,omitempty"`
}

type ApprovalDecision struct {
	RequestID  string                                  `json:"request_id"`
	ApprovalID string                                  `json:"approval_id"`
	Decisions  []zotigosession.DisplayApprovalDecision `json:"decisions"`
	StopTurn   bool                                    `json:"stop_turn,omitempty"`
}

type ApprovalResult struct {
	RequestID string           `json:"request_id"`
	Approval  *ApprovalRequest `json:"approval,omitempty"`
	Error     string           `json:"error,omitempty"`
}
