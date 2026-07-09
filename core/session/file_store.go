package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/bytedance/sonic"
)

const (
	displayLogTailScanBlockSize = 4096
	lockFileWriteGrace          = time.Second
)

var displayLogAppendMu sync.Mutex

// FileStore implements Store using the local filesystem.
// Sessions are stored as JSON files in a configurable directory.
type FileStore struct {
	rootDir      string
	registryPath string
	index        *sessionIndex
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

	index, err := openSessionIndex(filepath.Join(rootDir, "session_index.sqlite"))
	if err != nil {
		return nil, err
	}

	store := &FileStore{
		rootDir:      rootDir,
		registryPath: filepath.Join(rootDir, "registry.json"),
		index:        index,
	}
	if err := store.bootstrapSessionIndex(); err != nil {
		_ = index.close()
		return nil, err
	}
	return store, nil
}

func (s *FileStore) RootDir() string {
	return s.rootDir
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
	if err := s.index.upsert(ctx, sess.Metadata); err != nil {
		return err
	}

	// Keep the legacy registry file updated for compatibility. List uses SQLite.
	if err := s.updateRegistry(sess.Metadata); err != nil {
		return err
	}
	return s.recordLegacyRegistryMTime(ctx)
}

func (s *FileStore) AppendDisplayItem(ctx context.Context, id string, item DisplayItem) (DisplayItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.appendDisplayItemLocked(ctx, id, item, nil)
}

func (s *FileStore) AppendDisplayItemIf(ctx context.Context, id string, item DisplayItem, condition func([]DisplayItem) error) (DisplayItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.appendDisplayItemLocked(ctx, id, item, condition)
}

func (s *FileStore) appendDisplayItemLocked(ctx context.Context, id string, item DisplayItem, condition func([]DisplayItem) error) (DisplayItem, error) {
	if _, err := os.Stat(s.sessionPath(id)); err != nil {
		if os.IsNotExist(err) {
			return DisplayItem{}, fmt.Errorf("session not found: %s", id)
		}
		return DisplayItem{}, fmt.Errorf("stat session file: %w", err)
	}
	displayLogAppendMu.Lock()
	defer displayLogAppendMu.Unlock()
	unlock, err := s.lockDisplayLogAppendLocked(ctx, id)
	if err != nil {
		return DisplayItem{}, err
	}
	defer unlock()

	if condition != nil {
		items, err := s.readDisplayItemsLocked(id)
		if err != nil {
			return DisplayItem{}, err
		}
		if err := condition(items); err != nil {
			return DisplayItem{}, err
		}
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

func (s *FileStore) ListDisplayItemsFromOffset(ctx context.Context, id string, offset int64, maxLines int) ([]DisplayItem, bool, int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if offset < 0 {
		return nil, false, offset, fmt.Errorf("offset must be non-negative")
	}
	if _, err := os.Stat(s.sessionPath(id)); err != nil {
		if os.IsNotExist(err) {
			return nil, false, offset, nil
		}
		return nil, false, offset, fmt.Errorf("stat session file: %w", err)
	}
	items, nextOffset, err := s.readDisplayItemsFromOffsetLocked(id, offset, maxLines)
	if err != nil {
		return nil, true, offset, err
	}
	return items, true, nextOffset, nil
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
	_ = os.Remove(s.displayLogAppendLockPath(id))
	if err := os.RemoveAll(s.imageBlobDir(id)); err != nil {
		return fmt.Errorf("failed to delete session image blobs: %w", err)
	}
	if err := s.index.delete(ctx, id); err != nil {
		return err
	}

	// Keep the legacy registry file updated for compatibility. List uses SQLite.
	if err := s.removeFromRegistry(id); err != nil {
		return err
	}
	return s.recordLegacyRegistryMTime(ctx)
}

// List returns all sessions matching the filter.
func (s *FileStore) List(ctx context.Context, filter ListFilter) ([]Metadata, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.index.list(ctx, filter)
}

// Lock acquires an exclusive lock on a session.
func (s *FileStore) Lock(ctx context.Context, id string) error {
	lockPath := s.lockPath(id)
	pid := fmt.Sprintf("%d", os.Getpid())
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		file, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err == nil {
			if _, err := file.WriteString(pid); err != nil {
				_ = file.Close()
				_ = os.Remove(lockPath)
				return err
			}
			return file.Close()
		}
		if !os.IsExist(err) {
			return err
		}
		locked, err := s.IsLocked(ctx, id)
		if err != nil {
			return err
		}
		if locked {
			return fmt.Errorf("session %s is already locked", id)
		}
	}
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
	return isPIDLockActive(s.lockPath(id))
}

func isPIDLockActive(lockPath string) (bool, error) {
	data, err := os.ReadFile(lockPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return true, nil // Cannot read, assume locked for safety
	}

	pidStr := string(data)
	if pidStr == "" {
		info, statErr := os.Stat(lockPath)
		if statErr != nil {
			return true, nil
		}
		if time.Since(info.ModTime()) < lockFileWriteGrace {
			return true, nil
		}
		if removeErr := os.Remove(lockPath); removeErr != nil && !os.IsNotExist(removeErr) {
			return true, fmt.Errorf("remove empty lock file: %w", removeErr)
		}
		return false, nil
	}

	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		// Corrupt lock file, clean it up
		if removeErr := os.Remove(lockPath); removeErr != nil && !os.IsNotExist(removeErr) {
			return true, fmt.Errorf("remove corrupt lock file: %w", removeErr)
		}
		return false, nil
	}

	running, err := pidRunning(pid)
	if err != nil {
		return true, err
	}
	if running {
		return true, nil // Process is running
	}

	// Process is dead, clean up stale lock
	if removeErr := os.Remove(lockPath); removeErr != nil && !os.IsNotExist(removeErr) {
		return true, fmt.Errorf("remove stale lock file: %w", removeErr)
	}
	return false, nil
}

