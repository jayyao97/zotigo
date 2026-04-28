package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"syscall"
)

// FileStore implements Store using the local filesystem.
// Sessions are stored as JSON files in a configurable directory.
type FileStore struct {
	rootDir      string
	registryPath string
	mu           sync.RWMutex
}

// NewFileStore creates a new file-based session store.
// If rootDir is empty, it defaults to ~/.zotigo
func NewFileStore(rootDir string) (*FileStore, error) {
	if rootDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("failed to get home directory: %w", err)
		}
		rootDir = filepath.Join(home, ".zotigo")
	}

	sessionsDir := filepath.Join(rootDir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create sessions directory: %w", err)
	}

	return &FileStore{
		rootDir:      rootDir,
		registryPath: filepath.Join(rootDir, "registry.json"),
	}, nil
}

// Get retrieves a session by ID.
func (s *FileStore) Get(ctx context.Context, id string) (*Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	path := s.sessionPath(id)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read session file: %w", err)
	}

	var sess Session
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, fmt.Errorf("failed to unmarshal session: %w", err)
	}
	sess.EnsureInitialized()
	return &sess, nil
}

// Put stores a session.
func (s *FileStore) Put(ctx context.Context, sess *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Write session file
	path := s.sessionPath(sess.ID)
	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal session: %w", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write session file: %w", err)
	}

	// Update registry
	return s.updateRegistry(sess.Metadata)
}

// Delete removes a session by ID.
func (s *FileStore) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Remove session file
	path := s.sessionPath(id)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete session file: %w", err)
	}

	// Remove lock file if exists
	lockPath := s.lockPath(id)
	_ = os.Remove(lockPath)

	// Update registry
	return s.removeFromRegistry(id)
}

// List returns all sessions matching the filter.
func (s *FileStore) List(ctx context.Context, filter ListFilter) ([]Metadata, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	reg, err := s.loadRegistry()
	if err != nil {
		return nil, err
	}

	var result []Metadata
	for _, meta := range reg.Sessions {
		if filter.WorkingDirectory != "" && meta.WorkingDirectory != filter.WorkingDirectory {
			continue
		}
		result = append(result, meta)
	}

	// Sort
	switch filter.OrderBy {
	case OrderByUpdatedDesc:
		sort.Slice(result, func(i, j int) bool {
			return result[i].UpdatedAt.After(result[j].UpdatedAt)
		})
	case OrderByUpdatedAsc:
		sort.Slice(result, func(i, j int) bool {
			return result[i].UpdatedAt.Before(result[j].UpdatedAt)
		})
	case OrderByCreatedDesc:
		sort.Slice(result, func(i, j int) bool {
			return result[i].CreatedAt.After(result[j].CreatedAt)
		})
	case OrderByCreatedAsc:
		sort.Slice(result, func(i, j int) bool {
			return result[i].CreatedAt.Before(result[j].CreatedAt)
		})
	}

	// Limit
	if filter.Limit > 0 && len(result) > filter.Limit {
		result = result[:filter.Limit]
	}

	return result, nil
}

// Lock acquires an exclusive lock on a session.
func (s *FileStore) Lock(ctx context.Context, id string) error {
	locked, err := s.IsLocked(ctx, id)
	if err != nil {
		return err
	}
	if locked {
		return fmt.Errorf("session %s is already locked", id)
	}

	lockPath := s.lockPath(id)
	pid := fmt.Sprintf("%d", os.Getpid())
	return os.WriteFile(lockPath, []byte(pid), 0644)
}

// Unlock releases the lock on a session.
func (s *FileStore) Unlock(ctx context.Context, id string) error {
	lockPath := s.lockPath(id)
	err := os.Remove(lockPath)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// IsLocked checks if a session is currently locked.
func (s *FileStore) IsLocked(ctx context.Context, id string) (bool, error) {
	lockPath := s.lockPath(id)
	data, err := os.ReadFile(lockPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return true, nil // Cannot read, assume locked for safety
	}

	pidStr := string(data)
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		// Corrupt lock file, clean it up
		_ = os.Remove(lockPath)
		return false, nil
	}

	// Check if process exists
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, nil
	}

	// Sending Signal 0 checks for existence without killing
	err = process.Signal(syscall.Signal(0))
	if err == nil {
		return true, nil // Process is running
	}

	// Process is dead, clean up stale lock
	_ = os.Remove(lockPath)
	return false, nil
}

// Close releases any resources.
func (s *FileStore) Close() error {
	return nil
}

// Helper methods

func (s *FileStore) sessionPath(id string) string {
	return filepath.Join(s.rootDir, "sessions", id+".json")
}

func (s *FileStore) lockPath(id string) string {
	return filepath.Join(s.rootDir, "sessions", id+".lock")
}

// Registry represents the index file structure.
type registry struct {
	Sessions []Metadata `json:"sessions"`
}

func (s *FileStore) loadRegistry() (*registry, error) {
	data, err := os.ReadFile(s.registryPath)
	if os.IsNotExist(err) {
		return &registry{Sessions: []Metadata{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read registry: %w", err)
	}

	var reg registry
	if err := json.Unmarshal(data, &reg); err != nil {
		// If corrupt, return empty
		return &registry{Sessions: []Metadata{}}, nil
	}
	return &reg, nil
}

func (s *FileStore) updateRegistry(meta Metadata) error {
	reg, err := s.loadRegistry()
	if err != nil {
		return err
	}

	found := false
	for i, m := range reg.Sessions {
		if m.ID == meta.ID {
			reg.Sessions[i] = meta
			found = true
			break
		}
	}
	if !found {
		reg.Sessions = append(reg.Sessions, meta)
	}

	return s.saveRegistry(reg)
}

func (s *FileStore) removeFromRegistry(id string) error {
	reg, err := s.loadRegistry()
	if err != nil {
		return err
	}

	newSessions := make([]Metadata, 0, len(reg.Sessions))
	for _, m := range reg.Sessions {
		if m.ID != id {
			newSessions = append(newSessions, m)
		}
	}
	reg.Sessions = newSessions

	return s.saveRegistry(reg)
}

func (s *FileStore) saveRegistry(reg *registry) error {
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal registry: %w", err)
	}
	return os.WriteFile(s.registryPath, data, 0644)
}

// Ensure FileStore implements Store
var _ Store = (*FileStore)(nil)
