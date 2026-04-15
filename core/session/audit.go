package session

import "time"

// SafetyDecisionSource identifies where a safety decision came from.
type SafetyDecisionSource string

const (
	SafetyDecisionSourceHardRule     SafetyDecisionSource = "hard_rule"
	SafetyDecisionSourceClassifier   SafetyDecisionSource = "classifier"
	SafetyDecisionSourceUserApproval SafetyDecisionSource = "user_approval"
)

// SafetyDecision captures the outcome of a safety gate.
type SafetyDecision string

const (
	SafetyDecisionAllow   SafetyDecision = "allow"
	SafetyDecisionDeny    SafetyDecision = "deny"
	SafetyDecisionAskUser SafetyDecision = "ask_user"
)

// SnapshotStatus captures snapshot behavior for a turn or event.
type SnapshotStatus string

const (
	SnapshotStatusNotNeeded      SnapshotStatus = "not_needed"
	SnapshotStatusCreated        SnapshotStatus = "created"
	SnapshotStatusFailed         SnapshotStatus = "failed"
	SnapshotStatusMissingGitRepo SnapshotStatus = "missing_git_repo"
)

// ContextSummary stores a compact audit summary instead of full raw context.
type ContextSummary struct {
	UserPrompt    string   `json:"user_prompt,omitempty"`
	RecentActions []string `json:"recent_actions,omitempty"`
	Trigger       string   `json:"trigger,omitempty"`
}

// SafetyEvent stores a compact auditable safety decision tied to a turn.
type SafetyEvent struct {
	Timestamp          time.Time            `json:"timestamp"`
	TurnID             string               `json:"turn_id"`
	ToolCallID         string               `json:"tool_call_id,omitempty"`
	ToolName           string               `json:"tool_name,omitempty"`
	ToolArgsSummary    string               `json:"tool_args_summary,omitempty"`
	DecisionSource     SafetyDecisionSource `json:"decision_source"`
	Decision           SafetyDecision       `json:"decision"`
	Reason             string               `json:"reason,omitempty"`
	RiskLevel          string               `json:"risk_level,omitempty"`
	SnapshotStatus     SnapshotStatus       `json:"snapshot_status,omitempty"`
	SnapshotID         string               `json:"snapshot_id,omitempty"`
	ClassifierProvider string               `json:"classifier_provider,omitempty"`
	ClassifierModel    string               `json:"classifier_model,omitempty"`
	ContextSummary     ContextSummary       `json:"context_summary,omitempty"`
	RawContext         string               `json:"raw_context,omitempty"`
}

// Turn stores compact turn-scoped execution metadata for auditing.
type Turn struct {
	ID                string         `json:"id"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
	UserPromptSummary string         `json:"user_prompt_summary,omitempty"`
	SafetyEvents      []SafetyEvent  `json:"safety_events,omitempty"`
	SnapshotStatus    SnapshotStatus `json:"snapshot_status,omitempty"`
	SnapshotID        string         `json:"snapshot_id,omitempty"`
}
