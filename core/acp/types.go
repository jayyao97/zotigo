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
	MethodSessionSetMode = "session/set_mode"
	MethodSessionSetCfg  = "session/set_config_option"

	// Client → Agent notifications
	MethodSessionCancel = "session/cancel"

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

// StopReason values per ACP spec.
const (
	StopReasonEndTurn         = "end_turn"
	StopReasonMaxTokens       = "max_tokens"
	StopReasonMaxTurnRequests = "max_turn_requests"
	StopReasonRefusal         = "refusal"
	StopReasonCancelled       = "cancelled"
)

// ToolCallStatus values per ACP spec.
const (
	ToolCallStatusPending    = "pending"
	ToolCallStatusInProgress = "in_progress"
	ToolCallStatusCompleted  = "completed"
	ToolCallStatusFailed     = "failed"
)

// --- Initialize ---

type InitializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientInfo         *Implementation    `json:"clientInfo,omitempty"`
	ClientCapabilities ClientCapabilities `json:"clientCapabilities"`
}

type Implementation struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Title   string `json:"title,omitempty"`
}

type ClientCapabilities struct {
	FS       FSCapabilities `json:"fs"`
	Terminal bool           `json:"terminal"`
}

type FSCapabilities struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

type InitializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentInfo         *Implementation   `json:"agentInfo,omitempty"`
	AgentCapabilities AgentCapabilities `json:"agentCapabilities"`
	AuthMethods       []AuthMethod      `json:"authMethods,omitempty"`
}

type AgentCapabilities struct {
	LoadSession        bool                `json:"loadSession"`
	PromptCapabilities *PromptCapabilities `json:"promptCapabilities,omitempty"`
}

type PromptCapabilities struct {
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
	Image           bool `json:"image"`
}

type AuthMethod struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// --- Authentication ---

type AuthenticateParams struct {
	MethodID string `json:"methodId"`
}

type AuthenticateResult struct{}

// --- Session ---

type SessionNewParams struct {
	Cwd        string      `json:"cwd"`
	MCPServers []MCPServer `json:"mcpServers"`
}

type MCPServer struct {
	Name    string        `json:"name"`
	Type    string        `json:"type,omitempty"` // "http", "sse"; absent for stdio
	Command string        `json:"command,omitempty"`
	Args    []string      `json:"args,omitempty"`
	URL     string        `json:"url,omitempty"`
	Env     []EnvVariable `json:"env,omitempty"`
}

type EnvVariable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type SessionNewResult struct {
	SessionID string `json:"sessionId"`
}

type SessionLoadParams struct {
	SessionID  string      `json:"sessionId"`
	Cwd        string      `json:"cwd"`
	MCPServers []MCPServer `json:"mcpServers"`
}

type SessionLoadResult struct{}

type SessionListParams struct {
	Cursor string `json:"cursor,omitempty"`
	Cwd    string `json:"cwd,omitempty"`
}

type SessionListResult struct {
	Sessions   []SessionInfo `json:"sessions"`
	NextCursor string        `json:"nextCursor,omitempty"`
}

