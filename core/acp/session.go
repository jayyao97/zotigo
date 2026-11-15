package acp

// Session represents an ACP session with its own conversation context.
type Session struct {
	ID         string
	WorkingDir string
	Mode       string // "code", "ask", "architect"
}

// NewSession creates a new ACP session.
func NewSession(id, workDir string) *Session {
	return &Session{
		ID:         id,
		WorkingDir: workDir,
		Mode:       "code",
	}
}
