package session

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
)

const RuntimeWALFormatVersion = 1

type RuntimeWALHeader struct {
	FormatVersion       int       `json:"format_version"`
	SessionID           string    `json:"session_id"`
	TurnID              string    `json:"turn_id,omitempty"`
	WALID               string    `json:"wal_id"`
	BaseSnapshotVersion uint64    `json:"base_snapshot_version"`
	BaseSnapshotDigest  string    `json:"base_snapshot_digest"`
	CreatedAt           time.Time `json:"created_at"`
}

type RuntimeWALRecord struct {
	FormatVersion        int                      `json:"format_version"`
	WALID                string                   `json:"wal_id"`
	Sequence             uint64                   `json:"sequence"`
	Mutation             agent.HistoryMutation    `json:"mutation,omitempty"`
	ToolExecutionStarted *RuntimeWALToolExecution `json:"tool_execution_started,omitempty"`
	Checksum             string                   `json:"checksum"`
}

type RuntimeWALToolExecution struct {
	ToolCallID string `json:"tool_call_id"`
	ToolName   string `json:"tool_name"`
	Arguments  string `json:"arguments,omitempty"`
}

type RuntimeWAL struct {
	Header  RuntimeWALHeader
	Records []RuntimeWALRecord
}

// RuntimeWALStore is deliberately separate from Store: remote stores are not
// forced to pretend they provide local append-and-fsync durability.
type RuntimeWALStore interface {
	BeginRuntimeWAL(context.Context, RuntimeWALHeader) error
	AppendRuntimeWAL(context.Context, string, RuntimeWALRecord) error
	LoadRuntimeWAL(context.Context, string) (*RuntimeWAL, error)
	DeleteRuntimeWAL(context.Context, string) error
}

func NewRuntimeWALHeader(sessionID string, baseVersion uint64, snapshot agent.Snapshot) (RuntimeWALHeader, error) {
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return RuntimeWALHeader{}, fmt.Errorf("create runtime WAL id: %w", err)
	}
	return RuntimeWALHeader{
		FormatVersion:       RuntimeWALFormatVersion,
		SessionID:           sessionID,
		WALID:               hex.EncodeToString(idBytes),
		BaseSnapshotVersion: baseVersion,
		BaseSnapshotDigest:  SnapshotDigest(snapshot),
		CreatedAt:           time.Now().UTC(),
	}, nil
}

func SnapshotDigest(snapshot agent.Snapshot) string {
	data, _ := json.Marshal(snapshot.History)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s *FileStore) BeginRuntimeWAL(_ context.Context, header RuntimeWALHeader) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if header.FormatVersion != RuntimeWALFormatVersion || header.SessionID == "" || header.WALID == "" {
		return fmt.Errorf("invalid runtime WAL header")
	}
	line, err := json.Marshal(header)
	if err != nil {
		return fmt.Errorf("marshal runtime WAL header: %w", err)
	}
	line = append(line, '\n')
	if err := writeFileAtomic(s.runtimeWALPath(header.SessionID), line, 0600); err != nil {
		return fmt.Errorf("write runtime WAL header: %w", err)
	}
	return nil
}

func (s *FileStore) AppendRuntimeWAL(_ context.Context, sessionID string, record RuntimeWALRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record.FormatVersion = RuntimeWALFormatVersion
	record.Checksum = runtimeWALChecksum(record)
	line, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal runtime WAL record: %w", err)
	}
	file, err := os.OpenFile(s.runtimeWALPath(sessionID), os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("open runtime WAL: %w", err)
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		_ = file.Close()
		return fmt.Errorf("append runtime WAL: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync runtime WAL: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close runtime WAL: %w", err)
	}
	return nil
}

func (s *FileStore) LoadRuntimeWAL(_ context.Context, sessionID string) (*RuntimeWAL, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, err := os.ReadFile(s.runtimeWALPath(sessionID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read runtime WAL: %w", err)
	}
	lines := bytes.Split(data, []byte{'\n'})
	if len(lines) < 2 {
		return nil, fmt.Errorf("runtime WAL header is incomplete")
	}
	var header RuntimeWALHeader
	if err := json.Unmarshal(lines[0], &header); err != nil {
		return nil, fmt.Errorf("decode runtime WAL header: %w", err)
	}
	if header.FormatVersion != RuntimeWALFormatVersion || header.SessionID != sessionID || header.WALID == "" {
		return nil, fmt.Errorf("runtime WAL header does not match session")
	}
	wal := &RuntimeWAL{Header: header}
	expected := uint64(1)
	for index, line := range lines[1:] {
		if len(line) == 0 {
			continue
		}
		var record RuntimeWALRecord
		if err := json.Unmarshal(line, &record); err != nil {
			if index == len(lines)-2 && data[len(data)-1] != '\n' {
				break
			}
			return nil, fmt.Errorf("decode runtime WAL record %d: %w", expected, err)
		}
		if record.FormatVersion != RuntimeWALFormatVersion || record.WALID != header.WALID || record.Sequence != expected {
			return nil, fmt.Errorf("runtime WAL record sequence or identity mismatch at %d", expected)
		}
		if record.Checksum == "" || record.Checksum != runtimeWALChecksum(record) {
			return nil, fmt.Errorf("runtime WAL checksum mismatch at %d", expected)
		}
		wal.Records = append(wal.Records, record)
		expected++
	}
	return wal, nil
}

func (s *FileStore) DeleteRuntimeWAL(_ context.Context, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.runtimeWALPath(sessionID)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete runtime WAL: %w", err)
	} else if os.IsNotExist(err) {
		return nil
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync runtime WAL directory: %w", err)
	}
	return nil
}

func runtimeWALChecksum(record RuntimeWALRecord) string {
	record.Checksum = ""
	data, _ := json.Marshal(record)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s *FileStore) runtimeWALPath(id string) string {
	return s.sessionPath(id) + ".runtime.jsonl"
}