// Close releases any resources.
func (s *FileStore) Close() error {
	if s.index != nil {
		return s.index.close()
	}
	return nil
}

// Helper methods

func (s *FileStore) sessionPath(id string) string {
	return filepath.Join(s.rootDir, "sessions", id+".json")
}

func (s *FileStore) displayLogPath(id string) string {
	return filepath.Join(s.rootDir, "sessions", id+".display.jsonl")
}

func (s *FileStore) displayLogAppendLockPath(id string) string {
	return filepath.Join(s.rootDir, "sessions", id+".display.lock")
}

func (s *FileStore) imageBlobDir(id string) string {
	return filepath.Join(s.rootDir, "sessions", id+".images")
}

func (s *FileStore) lockPath(id string) string {
	return filepath.Join(s.rootDir, "sessions", id+".lock")
}

func (s *FileStore) lockDisplayLogAppendLocked(ctx context.Context, id string) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	lockPath := s.displayLogAppendLockPath(id)
	pid := fmt.Sprintf("%d", os.Getpid())
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		file, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
		if err == nil {
			if _, err := file.WriteString(pid); err != nil {
				_ = file.Close()
				_ = os.Remove(lockPath)
				return nil, fmt.Errorf("write display log append lock: %w", err)
			}
			if err := file.Close(); err != nil {
				_ = os.Remove(lockPath)
				return nil, fmt.Errorf("close display log append lock: %w", err)
			}
			return func() {
				_ = os.Remove(lockPath)
			}, nil
		}
		if !os.IsExist(err) {
			return nil, fmt.Errorf("create display log append lock: %w", err)
		}
		locked, err := isPIDLockActive(lockPath)
		if err != nil {
			return nil, err
		}
		if !locked {
			continue
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
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
		if idx == len(lines)-1 && !endedWithNewline {
			break
		}
		if len(line) == 0 {
			continue
		}
		line = bytes.TrimSuffix(line, []byte{'\r'})
		var item DisplayItem
		if err := sonic.Unmarshal(line, &item); err != nil {
			return nil, fmt.Errorf("unmarshal display log item: %w", err)
		}
		items = append(items, item)
	}
	return items, nil
}

func (s *FileStore) readDisplayItemsFromOffsetLocked(id string, offset int64, maxLines int) ([]DisplayItem, int64, error) {
	path := s.displayLogPath(id)
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return []DisplayItem{}, offset, nil
	}
	if err != nil {
		return nil, offset, fmt.Errorf("open display log: %w", err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return nil, offset, fmt.Errorf("stat display log: %w", err)
	}
	if offset > info.Size() {
		return nil, offset, fmt.Errorf("offset exceeds display log size")
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, fmt.Errorf("seek display log: %w", err)
	}

	reader := bufio.NewReader(file)
	nextOffset := offset
	items := make([]DisplayItem, 0)
	for maxLines <= 0 || len(items) < maxLines {
		line, err := reader.ReadBytes('\n')
		if len(line) == 0 && errors.Is(err, io.EOF) {
			break
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, nextOffset, fmt.Errorf("read display log line: %w", err)
		}
		if errors.Is(err, io.EOF) && (len(line) == 0 || line[len(line)-1] != '\n') {
			break
		}
		nextOffset += int64(len(line))
		line = bytes.TrimSuffix(line, []byte{'\n'})
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(line) == 0 {
			if errors.Is(err, io.EOF) {
				break
			}
			continue
		}
		var item DisplayItem
		if err := sonic.Unmarshal(line, &item); err != nil {
			return nil, nextOffset, fmt.Errorf("unmarshal display log item: %w", err)
		}
		item.LogOffset = nextOffset
		items = append(items, item)
		if errors.Is(err, io.EOF) {
			break
		}
	}
	return items, nextOffset, nil
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

func (s *FileStore) loadRegistryStrict() (*registry, error) {
	data, err := os.ReadFile(s.registryPath)
	if os.IsNotExist(err) {
		return &registry{Sessions: []Metadata{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read registry: %w", err)
	}
	var reg registry
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal registry: %w", err)
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
