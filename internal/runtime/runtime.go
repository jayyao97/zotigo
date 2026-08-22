package runtime

import (
	"context"
	"errors"
)

var (
	ErrWorkspaceNotFound        = errors.New("runtime workspace not found")
	ErrWorkspaceConflict        = errors.New("runtime workspace conflict")
	ErrWorkspaceCreateTombstone = errors.New("runtime workspace create key refers to deleted workspace")
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

type WorkspaceBinding struct {
	WorkspaceID    string
	Agent          AgentKind
	ExternalID     string
	Revision       uint64
	BackendVersion string
}

type WorkerLaunchSpec struct {
	SessionID        string
	SessionStoreRoot string
	Agent            AgentKind
	WorkingDirectory string
	SessionBinding   *BackendBinding
	WorkspaceBinding *WorkspaceBinding
	Settings         Settings
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
}

type ExternalWorkspace struct {
	ID       string
	Name     string
	RootPath string
	Metadata map[string]string
}

type WorkspaceSpec struct {
	WorkspaceID string
	Name        string
	RootPath    string
}

type WorkspaceCreateIntent struct {
	WorkspaceSpec
	IdempotencyKey string
}

type WorkspaceAdapter interface {
	ReadWorkspace(context.Context, string) (ExternalWorkspace, error)
	FindWorkspace(context.Context, WorkspaceSpec) (*ExternalWorkspace, error)
	CreateWorkspace(context.Context, WorkspaceCreateIntent) (ExternalWorkspace, error)
}