type SessionInfo struct {
	SessionID string `json:"sessionId"`
	Title     string `json:"title,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
}

// --- Prompt ---

type SessionPromptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

// ContentBlock is a union type for prompt content (discriminated on Type).
type ContentBlock struct {
	Type string `json:"type"` // "text", "image", "audio", "resource_link", "resource"
	// Text fields
	Text string `json:"text,omitempty"`
	// Image/Audio fields
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	// Resource fields
	URI  string `json:"uri,omitempty"`
	Name string `json:"name,omitempty"`
}

type SessionPromptResult struct {
	StopReason string `json:"stopReason"`
}

// --- Session Update (Agent → Client notification) ---

type SessionUpdateParams struct {
	SessionID string        `json:"sessionId"`
	Update    SessionUpdate `json:"update"`
}

// SessionUpdate is sent as the "update" field in session/update notifications.
// It is a discriminated union keyed on "sessionUpdate".
// We represent it as a map to avoid Go struct tag collisions between variants.
type SessionUpdate = map[string]any

type ToolCallContent struct {
	Type string `json:"type"` // "content", "diff", "terminal"
	// content variant
	ContentBlock *ContentBlock `json:"content,omitempty"`
	// diff variant
	Path    string `json:"path,omitempty"`
	NewText string `json:"newText,omitempty"`
	OldText string `json:"oldText,omitempty"`
	// terminal variant
	TerminalID string `json:"terminalId,omitempty"`
}

type PlanEntry struct {
	Content  string `json:"content"`
	Status   string `json:"status"`   // "pending", "in_progress", "completed"
	Priority string `json:"priority"` // "high", "medium", "low"
}

type SessionInfoUpdateData struct {
	Title     string `json:"title,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// --- Cancel (Client → Agent notification) ---

type SessionCancelParams struct {
	SessionID string `json:"sessionId"`
}

// --- Mode ---

type SessionSetModeParams struct {
	SessionID string `json:"sessionId"`
	ModeID    string `json:"modeId"`
}

type SessionSetModeResult struct{}

// --- Filesystem (Agent → Client) ---

type FSReadTextFileParams struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Limit     int    `json:"limit,omitempty"`
	Line      int    `json:"line,omitempty"`
}

type FSReadTextFileResult struct {
	Content string `json:"content"`
}

type FSWriteTextFileParams struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Content   string `json:"content"`
}

type FSWriteTextFileResult struct{}

// --- Terminal (Agent → Client) ---

type TerminalCreateParams struct {
	SessionID       string        `json:"sessionId"`
	Command         string        `json:"command"`
	Args            []string      `json:"args,omitempty"`
	Cwd             string        `json:"cwd,omitempty"`
	Env             []EnvVariable `json:"env,omitempty"`
	OutputByteLimit int           `json:"outputByteLimit,omitempty"`
}

type TerminalCreateResult struct {
	TerminalID string `json:"terminalId"`
}

type TerminalOutputParams struct {
	SessionID  string `json:"sessionId"`
	TerminalID string `json:"terminalId"`
}

type TerminalOutputResult struct {
	Output     string              `json:"output"`
	Truncated  bool                `json:"truncated"`
	ExitStatus *TerminalExitStatus `json:"exitStatus,omitempty"`
}

type TerminalExitStatus struct {
	ExitCode *int   `json:"exitCode,omitempty"`
	Signal   string `json:"signal,omitempty"`
}

type TerminalWaitParams struct {
	SessionID  string `json:"sessionId"`
	TerminalID string `json:"terminalId"`
}

type TerminalWaitResult struct {
	ExitCode *int   `json:"exitCode,omitempty"`
	Signal   string `json:"signal,omitempty"`
}

type TerminalKillParams struct {
	SessionID  string `json:"sessionId"`
	TerminalID string `json:"terminalId"`
}

type TerminalKillResult struct{}

type TerminalReleaseParams struct {
	SessionID  string `json:"sessionId"`
	TerminalID string `json:"terminalId"`
}

type TerminalReleaseResult struct{}

// --- Permission (Agent → Client) ---

type RequestPermissionParams struct {
	SessionID string             `json:"sessionId"`
	ToolCall  ToolCallData       `json:"toolCall"`
	Options   []PermissionOption `json:"options"`
}

type ToolCallData struct {
	ToolCallID string            `json:"toolCallId"`
	Title      string            `json:"title"`
	Kind       string            `json:"kind,omitempty"`
	Status     string            `json:"status,omitempty"`
	Content    []ToolCallContent `json:"content,omitempty"`
	RawInput   any               `json:"rawInput,omitempty"`
	RawOutput  any               `json:"rawOutput,omitempty"`
}

type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"` // "allow_once", "allow_always", "reject_once", "reject_always"
}

type RequestPermissionResult struct {
	Outcome PermissionOutcome `json:"outcome"`
}

// PermissionOutcome is a discriminated union on the Outcome field.
type PermissionOutcome struct {
	Outcome  string `json:"outcome"`            // "selected" or "cancelled"
	OptionID string `json:"optionId,omitempty"` // only for "selected"
}
