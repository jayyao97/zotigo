package wire

import "github.com/jayyao97/zotigo/core/agent"

// Input frames: the daemon asks a live worker to accept a command as a new
// turn or as steering, and the worker reports what it did with it.

type InputRequest struct {
	RequestID              string               `json:"request_id"`
	Command                Command              `json:"command"`
	SteeringOnly           bool                 `json:"steering_only,omitempty"`
	StartOnly              bool                 `json:"start_only,omitempty"`
	ExpectedTurnID         string               `json:"expected_turn_id,omitempty"`
	RequiredApprovalPolicy agent.ApprovalPolicy `json:"required_approval_policy,omitempty"`
	RequiredPromptRevision uint64               `json:"required_prompt_revision,omitempty"`
	RequirePromptRevision  bool                 `json:"require_prompt_revision,omitempty"`
}

type InputResult struct {
	RequestID string   `json:"request_id"`
	Command   *Command `json:"command,omitempty"`
	ErrorCode string   `json:"error_code,omitempty"`
	Error     string   `json:"error,omitempty"`
}
