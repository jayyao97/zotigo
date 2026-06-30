package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/bytedance/sonic"
)

const displayLogTailScanBlockSize = 4096

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

func (s *FileStore) AppendDisplayItem(ctx context.Context, id string, item DisplayItem) (DisplayItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := os.Stat(s.sessionPath(id)); err != nil {
		if os.IsNotExist(err) {
			return DisplayItem{}, fmt.Errorf("session not found: %s", id)
		}
		return DisplayItem{}, fmt.Errorf("stat session file: %w", err)
	}

	lastSequence, completeEndOffset, err := s.lastDisplaySequenceLocked(id)
	if err != nil {
		return DisplayItem{}, err
	}
	if completeEndOffset >= 0 {
		if err := s.truncateDisplayLogTailLocked(id, completeEndOffset); err != nil {
			return DisplayItem{}, err
		}
	}
	sequence := lastSequence + 1
	item.Sequence = sequence
	if item.ID == "" {
		item.ID = fmt.Sprintf("item_%s_%d", id, sequence)
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = time.Now().UTC()
	}

	data, err := sonic.Marshal(item)
	if err != nil {
		return DisplayItem{}, fmt.Errorf("marshal display item: %w", err)
	}
	file, err := os.OpenFile(s.displayLogPath(id), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return DisplayItem{}, fmt.Errorf("open display log: %w", err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Write(append(data, '\n')); err != nil {
		return DisplayItem{}, fmt.Errorf("write display log: %w", err)
	}
	return item, nil
}

func (s *FileStore) ListDisplayItems(ctx context.Context, id string) ([]DisplayItem, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if _, err := os.Stat(s.sessionPath(id)); err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("stat session file: %w", err)
	}
	items, err := s.readDisplayItemsLocked(id)
	if err != nil {
		return nil, true, err
	}
	return items, true, nil
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
	_ = os.Remove(s.displayLogPath(id))

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

func (s *FileStore) displayLogPath(id string) string {
	return filepath.Join(s.rootDir, "sessions", id+".display.jsonl")
}

func (s *FileStore) lockPath(id string) string {
	return filepath.Join(s.rootDir, "sessions", id+".lock")
}

func (s *FileStore) readDisplayItemsLocked(id string) ([]DisplayItem, error) {
	data, err := os.ReadFile(s.displayLogPath(id))
	if os.IsNotExist(err) {
		return []DisplayItem{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read display log: %w", err)
	}

	var items []DisplayItem
	lines := bytes.Split(data, []byte{'\n'})
	endedWithNewline := len(data) == 0 || data[len(data)-1] == '\n'
	for idx, line := range lines {
		if len(line) == 0 {
			continue
		}
		line = bytes.TrimSuffix(line, []byte{'\r'})
		var item DisplayItem
		if err := sonic.Unmarshal(line, &item); err != nil {
			if idx == len(lines)-1 && !endedWithNewline {
				break
			}
			return nil, fmt.Errorf("unmarshal display log item: %w", err)
		}
		items = append(items, item)
	}
	return items, nil
}

func (s *FileStore) lastDisplaySequenceLocked(id string) (uint64, int64, error) {
	path := s.displayLogPath(id)
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return 0, -1, nil
	}
	if err != nil {
		return 0, -1, fmt.Errorf("open display log: %w", err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return 0, -1, fmt.Errorf("stat display log: %w", err)
	}
	size := info.Size()
	if size == 0 {
		return 0, 0, nil
	}

	completeEndOffset := size
	lastByte, err := readFileByte(file, size-1)
	if err != nil {
		return 0, -1, err
	}
	if lastByte != '\n' {
		newlineOffset, err := previousDisplayLogNewline(file, size-1)
		if err != nil {
			return 0, -1, err
		}
		if newlineOffset < 0 {
			return 0, 0, nil
		}
		completeEndOffset = newlineOffset + 1
	}

	lineEnd := completeEndOffset
	for lineEnd > 0 {
		b, err := readFileByte(file, lineEnd-1)
		if err != nil {
			return 0, -1, err
		}
		if b != '\n' && b != '\r' {
			break
		}
		lineEnd--
	}
	if lineEnd == 0 {
		return 0, completeEndOffset, nil
	}

	lineStart, err := previousDisplayLogNewline(file, lineEnd)
	if err != nil {
		return 0, -1, err
	}
	lineStart++

	line := make([]byte, lineEnd-lineStart)
	if _, err := file.ReadAt(line, lineStart); err != nil {
		return 0, -1, fmt.Errorf("read display log tail: %w", err)
	}
	line = bytes.TrimSuffix(line, []byte{'\r'})

	var item DisplayItem
	if err := sonic.Unmarshal(line, &item); err != nil {
		return 0, -1, fmt.Errorf("unmarshal display log tail item: %w", err)
	}
	return item.Sequence, completeEndOffset, nil
}

func (s *FileStore) truncateDisplayLogTailLocked(id string, completeEndOffset int64) error {
	path := s.displayLogPath(id)
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat display log: %w", err)
	}
	if info.Size() <= completeEndOffset {
		return nil
	}
	if err := os.Truncate(path, completeEndOffset); err != nil {
		return fmt.Errorf("truncate display log tail: %w", err)
	}
	return nil
}

func previousDisplayLogNewline(file *os.File, before int64) (int64, error) {
	for offset := before; offset > 0; {
		readSize := int64(displayLogTailScanBlockSize)
		if offset < readSize {
			readSize = offset
		}
		start := offset - readSize
		buf := make([]byte, readSize)
		if _, err := file.ReadAt(buf, start); err != nil {
			return -1, fmt.Errorf("scan display log tail: %w", err)
		}
		for idx := len(buf) - 1; idx >= 0; idx-- {
			if buf[idx] == '\n' {
				return start + int64(idx), nil
			}
		}
		offset = start
	}
	return -1, nil
}

func readFileByte(file *os.File, offset int64) (byte, error) {
	var buf [1]byte
	if _, err := file.ReadAt(buf[:], offset); err != nil {
		return 0, fmt.Errorf("read display log byte: %w", err)
	}
	return buf[0], nil
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
