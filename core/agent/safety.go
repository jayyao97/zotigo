package agent

import (
	"time"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/protocol"
)

// ExecutionDecision controls how a pending action should be handled.
type ExecutionDecision string

const (
	ExecutionDecisionAutoExecute     ExecutionDecision = "auto_execute"
	ExecutionDecisionRequireApproval ExecutionDecision = "require_approval"
	ExecutionDecisionBlock           ExecutionDecision = "block"
)

// SafetyDecisionSource identifies which subsystem produced a decision.
type SafetyDecisionSource string

const (
	SafetyDecisionSourceHardRule     SafetyDecisionSource = "hard_rule"
	SafetyDecisionSourceClassifier   SafetyDecisionSource = "classifier"
	SafetyDecisionSourceUserApproval SafetyDecisionSource = "user_approval"
)

// SafetyClassifierDecision is the classifier output shape.
type SafetyClassifierDecision string

const (
	SafetyClassifierDecisionAllow   SafetyClassifierDecision = "allow"
	SafetyClassifierDecisionDeny    SafetyClassifierDecision = "deny"
	SafetyClassifierDecisionAskUser SafetyClassifierDecision = "ask_user"
)

// ActionDecision captures a structured decision for a tool call.
type ActionDecision struct {
	Decision         ExecutionDecision
	Source           SafetyDecisionSource
	Reason           string
	RiskLevel        string
	RequiresSnapshot bool
}

// SafetyClassifierRequest is the bounded input sent to the classifier.
type SafetyClassifierRequest struct {
	UserPrompt    string
	ToolName      string
	ToolArguments string
	RiskLevel     string
	IsGitRepo     bool
	HasSnapshot   bool
	RecentActions []RecentAction
}

// RecentAction captures relevant recent tool activity for classification.
type RecentAction struct {
	ToolName string
	Result   string
	IsError  bool
}

// SafetyClassifierResponse is the structured classifier output.
type SafetyClassifierResponse struct {
	Decision         SafetyClassifierDecision
	Reason           string
	RequiresSnapshot bool
}

// SafetyClassifier provides contextual decisions for high-risk actions.
type SafetyClassifier interface {
	Classify(req SafetyClassifierRequest) (SafetyClassifierResponse, error)
}

// WithSafetyClassifier registers the classifier used for contextual safety decisions.
func WithSafetyClassifier(c SafetyClassifier) AgentOption {
	return func(a *Agent) { a.classifier = c }
}

// WithClassifierProfile stores metadata about the profile selected for classifier execution.
func WithClassifierProfile(name string, profile config.ProfileConfig) AgentOption {
	return func(a *Agent) {
		a.classifierProfileName = name
		a.classifierProfile = profile
	}
}

// WithClassifierUnavailableReason stores why classifier execution cannot be fully configured.
func WithClassifierUnavailableReason(reason string) AgentOption {
	return func(a *Agent) { a.classifierUnavailableReason = reason }
}

// TurnSafetyState tracks turn-scoped safety state such as snapshots.
type TurnSafetyState struct {
	TurnID              string
	SnapshotCreated     bool
	SnapshotID          string
	SnapshotAttempted   bool
	SnapshotFailed      bool
	CurrentUserPrompt   string
	LastDecisionContext *protocol.ToolCall
}

// SnapshotStatus captures turn-level snapshot behavior.
type SnapshotStatus string

const (
	SnapshotStatusNotNeeded      SnapshotStatus = "not_needed"
	SnapshotStatusCreated        SnapshotStatus = "created"
	SnapshotStatusFailed         SnapshotStatus = "failed"
	SnapshotStatusMissingGitRepo SnapshotStatus = "missing_git_repo"
)

// AuditContextSummary stores a compact audit summary instead of full raw context.
type AuditContextSummary struct {
	UserPrompt    string   `json:"user_prompt,omitempty"`
	RecentActions []string `json:"recent_actions,omitempty"`
	Trigger       string   `json:"trigger,omitempty"`
}

// AuditEvent stores a compact auditable safety decision tied to a turn.
type AuditEvent struct {
	Timestamp          time.Time                `json:"timestamp"`
	TurnID             string                   `json:"turn_id"`
	ToolCallID         string                   `json:"tool_call_id,omitempty"`
	ToolName           string                   `json:"tool_name,omitempty"`
	ToolArgsSummary    string                   `json:"tool_args_summary,omitempty"`
	DecisionSource     SafetyDecisionSource     `json:"decision_source"`
	Decision           SafetyClassifierDecision `json:"decision"`
	Reason             string                   `json:"reason,omitempty"`
	RiskLevel          string                   `json:"risk_level,omitempty"`
	SnapshotStatus     SnapshotStatus           `json:"snapshot_status,omitempty"`
	SnapshotID         string                   `json:"snapshot_id,omitempty"`
	ClassifierProvider string                   `json:"classifier_provider,omitempty"`
	ClassifierModel    string                   `json:"classifier_model,omitempty"`
	ContextSummary     AuditContextSummary      `json:"context_summary,omitempty"`
	RawContext         string                   `json:"raw_context,omitempty"`
}

// TurnAudit stores compact turn-scoped execution metadata for auditing.
type TurnAudit struct {
	ID                string         `json:"id"`
	CreatedAt         time.Time      `json:"created_at"`
	UpdatedAt         time.Time      `json:"updated_at"`
	UserPromptSummary string         `json:"user_prompt_summary,omitempty"`
	SafetyEvents      []AuditEvent   `json:"safety_events,omitempty"`
	SnapshotStatus    SnapshotStatus `json:"snapshot_status,omitempty"`
	SnapshotID        string         `json:"snapshot_id,omitempty"`
}
