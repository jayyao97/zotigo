package acp

import (
	"context"
	"sync"
)

// Session represents an ACP session with its own conversation context.
type Session struct {
	ID         string
	WorkingDir string
	Mode       string // "code", "ask", "architect"

	mu       sync.Mutex
	cancelFn context.CancelFunc
}

// NewSession creates a new ACP session.
func NewSession(id, workDir string) *Session {
	return &Session{
		ID:         id,
		WorkingDir: workDir,
		Mode:       "code",
	}
}

// SetCancel stores the cancel function for the current prompt processing.
func (s *Session) SetCancel(cancel context.CancelFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancelFn = cancel
}

// Cancel cancels any in-progress prompt processing.
func (s *Session) Cancel() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelFn != nil {
		s.cancelFn()
		s.cancelFn = nil
	}
}
