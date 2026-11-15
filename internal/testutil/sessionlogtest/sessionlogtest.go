// Package sessionlogtest provides session-log fixtures shared by the daemon's
// tests and the native worker's tests: an in-memory display item source and
// helpers that seed a stored session with a usable project profile.
package sessionlogtest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/config"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

// ItemSource is an in-memory display item source. It satisfies the
// sessionlog item-source interfaces; tests seed Items and inject failures
// through Err, AppendErr and AppendHook.
type ItemSource struct {
	Mu             sync.Mutex
	Items          map[string][]zotigosession.DisplayItem
	Err            error
	AppendErr      func(sessionID string, item zotigosession.DisplayItem) error
	AppendHook     func(sessionID string, item zotigosession.DisplayItem)
	OffsetCalls    atomic.Int32
	LoadCalls      atomic.Int32
	OffsetMaxLines []int
}

func (s *ItemSource) LoadItems(_ context.Context, sessionID string) ([]zotigosession.DisplayItem, bool, error) {
	s.LoadCalls.Add(1)
	s.Mu.Lock()
	defer s.Mu.Unlock()
	if s.Err != nil {
		return nil, false, s.Err
	}
	items, ok := s.Items[sessionID]
	items = append([]zotigosession.DisplayItem(nil), items...)
	return items, ok, nil
}

func (s *ItemSource) LoadItemsFromOffset(_ context.Context, sessionID string, offset int64, maxLines int) ([]zotigosession.DisplayItem, bool, int64, error) {
	s.Mu.Lock()
	defer s.Mu.Unlock()
	s.OffsetCalls.Add(1)
	s.OffsetMaxLines = append(s.OffsetMaxLines, maxLines)
	if s.Err != nil {
		return nil, false, offset, s.Err
	}
	items, ok := s.Items[sessionID]
	if !ok {
		return nil, false, offset, nil
	}
	if offset < 0 {
		return nil, true, offset, fmt.Errorf("offset must be non-negative")
	}
	start := int(offset)
	if start > len(items) {
		return nil, true, offset, fmt.Errorf("offset exceeds display log size")
	}
	end := len(items)
	if maxLines > 0 && start+maxLines < end {
		end = start + maxLines
	}
	result := append([]zotigosession.DisplayItem(nil), items[start:end]...)
	for idx := range result {
		result[idx].LogOffset = int64(start + idx + 1)
	}
	return result, true, int64(end), nil
}

func (s *ItemSource) AppendItem(_ context.Context, sessionID string, item zotigosession.DisplayItem) (zotigosession.DisplayItem, error) {
	if s.Err != nil {
		return zotigosession.DisplayItem{}, s.Err
	}
	if s.Items == nil {
		s.Items = make(map[string][]zotigosession.DisplayItem)
	}
	if s.AppendErr != nil {
		if err := s.AppendErr(sessionID, item); err != nil {
			return zotigosession.DisplayItem{}, err
		}
	}
	s.Mu.Lock()
	next := uint64(len(s.Items[sessionID]) + 1)
	item.Sequence = next
	if item.ID == "" {
		item.ID = fmt.Sprintf("item_%s_%d", sessionID, next)
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = time.Now().UTC()
	}
	s.Items[sessionID] = append(s.Items[sessionID], item)
	s.Mu.Unlock()
	if s.AppendHook != nil {
		s.AppendHook(sessionID, item)
	}
	return item, nil
}

func (s *ItemSource) AppendItemIf(ctx context.Context, sessionID string, item zotigosession.DisplayItem, condition func([]zotigosession.DisplayItem) error) (zotigosession.DisplayItem, error) {
	if s.Err != nil {
		return zotigosession.DisplayItem{}, s.Err
	}
	if s.Items == nil {
		s.Items = make(map[string][]zotigosession.DisplayItem)
	}
	if s.AppendErr != nil {
		if err := s.AppendErr(sessionID, item); err != nil {
			return zotigosession.DisplayItem{}, err
		}
	}
	s.Mu.Lock()
	items := append([]zotigosession.DisplayItem(nil), s.Items[sessionID]...)
	if condition != nil {
		if err := condition(items); err != nil {
			s.Mu.Unlock()
			return zotigosession.DisplayItem{}, err
		}
	}
	next := uint64(len(s.Items[sessionID]) + 1)
	item.Sequence = next
	if item.ID == "" {
		item.ID = fmt.Sprintf("item_%s_%d", sessionID, next)
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = time.Now().UTC()
	}
	s.Items[sessionID] = append(s.Items[sessionID], item)
	s.Mu.Unlock()
	if s.AppendHook != nil {
		s.AppendHook(sessionID, item)
	}
	return item, nil
}

