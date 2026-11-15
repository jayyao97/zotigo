package zotigod

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
	"github.com/jayyao97/zotigo/internal/codexapp"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
)

const (
	codexSessionSyncInterval = 10 * time.Second
	codexSessionSyncVersion  = 3
)

type codexSessionSyncFailure struct {
	sessionID string
	err       error
}

func (e *codexSessionSyncFailure) Error() string {
	return fmt.Sprintf("sync session %s: %v", e.sessionID, e.err)
}

func (e *codexSessionSyncFailure) Unwrap() error { return e.err }

type codexSessionSyncer struct {
	host       codexapp.HostProvider
	store      zotigosession.Store
	items      sessionlog.ItemSource
	catalog    *zotigoworkspace.Store
	registry   *sessionRegistry
	sessionOps *sessionOperationLocks
	mu         sync.Mutex
	lastCheck  time.Time
}

type backendActivityUpdater interface {
	UpdateBackendActivity(context.Context, string, time.Time, int) error
}

func newCodexSessionSyncer(host codexapp.HostProvider, store zotigosession.Store, items sessionlog.ItemSource, catalog *zotigoworkspace.Store, registry *sessionRegistry, sessionOps *sessionOperationLocks) *codexSessionSyncer {
	return &codexSessionSyncer{host: host, store: store, items: items, catalog: catalog, registry: registry, sessionOps: sessionOps}
}

func (s *codexSessionSyncer) Sync(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.lastCheck) < codexSessionSyncInterval {
		return nil
	}
	metadata, err := s.store.List(ctx, zotigosession.ListFilter{OrderBy: zotigosession.OrderByUpdatedDesc})
	if err != nil {
		return fmt.Errorf("list sessions for Codex sync: %w", err)
	}
	candidates := make([]zotigosession.Metadata, 0)
	for _, meta := range metadata {
		if meta.Agent != "codex" || meta.ConversationID == "" {
			continue
		}
		candidates = append(candidates, meta)
	}
	if len(candidates) == 0 {
		s.lastCheck = time.Now()
		return nil
	}

	lease, err := s.host.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire Codex metadata connection: %w", err)
	}
	defer func() { _ = lease.Release() }()
	activity, err := listCodexThreadActivity(ctx, lease.RPC, candidates)
	if err != nil {
		return err
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		return activity[candidates[i].ConversationID].Before(activity[candidates[j].ConversationID])
	})
	var syncErrors []error
	for _, meta := range candidates {
		backendUpdatedAt, ok := activity[meta.ConversationID]
		if !ok || (!backendUpdatedAt.After(meta.BackendUpdatedAt) && meta.BackendSyncVersion >= codexSessionSyncVersion) {
			continue
		}
		if err := s.syncCandidate(ctx, lease.RPC, meta); err != nil {
			syncErrors = append(syncErrors, &codexSessionSyncFailure{sessionID: meta.ID, err: err})
		}
	}
	result := errors.Join(syncErrors...)
	if result == nil {
		s.lastCheck = time.Now()
	}
	return result
}

func (s *codexSessionSyncer) syncCandidate(ctx context.Context, rpc codexapp.RPC, meta zotigosession.Metadata) error {
	unlockOperation := func() {}
	if s.sessionOps != nil {
		unlockOperation = s.sessionOps.lock(meta.ID)
	}
	defer unlockOperation()
	locker, ok := s.store.(interface {
		LockHistorySync(context.Context, string) (func(), error)
	})
	if !ok {
		return errors.New("session store does not support isolated history sync locking")
	}
	unlock, err := locker.LockHistorySync(ctx, meta.ID)
	if err != nil {
		return fmt.Errorf("lock session history sync: %w", err)
	}
	defer unlock()
	return s.syncSession(ctx, rpc, meta)
}

func (s *codexSessionSyncer) syncSession(ctx context.Context, rpc codexapp.RPC, meta zotigosession.Metadata) error {
	var history codexThreadSnapshot
	if err := rpc.Call(ctx, "thread/read", map[string]any{
		"threadId": meta.ConversationID, "includeTurns": false,
	}, &history); err != nil {
		return fmt.Errorf("read changed thread: %w", err)
	}
	if history.Thread.ID != meta.ConversationID {
		return fmt.Errorf("read changed thread: got thread %q, want %q", history.Thread.ID, meta.ConversationID)
	}
	if history.Thread.UpdatedAt <= 0 {
		return fmt.Errorf("read changed thread: missing updatedAt")
	}
	snapshotUpdatedAt := time.Unix(history.Thread.UpdatedAt, 0).UTC()
	turns, err := readCodexThreadHistory(ctx, rpc, meta.ConversationID)
	if err != nil {
		return err
	}
	if err := syncCompletedCodexTurns(ctx, s.items, meta.ID, turns); err != nil {
		return err
	}
	updater, ok := s.store.(backendActivityUpdater)
	if !ok {
		return fmt.Errorf("session store does not support backend activity updates")
	}
	if s.catalog != nil {
		if _, err := s.catalog.RecordSessionActivity(ctx, meta.ID, snapshotUpdatedAt); err != nil {
			return fmt.Errorf("record catalog activity: %w", err)
		}
	}
	if err := updater.UpdateBackendActivity(ctx, meta.ID, snapshotUpdatedAt, codexSessionSyncVersion); err != nil {
		return fmt.Errorf("persist backend activity: %w", err)
	}
	return nil
}
