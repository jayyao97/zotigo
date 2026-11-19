package wire

// Header names of the worker connect handshake.
const (
	GenerationHeader    = "X-Zotigo-Worker-Generation"
	WorkspaceRootHeader = "X-Zotigo-Workspace-Root" // URL-escaped filesystem path.
	WorkspaceHeader     = "X-Zotigo-Workspace-ID"
)

type ReadyRequest struct {
	Generation string `json:"generation"`
}

type FinishSessionRequest struct {
	Error      string `json:"error,omitempty"`
	ErrorCode  string `json:"error_code,omitempty"`
	Generation string `json:"generation,omitempty"`
}
