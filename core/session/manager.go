package session

import (
	"context"
	"fmt"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
)

// Metadata holds the summary info for listing/indexing.
type Metadata struct {
	ID               string    `json:"id"`
	WorkingDirectory string    `json:"working_directory"` // The project path this session belongs to
	LastPrompt       string    `json:"last_prompt"`       // Preview of the last user interaction
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// Session represents the full state on disk.
type Session struct {
	Metadata
	AgentSnapshot agent.Snapshot `json:"agent_snapshot"`
}

// Manager handles session storage, retrieval, and locking.
// It uses a Store backend for persistence.
type Manager struct {
	store Store
}

// NewManager creates a new Manager with the default FileStore.
func NewManager() (*Manager, error) {
	store, err := NewFileStore("")
	if err != nil {
		return nil, err
	}
	return &Manager{store: store}, nil
}

// NewManagerWithStore creates a new Manager with a custom Store backend.
func NewManagerWithStore(store Store) *Manager {
	return &Manager{store: store}
}

// CreateNew initializes a new session for the current directory.
func (m *Manager) CreateNew(workDir string) (*Session, error) {
	id := fmt.Sprintf("sess_%d", time.Now().UnixNano())
	sess := &Session{
		Metadata: Metadata{
			ID:               id,
			WorkingDirectory: workDir,
			CreatedAt:        time.Now(),
			UpdatedAt:        time.Now(),
		},
		AgentSnapshot: agent.Snapshot{
			State:     agent.StateIdle,
			CreatedAt: time.Now(),
		},
	}

	// Initial save to register it
	if err := m.Save(sess); err != nil {
		return nil, err
	}
	return sess, nil
}

// ListByDir returns all sessions for a given directory, sorted by UpdatedAt desc.
func (m *Manager) ListByDir(workDir string) ([]Metadata, error) {
	return m.store.List(context.Background(), ListFilter{
		WorkingDirectory: workDir,
		OrderBy:          OrderByUpdatedDesc,
	})
}

// Load reads a session from the store.
func (m *Manager) Load(id string) (*Session, error) {
	sess, err := m.store.Get(context.Background(), id)
	if err != nil {
		return nil, err
	}
	if sess == nil {
		return nil, fmt.Errorf("session not found: %s", id)
	}
	return sess, nil
}

// Save writes the session to the store.
func (m *Manager) Save(sess *Session) error {
	sess.UpdatedAt = time.Now()
	return m.store.Put(context.Background(), sess)
}

// Lock creates a lock on the session.
func (m *Manager) Lock(id string) error {
	return m.store.Lock(context.Background(), id)
}

// Unlock releases the lock on the session.
func (m *Manager) Unlock(id string) error {
	return m.store.Unlock(context.Background(), id)
}

// IsLocked checks if a session is currently locked.
func (m *Manager) IsLocked(id string) bool {
	locked, _ := m.store.IsLocked(context.Background(), id)
	return locked
}

// Close closes the underlying store.
func (m *Manager) Close() error {
	return m.store.Close()
}
