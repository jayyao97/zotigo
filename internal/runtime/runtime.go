package runtime

import (
	"context"
	"time"
)

type AgentKind string

const (
	AgentZotigo AgentKind = "zotigo"
	AgentCodex  AgentKind = "codex"
)

type BackendBinding struct {
	Agent          AgentKind `json:"agent"`
	ConversationID string    `json:"conversation_id,omitempty"`
	BackendVersion string    `json:"backend_version,omitempty"`
}

type Settings struct {
	Model           string `json:"model,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

type WorkerLaunchSpec struct {
	SessionID        string
	SessionStoreRoot string
	Agent            AgentKind
	WorkingDirectory string
	SessionBinding   *BackendBinding
	Settings         Settings
}

type WorkerLifecycle struct {
	IdleTimeout time.Duration
}

type ProbeRequest struct{}

type Capabilities struct {
	Installed bool
	Version   string
	Models    []Model
}

type Model struct {
	ID                        string
	DisplayName               string
	Default                   bool
	SupportedReasoningEfforts []string
}

type Adapter interface {
	Kind() AgentKind
	Probe(context.Context, ProbeRequest) (Capabilities, error)
	StartWorker(context.Context, WorkerLaunchSpec) error
	WorkerLifecycle() WorkerLifecycle
}
