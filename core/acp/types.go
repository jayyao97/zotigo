// Package acp implements the Agent Client Protocol (ACP) for editor-agent communication.
// ACP is a JSON-RPC 2.0 based protocol over stdio, analogous to LSP but for AI coding agents.
// Spec: https://agentclientprotocol.com
package acp

// ACP method names (JSON-RPC 2.0).
const (
	// Client → Agent requests
	MethodInitialize     = "initialize"
	MethodAuthenticate   = "authenticate"
	MethodSessionNew     = "session/new"
	MethodSessionLoad    = "session/load"
	MethodSessionList    = "session/list"
	MethodSessionPrompt  = "session/prompt"
	MethodSessionCancel  = "session/cancel"
	MethodSessionSetMode = "session/set_mode"
	MethodSessionSetCfg  = "session/set_config_option"

	// Agent → Client notifications
	MethodSessionUpdate = "session/update"

	// Agent → Client requests (delegated operations)
	MethodFSReadTextFile  = "fs/read_text_file"
	MethodFSWriteTextFile = "fs/write_text_file"
	MethodTerminalCreate  = "terminal/create"
	MethodTerminalOutput  = "terminal/output"
	MethodTerminalWait    = "terminal/wait_for_exit"
	MethodTerminalKill    = "terminal/kill"
	MethodTerminalRelease = "terminal/release"
	MethodRequestPerm     = "session/request_permission"
)

// --- Initialize ---

type InitializeParams struct {
	ProtocolVersion int                `json:"protocolVersion"`
	ClientInfo      ClientInfo         `json:"clientInfo"`
	Capabilities    ClientCapabilities `json:"capabilities"`
}

type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type ClientCapabilities struct {
	Filesystem *FSCapabilities       `json:"filesystem,omitempty"`
	Terminal   *TerminalCapabilities `json:"terminal,omitempty"`
}

type FSCapabilities struct {
	ReadTextFile  bool `json:"readTextFile,omitempty"`
	WriteTextFile bool `json:"writeTextFile,omitempty"`
}

type TerminalCapabilities struct {
	Create bool `json:"create,omitempty"`
}

type InitializeResult struct {
	ProtocolVersion int               `json:"protocolVersion"`
	AgentInfo       AgentInfo         `json:"agentInfo"`
	Capabilities    AgentCapabilities `json:"capabilities"`
}

type AgentInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

type AgentCapabilities struct {
	LoadSession      bool     `json:"loadSession,omitempty"`
	AvailableModes   []string `json:"availableModes,omitempty"`
	SupportedPrompts []string `json:"supportedPrompts,omitempty"`
}

// --- Authentication ---

type AuthenticateParams struct {
	Token string `json:"token,omitempty"`
}

type AuthenticateResult struct {
	Success bool   `json:"success"`
	Message string `json:"message,omitempty"`
}

// --- Session ---

type SessionNewParams struct {
	WorkingDirectory string   `json:"workingDirectory"`
	MCPServers       []string `json:"mcpServers,omitempty"`
}

type SessionNewResult struct {
	SessionID string `json:"sessionId"`
}

type SessionLoadParams struct {
	SessionID string `json:"sessionId"`
}

type SessionLoadResult struct {
	SessionID string `json:"sessionId"`
}

type SessionListResult struct {
	Sessions []SessionInfo `json:"sessions"`
}

type SessionInfo struct {
	SessionID  string `json:"sessionId"`
	Title      string `json:"title,omitempty"`
	WorkingDir string `json:"workingDirectory,omitempty"`
	CreatedAt  string `json:"createdAt,omitempty"`
}

// --- Prompt ---

type SessionPromptParams struct {
	SessionID string         `json:"sessionId"`
	Content   []ContentBlock `json:"content"`
}

// ContentBlock is a union type for prompt content.
type ContentBlock struct {
	Type string `json:"type"` // "text", "image", "audio", "resource_link", "resource"
	// Text fields
	Text string `json:"text,omitempty"`
	// Image fields
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	// Resource fields
	URI string `json:"uri,omitempty"`
}

type SessionPromptResult struct {
	// empty — actual response comes via session/update notifications
}

// --- Session Update (Agent → Client notification) ---

type SessionUpdateParams struct {
	SessionID string        `json:"sessionId"`
	Update    SessionUpdate `json:"update"`
}

type SessionUpdate struct {
	Type string `json:"type"` // discriminator

	// agent_message_chunk
	MessageChunk *MessageChunk `json:"messageChunk,omitempty"`

	// tool_call_update
	ToolCallUpdate *ToolCallUpdate `json:"toolCallUpdate,omitempty"`

	// agent_plan
	Plan *AgentPlan `json:"plan,omitempty"`

	// session_info_update
	SessionInfo *SessionInfoUpdate `json:"sessionInfo,omitempty"`
}

type MessageChunk struct {
	Role    string `json:"role"` // "assistant"
	Content string `json:"content"`
}

type ToolCallUpdate struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Status    string `json:"status,omitempty"` // "running", "completed", "failed"
	Result    string `json:"result,omitempty"`
}

type AgentPlan struct {
	Entries []PlanEntry `json:"entries"`
}

type PlanEntry struct {
	Title    string `json:"title"`
	Status   string `json:"status,omitempty"` // "pending", "in_progress", "completed"
	Priority string `json:"priority,omitempty"`
}

type SessionInfoUpdate struct {
	Title string `json:"title,omitempty"`
}

// --- Cancel ---

type SessionCancelParams struct {
	SessionID string `json:"sessionId"`
}

// --- Mode ---

type SessionSetModeParams struct {
	SessionID string `json:"sessionId"`
	Mode      string `json:"mode"`
}

type SessionSetModeResult struct {
	Mode string `json:"mode"`
}

// --- Filesystem (Agent → Client) ---

type FSReadTextFileParams struct {
	Path string `json:"path"`
}

type FSReadTextFileResult struct {
	Content string `json:"content"`
}

type FSWriteTextFileParams struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type FSWriteTextFileResult struct {
	Success bool `json:"success"`
}

// --- Terminal (Agent → Client) ---

type TerminalCreateParams struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
	Env     []string `json:"env,omitempty"`
}

type TerminalCreateResult struct {
	TerminalID string `json:"terminalId"`
}

type TerminalOutputParams struct {
	TerminalID string `json:"terminalId"`
}

type TerminalOutputResult struct {
	Output string `json:"output"`
}

type TerminalWaitParams struct {
	TerminalID string `json:"terminalId"`
}

type TerminalWaitResult struct {
	ExitCode int `json:"exitCode"`
}

type TerminalKillParams struct {
	TerminalID string `json:"terminalId"`
}

type TerminalReleaseParams struct {
	TerminalID string `json:"terminalId"`
}

// --- Permission (Agent → Client) ---

type RequestPermissionParams struct {
	SessionID   string             `json:"sessionId"`
	Permissions []PermissionDetail `json:"permissions"`
}

type PermissionDetail struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
}

type RequestPermissionResult struct {
	Decisions []PermissionDecision `json:"decisions"`
}

type PermissionDecision struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // "allow_once", "allow_always", "reject_once", "reject_always"
}
