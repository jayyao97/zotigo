package session

import (
	"time"

	"github.com/jayyao97/zotigo/core/agent"
)

// Metadata holds the summary info for listing/indexing.
type Metadata struct {
	ID                 string               `json:"id"`
	WorkingDirectory   string               `json:"working_directory"` // The project path this session belongs to
	Agent              string               `json:"agent,omitempty"`
	ProfileName        string               `json:"profile_name,omitempty"`
	Model              string               `json:"model,omitempty"`
	ReasoningEffort    string               `json:"reasoning_effort,omitempty"`
	ConversationID     string               `json:"conversation_id,omitempty"`
	BackendVersion     string               `json:"backend_version,omitempty"`
	BackendUpdatedAt   time.Time            `json:"backend_updated_at,omitempty"`
	BackendSyncVersion int                  `json:"backend_sync_version,omitempty"`
	ApprovalPolicy     agent.ApprovalPolicy `json:"approval_policy,omitempty"`
	PromptConfig       PromptConfig         `json:"prompt_config,omitempty"`
	Capabilities       Capabilities         `json:"capabilities,omitempty"`
	LastPrompt         string               `json:"last_prompt"` // Preview of the last user interaction
	CreatedAt          time.Time            `json:"created_at"`
	UpdatedAt          time.Time            `json:"updated_at"`
}

// Capabilities records runtime features that a worker must install when it
// starts. Whether a capability can be added after the provider conversation is
// created depends on that runtime; validators enforce those constraints.
type Capabilities struct {
	ChannelToolsVersion uint64 `json:"channel_tools_version,omitempty"`
	SessionToolsVersion uint64 `json:"session_tools_version,omitempty"`
}

// PromptConfig captures session-scoped prompt and approval behavior. Empty
// values preserve the behavior of sessions created before this field existed.
type PromptConfig struct {
	AgentInstructions    string `json:"agent_instructions,omitempty"`
	ApprovalInstructions string `json:"approval_instructions,omitempty"`
	ReviewAllTools       bool   `json:"review_all_tools,omitempty"`
	Revision             uint64 `json:"revision,omitempty"`
}

// Session represents the full state on disk.
type Session struct {
	Metadata
	ForkedFrom             *ForkOrigin          `json:"forked_from,omitempty"`
	ForkPoints             map[string]ForkPoint `json:"fork_points,omitempty"`
	AgentSnapshot          agent.Snapshot       `json:"agent_snapshot"`
	SnapshotVersion        uint64               `json:"snapshot_version,omitempty"`
	CommittedRuntimeWALID  string               `json:"committed_runtime_wal_id,omitempty"`
	CommittedRuntimeWALSeq uint64               `json:"committed_runtime_wal_seq,omitempty"`
	Turns                  []Turn               `json:"turns,omitempty"`
}

// ForkOrigin is provenance, not authority. A branch does not inherit live
// commands, approvals, worker bindings, or channel delivery ownership.
type ForkOrigin struct {
	SessionID     string `json:"session_id"`
	ThroughTurnID string `json:"through_turn_id"`
}

// ForkPoint proves that a prefix still represents a completed native turn.
// Compression may replace that prefix; in that case exact historical forks
// fail closed rather than using a later summary containing future information.
type ForkPoint struct {
	HistoryLength int    `json:"history_length"`
	HistoryDigest string `json:"history_digest"`
}

// EnsureInitialized backfills fields added in newer versions so old sessions remain usable.
func (s *Session) EnsureInitialized() {
	if s.Agent == "" {
		s.Agent = "zotigo"
	}
	if s.ApprovalPolicy == "" {
		s.ApprovalPolicy = agent.ApprovalPolicyAuto
	}
	if s.Turns == nil {
		s.Turns = make([]Turn, 0)
	}
}