func (s *ItemSource) AppendItemAtomically(_ context.Context, sessionID string, build func([]zotigosession.DisplayItem) (zotigosession.DisplayItem, bool, error)) (zotigosession.DisplayItem, bool, error) {
	if s.Err != nil {
		return zotigosession.DisplayItem{}, false, s.Err
	}
	s.Mu.Lock()
	items := append([]zotigosession.DisplayItem(nil), s.Items[sessionID]...)
	item, appendItem, err := build(items)
	if err != nil || !appendItem {
		s.Mu.Unlock()
		return item, false, err
	}
	if s.AppendErr != nil {
		if err := s.AppendErr(sessionID, item); err != nil {
			s.Mu.Unlock()
			return zotigosession.DisplayItem{}, false, err
		}
	}
	next := uint64(len(s.Items[sessionID]) + 1)
	item.Sequence = next
	if item.ID == "" {
		item.ID = fmt.Sprintf("item_%s_%d", sessionID, next)
	}
	if item.CreatedAt.IsZero() {
		item.CreatedAt = time.Now().UTC()
	}
	s.Items[sessionID] = append(s.Items[sessionID], item)
	s.Mu.Unlock()
	if s.AppendHook != nil {
		s.AppendHook(sessionID, item)
	}
	return item, true, nil
}

func (s *ItemSource) AppendItems(_ context.Context, sessionID string, items []zotigosession.DisplayItem) ([]zotigosession.DisplayItem, error) {
	return s.appendItemsAfter(sessionID, nil, items)
}

func (s *ItemSource) AppendItemsAfter(_ context.Context, sessionID string, expectedSequence uint64, items []zotigosession.DisplayItem) ([]zotigosession.DisplayItem, error) {
	return s.appendItemsAfter(sessionID, &expectedSequence, items)
}

func (s *ItemSource) appendItemsAfter(sessionID string, expectedSequence *uint64, items []zotigosession.DisplayItem) ([]zotigosession.DisplayItem, error) {
	if s.Err != nil {
		return nil, s.Err
	}
	for _, item := range items {
		if s.AppendErr != nil {
			if err := s.AppendErr(sessionID, item); err != nil {
				return nil, err
			}
		}
	}
	s.Mu.Lock()
	latest := uint64(0)
	if existing := s.Items[sessionID]; len(existing) > 0 {
		latest = existing[len(existing)-1].Sequence
	}
	if expectedSequence != nil && latest != *expectedSequence {
		s.Mu.Unlock()
		return nil, zotigosession.ErrDisplayLogChanged
	}
	if s.Items == nil {
		s.Items = make(map[string][]zotigosession.DisplayItem)
	}
	stored := append([]zotigosession.DisplayItem(nil), items...)
	for index := range stored {
		stored[index].Sequence = uint64(len(s.Items[sessionID]) + 1)
		if stored[index].ID == "" {
			stored[index].ID = fmt.Sprintf("item_%s_%d", sessionID, stored[index].Sequence)
		}
		if stored[index].CreatedAt.IsZero() {
			stored[index].CreatedAt = time.Now().UTC()
		}
		s.Items[sessionID] = append(s.Items[sessionID], stored[index])
	}
	s.Mu.Unlock()
	if s.AppendHook != nil {
		for _, item := range stored {
			s.AppendHook(sessionID, item)
		}
	}
	return stored, nil
}

// WriteProfileConfig writes a minimal project config with a "test" profile
// into workDir unless one already exists.
func WriteProfileConfig(t *testing.T, workDir string) {
	t.Helper()

	configPath := filepath.Join(workDir, config.ProjectConfig)
	if _, err := os.Stat(configPath); err == nil {
		return
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat project config: %v", err)
	}
	const contents = "default_profile: test\nprofiles:\n  test:\n    provider: openai\n    model: test\n"
	if err := os.WriteFile(configPath, []byte(contents), 0644); err != nil {
		t.Fatalf("write project config: %v", err)
	}
}

// PutStoredSession stores an idle session for workDir using the "test" profile.
func PutStoredSession(t *testing.T, store zotigosession.Store, id string, workDir string) {
	t.Helper()
	WriteProfileConfig(t, workDir)
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID:               id,
			WorkingDirectory: workDir,
			ProfileName:      "test",
			CreatedAt:        now,
			UpdatedAt:        now,
		},
		AgentSnapshot: agent.Snapshot{
			State:     agent.StateIdle,
			CreatedAt: now,
		},
		Turns: make([]zotigosession.Turn, 0),
	}); err != nil {
		t.Fatalf("put stored session: %v", err)
	}
}
