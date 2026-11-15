package zotigod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/internal/codexapp"
	"github.com/jayyao97/zotigo/internal/testutil/sessionlogtest"

	"github.com/bytedance/sonic"
	"github.com/jayyao97/zotigo/core/agent"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
	"github.com/jayyao97/zotigo/internal/zotigod/codexworker"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
)

func TestCodexSessionSyncFallsBackWhenItemsListUnsupportedWhileRuntimeActive(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	meta := zotigosession.Metadata{ID: "session-active", Agent: "codex", ConversationID: "thread-active", CreatedAt: now, UpdatedAt: now}
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: meta}); err != nil {
		t.Fatal(err)
	}
	if err := store.Lock(context.Background(), meta.ID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Unlock(context.Background(), meta.ID) })
	completedAt := now.Add(time.Minute).Unix()
	startedAt := now.Unix()
	rpc := &codexSyncRPC{itemsUnsupported: true, thread: codexworker.Thread{
		ID: meta.ConversationID, UpdatedAt: completedAt,
		Turns: []codexworker.Turn{{
			ID: "turn-latest", Status: "completed", StartedAt: &startedAt, CompletedAt: &completedAt,
			Items: []codexworker.ThreadItem{{ID: "latest-message", Type: "agentMessage", Text: "latest history"}},
		}},
	}}
	registry := newSessionRegistry()
	registry.Add(Session{ID: meta.ID, Agent: "codex", State: SessionStateRunning, CreatedAt: now, UpdatedAt: now})
	syncer := newCodexSessionSyncer(&codexSyncHost{rpc: rpc}, store, sessionlog.StoredItemSource{Store: store}, nil, registry, newSessionOperationLocks())
	if err := syncer.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(rpc.calls) != "[list read turns items turns]" {
		t.Fatalf("RPC fallback calls = %v", rpc.calls)
	}
	items, _, err := store.ListDisplayItems(context.Background(), meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range items {
		found = found || item.ID == "latest-message"
	}
	if !found {
		t.Fatalf("latest fallback history missing: %#v", items)
	}
	stored, err := store.Get(context.Background(), meta.ID)
	if err != nil || stored.BackendUpdatedAt.Unix() != completedAt || stored.BackendSyncVersion != codexSessionSyncVersion {
		t.Fatalf("backend checkpoint = %#v, err=%v", stored, err)
	}
	active, _ := registry.Get(meta.ID)
	if active.State != SessionStateRunning {
		t.Fatalf("history sync changed runtime state: %#v", active)
	}
	before := len(items)
	syncer.lastCheck = time.Time{}
	if err := syncer.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	items, _, err = store.ListDisplayItems(context.Background(), meta.ID)
	if err != nil || len(items) != before {
		t.Fatalf("repeated fallback sync duplicated items: before=%d after=%d err=%v", before, len(items), err)
	}
}

func TestCodexSessionSyncReportsConcurrentHistoryCommitInsteadOfFalseSuccess(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	meta := zotigosession.Metadata{ID: "session-locked", Agent: "codex", ConversationID: "thread-locked", CreatedAt: now, UpdatedAt: now}
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: meta}); err != nil {
		t.Fatal(err)
	}
	unlock, err := store.LockHistorySync(context.Background(), meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
	rpc := &codexSyncRPC{thread: codexworker.Thread{ID: meta.ConversationID, UpdatedAt: now.Add(time.Minute).Unix()}}
	syncer := newCodexSessionSyncer(&codexSyncHost{rpc: rpc}, store, sessionlog.StoredItemSource{Store: store}, nil, newSessionRegistry(), newSessionOperationLocks())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err = syncer.Sync(ctx)
	diagnostics := codexSyncErrorDiagnostics(err)
	if len(diagnostics) != 1 || diagnostics[0].SessionID != meta.ID || diagnostics[0].Code != "codex_history_sync_failed" {
		t.Fatalf("locked sync diagnostics = %#v, err=%v", diagnostics, err)
	}
}

func TestCodexSessionSyncReadsHistoryOnlyWhenMetadataChanges(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	createdAt := time.Unix(1_700_000_000, 0).UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{
		Metadata: zotigosession.Metadata{
			ID: "session-1", WorkingDirectory: t.TempDir(), Agent: "codex", ConversationID: "thread-1",
			CreatedAt: createdAt, UpdatedAt: createdAt,
		},
		AgentSnapshot: agent.Snapshot{State: agent.StateIdle, CreatedAt: createdAt},
	}); err != nil {
		t.Fatal(err)
	}
	completedAt := createdAt.Add(time.Minute).Unix()
	startedAt := createdAt.Unix()
	output := "ok\n"
	rpc := &codexSyncRPC{thread: codexworker.Thread{
		ID:        "thread-1",
		UpdatedAt: completedAt,
		Turns: []codexworker.Turn{{
			ID: "turn-1", Status: "completed", StartedAt: &startedAt, CompletedAt: &completedAt,
			Items: []codexworker.ThreadItem{
				{ID: "user-1", Type: "userMessage", Content: []any{map[string]any{"type": "text", "text": "fix it"}}},
				{ID: "assistant-1", Type: "agentMessage", Text: "Done."},
				{ID: "command-1", Type: "commandExecution", Command: "go test ./...", CWD: "/tmp", Status: "completed", AggregatedOutput: &output},
			},
		}},
	}}
	syncer := &codexSessionSyncer{store: store, items: sessionlog.StoredItemSource{Store: store}}
	meta, err := store.Get(context.Background(), "session-1")
	if err != nil || meta == nil {
		t.Fatalf("load session: %#v %v", meta, err)
	}
	_, err = codexworker.ListThreadActivity(context.Background(), rpc, []zotigosession.Metadata{meta.Metadata})
	if err != nil {
		t.Fatal(err)
	}
	if err := syncer.syncSession(context.Background(), rpc, meta.Metadata); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(rpc.calls) != "[list read turns items]" {
		t.Fatalf("thread reads = %v", rpc.calls)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 6 {
		t.Fatalf("display items = %#v", items)
	}
	stored, err := store.Get(context.Background(), "session-1")
	if err != nil || stored == nil {
		t.Fatalf("load synced session: %#v %v", stored, err)
	}
	if got := stored.BackendUpdatedAt.Unix(); got != completedAt {
		t.Fatalf("backend updated at = %d", got)
	}
	if stored.BackendSyncVersion != codexSessionSyncVersion {
		t.Fatalf("backend sync version = %d", stored.BackendSyncVersion)
	}
	listed, err := store.List(context.Background(), zotigosession.ListFilter{})
	if err != nil || len(listed) != 1 || listed[0].BackendUpdatedAt.Unix() != completedAt || listed[0].BackendSyncVersion != codexSessionSyncVersion {
		t.Fatalf("indexed backend activity = %#v, err=%v", listed, err)
	}

	rpc.calls = nil
	activity, err := codexworker.ListThreadActivity(context.Background(), rpc, []zotigosession.Metadata{stored.Metadata})
	if err != nil {
		t.Fatal(err)
	}
	if activity[stored.ConversationID].After(stored.BackendUpdatedAt) {
		t.Fatal("unchanged thread appeared newer than its checkpoint")
	}
	if fmt.Sprint(rpc.calls) != "[list]" {
		t.Fatalf("unchanged thread reads = %v", rpc.calls)
	}
	again, _, err := store.ListDisplayItems(context.Background(), "session-1")
	if err != nil || len(again) != len(items) {
		t.Fatalf("idempotent items = %d, err=%v", len(again), err)
	}
}

func TestCodexSessionSyncOrchestratesAndThrottlesSuccessfulRefresh(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: "session-1", Agent: "codex", ConversationID: "thread-1", CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	rpc := &codexSyncRPC{thread: codexworker.Thread{ID: "thread-1", UpdatedAt: now.Add(time.Minute).Unix(), Turns: []codexworker.Turn{{ID: "turn-1", Status: "completed"}}}}
	host := &codexSyncHost{rpc: rpc}
	syncer := newCodexSessionSyncer(host, store, sessionlog.StoredItemSource{Store: store}, nil, newSessionRegistry(), newSessionOperationLocks())
	if err := syncer.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if host.acquires != 1 || fmt.Sprint(rpc.calls) != "[list read turns items]" {
		t.Fatalf("acquires = %d, calls = %v", host.acquires, rpc.calls)
	}
	stored, err := store.Get(context.Background(), "session-1")
	if err != nil || stored.BackendSyncVersion != codexSessionSyncVersion {
		t.Fatalf("stored checkpoint = %#v, err=%v", stored, err)
	}
	rpc.calls = nil
	if err := syncer.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if host.acquires != 1 || len(rpc.calls) != 0 {
		t.Fatalf("throttled acquires = %d, calls = %v", host.acquires, rpc.calls)
	}
}

func TestCodexSessionSyncProcessesOlderBackendActivityFirst(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	for _, meta := range []zotigosession.Metadata{
		{ID: "session-newer", Agent: "codex", ConversationID: "thread-newer", CreatedAt: now, UpdatedAt: now.Add(-time.Hour)},
		{ID: "session-older", Agent: "codex", ConversationID: "thread-older", CreatedAt: now, UpdatedAt: now},
	} {
		if err := store.Put(context.Background(), &zotigosession.Session{Metadata: meta}); err != nil {
			t.Fatal(err)
		}
	}
	updatedAt := map[string]int64{"thread-older": now.Add(time.Minute).Unix(), "thread-newer": now.Add(2 * time.Minute).Unix()}
	rpc := codexSyncFuncRPC{call: func(_ context.Context, method string, params any, result any) error {
		request := params.(map[string]any)
		var payload any
		switch method {
		case "thread/list":
			payload = codexworker.ThreadList{Data: []codexworker.Thread{{ID: "thread-newer", UpdatedAt: updatedAt["thread-newer"]}, {ID: "thread-older", UpdatedAt: updatedAt["thread-older"]}}}
		case "thread/read":
			threadID := request["threadId"].(string)
			payload = codexworker.ThreadSnapshot{Thread: codexworker.Thread{ID: threadID, UpdatedAt: updatedAt[threadID]}}
		case "thread/turns/list":
			threadID := request["threadId"].(string)
			payload = codexworker.TurnList{Data: []codexworker.Turn{{ID: "turn-" + threadID, Status: "completed"}}}
		case "thread/items/list":
			payload = codexworker.ThreadItemList{}
		default:
			return fmt.Errorf("unexpected method %s", method)
		}
		encoded, _ := sonic.Marshal(payload)
		return sonic.Unmarshal(encoded, result)
	}}
	var appendOrder []string
	items := &sessionlogtest.ItemSource{Items: make(map[string][]zotigosession.DisplayItem), AppendHook: func(sessionID string, item zotigosession.DisplayItem) {
		if item.Type == zotigosession.DisplayItemTurnStarted {
			appendOrder = append(appendOrder, sessionID)
		}
	}}
	syncer := newCodexSessionSyncer(&codexSyncHost{rpc: rpc}, store, items, nil, newSessionRegistry(), newSessionOperationLocks())
	if err := syncer.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(appendOrder) != "[session-older session-newer]" {
		t.Fatalf("append order = %v", appendOrder)
	}
}

func TestCodexSyncCandidateHoldsOperationAndHistorySyncLocks(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	meta := zotigosession.Metadata{ID: "session-1", Agent: "codex", ConversationID: "thread-1", CreatedAt: now, UpdatedAt: now}
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: meta}); err != nil {
		t.Fatal(err)
	}
	readStarted := make(chan struct{})
	releaseRead := make(chan struct{})
	rpc := codexSyncFuncRPC{call: func(ctx context.Context, method string, _ any, result any) error {
		switch method {
		case "thread/read":
			close(readStarted)
			select {
			case <-releaseRead:
			case <-ctx.Done():
				return ctx.Err()
			}
			payload, _ := sonic.Marshal(codexworker.ThreadSnapshot{Thread: codexworker.Thread{ID: "thread-1", UpdatedAt: now.Add(time.Second).Unix()}})
			return sonic.Unmarshal(payload, result)
		case "thread/turns/list":
			payload, _ := sonic.Marshal(codexworker.TurnList{})
			return sonic.Unmarshal(payload, result)
		case "thread/items/list":
			payload, _ := sonic.Marshal(codexworker.ThreadItemList{})
			return sonic.Unmarshal(payload, result)
		default:
			return fmt.Errorf("unexpected method %s", method)
		}
	}}
	operations := newSessionOperationLocks()
	syncer := &codexSessionSyncer{store: store, items: sessionlog.StoredItemSource{Store: store}, sessionOps: operations}
	done := make(chan error, 1)
	go func() { done <- syncer.syncCandidate(context.Background(), rpc, meta) }()
	<-readStarted
	locked, err := store.IsLocked(context.Background(), meta.ID)
	if err != nil || locked {
		t.Fatalf("history sync took runtime writer lock = %v, err=%v", locked, err)
	}
	lockCtx, cancelLock := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelLock()
	if unlock, err := store.LockHistorySync(lockCtx, meta.ID); err == nil {
		unlock()
		t.Fatal("competing history sync acquired lock")
	}
	operationAcquired := make(chan struct{})
	go func() {
		unlock := operations.lock(meta.ID)
		close(operationAcquired)
		unlock()
	}()
	select {
	case <-operationAcquired:
		t.Fatal("competing operation acquired lock during sync")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseRead)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-operationAcquired:
	case <-time.After(time.Second):
		t.Fatal("competing operation did not acquire released lock")
	}
}

func TestCodexSyncDoesNotAdvanceCheckpointWhenDisplayMergeFails(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	meta := zotigosession.Metadata{ID: "session-1", Agent: "codex", ConversationID: "thread-1", CreatedAt: now, UpdatedAt: now}
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: meta}); err != nil {
		t.Fatal(err)
	}
	rpc := &codexSyncRPC{thread: codexworker.Thread{ID: "thread-1", UpdatedAt: now.Add(time.Minute).Unix()}}
	syncer := &codexSessionSyncer{store: store, items: sessionlog.FailingItemSource{Err: errors.New("display unavailable")}}
	if err := syncer.syncSession(context.Background(), rpc, meta); err == nil {
		t.Fatal("expected display merge failure")
	}
	stored, err := store.Get(context.Background(), meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.BackendUpdatedAt.IsZero() || stored.BackendSyncVersion != 0 {
		t.Fatalf("checkpoint advanced after failure: %+v", stored.Metadata)
	}
}

func TestCodexSyncDoesNotAdvanceCheckpointWhenCatalogActivityFails(t *testing.T) {
	root := t.TempDir()
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	catalog, err := zotigoworkspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = catalog.Close() })
	now := time.Now().UTC()
	meta := zotigosession.Metadata{ID: "session-1", Agent: "codex", ConversationID: "thread-1", CreatedAt: now, UpdatedAt: now}
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: meta}); err != nil {
		t.Fatal(err)
	}
	rpc := &codexSyncRPC{thread: codexworker.Thread{ID: "thread-1", UpdatedAt: now.Add(time.Minute).Unix()}}
	syncer := &codexSessionSyncer{store: store, items: sessionlog.StoredItemSource{Store: store}, catalog: catalog}
	if err := syncer.syncSession(context.Background(), rpc, meta); !errors.Is(err, zotigoworkspace.ErrNotFound) {
		t.Fatalf("catalog error = %v, want not found", err)
	}
	stored, err := store.Get(context.Background(), meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.BackendUpdatedAt.IsZero() || stored.BackendSyncVersion != 0 {
		t.Fatalf("checkpoint advanced after catalog failure: %+v", stored.Metadata)
	}
}

func TestCodexSyncDiagnosticsIdentifyUnsupportedHistoryAPIBySession(t *testing.T) {
	diagnostics := codexSyncErrorDiagnostics(errors.Join(
		&codexSessionSyncFailure{sessionID: "session-unsupported", err: fmt.Errorf("read history: %w", codexworker.ErrHistoryAPIUnsupported)},
		&codexSessionSyncFailure{sessionID: "session-failed", err: errors.New("connection closed")},
	))
	if len(diagnostics) != 2 {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
	if diagnostics[0].SessionID != "session-unsupported" || diagnostics[0].Code != "codex_history_api_unsupported" {
		t.Fatalf("unsupported diagnostic = %#v", diagnostics[0])
	}
	if diagnostics[1].SessionID != "session-failed" || diagnostics[1].Code != "codex_history_sync_failed" {
		t.Fatalf("failed diagnostic = %#v", diagnostics[1])
	}
}

func TestCodexSessionSyncContinuesPastUnavailableHistoricalImage(t *testing.T) {
	root := t.TempDir()
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	meta := zotigosession.Metadata{
		ID: "session-missing-image", Agent: "codex", ConversationID: "thread-missing-image",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: meta}); err != nil {
		t.Fatal(err)
	}
	missingPath := filepath.Join(root, "already-deleted.png")
	completedAt := now.Add(time.Minute).Unix()
	startedAt := now.Unix()
	rpc := &codexSyncRPC{thread: codexworker.Thread{
		ID: meta.ConversationID, UpdatedAt: completedAt,
		Turns: []codexworker.Turn{{
			ID: "turn-1", Status: "completed", StartedAt: &startedAt, CompletedAt: &completedAt,
			Items: []codexworker.ThreadItem{
				{ID: "missing-image", Type: "imageGeneration", Status: "completed", SavedPath: &missingPath},
				{ID: "no-source-image", Type: "imageGeneration", Status: "completed"},
				{ID: "bad-tool-image", Type: "dynamicToolCall", Tool: "image_tool", Status: "completed", ContentItems: []any{
					map[string]any{"type": "inputImage", "imageUrl": "data:image/png;base64,not-valid!"},
				}},
				{ID: "later-message", Type: "agentMessage", Text: "sync still progresses"},
			},
		}},
	}}
	syncer := &codexSessionSyncer{store: store, items: sessionlog.StoredItemSource{Store: store}}
	if err := syncer.syncSession(context.Background(), rpc, meta); err != nil {
		t.Fatal(err)
	}
	items, _, err := store.ListDisplayItems(context.Background(), meta.ID)
	if err != nil {
		t.Fatal(err)
	}
	unavailable := 0
	foundLater := false
	foundToolPlaceholder := false
	for _, item := range items {
		if (item.ID == "missing-image" || item.ID == "no-source-image") && item.Type == zotigosession.DisplayItemError {
			unavailable++
		}
		foundLater = foundLater || item.ID == "later-message"
		if item.ID == "codex-tool-result-bad-tool-image" && len(item.Content) == 1 && item.Content[0].ToolResult != nil {
			content := item.Content[0].ToolResult.Content
			foundToolPlaceholder = len(content) == 1 && content[0].Text == "[image content is unavailable]"
		}
	}
	if unavailable != 2 || !foundToolPlaceholder || !foundLater {
		t.Fatalf("sync items after unavailable image: %#v", items)
	}
	stored, err := store.Get(context.Background(), meta.ID)
	if err != nil || stored.BackendSyncVersion != codexSessionSyncVersion || stored.BackendUpdatedAt.Unix() != completedAt {
		t.Fatalf("sync checkpoint = %#v, err=%v", stored, err)
	}
}

func TestCodexSyncFailureDoesNotStartThrottleWindow(t *testing.T) {
	syncer := &codexSessionSyncer{store: unavailableSessionStore{err: errors.New("index unavailable")}}
	if err := syncer.Sync(context.Background()); err == nil {
		t.Fatal("expected sync failure")
	}
	if !syncer.lastCheck.IsZero() {
		t.Fatalf("failed sync advanced throttle checkpoint to %s", syncer.lastCheck)
	}
}

func TestBackendActivityCheckpointIsMonotonic(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{ID: "session-1", CreatedAt: now, UpdatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	newer := now.Add(2 * time.Minute)
	if err := store.UpdateBackendActivity(context.Background(), "session-1", newer, 1); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateBackendActivity(context.Background(), "session-1", now.Add(time.Minute), 2); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Get(context.Background(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if !stored.BackendUpdatedAt.Equal(newer) || stored.BackendSyncVersion != 2 {
		t.Fatalf("checkpoint = %s/v%d, want %s/v2", stored.BackendUpdatedAt, stored.BackendSyncVersion, newer)
	}
}

func TestCatalogSessionListReportsCodexSyncDiagnostic(t *testing.T) {
	root := t.TempDir()
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: "session-1", Agent: "codex", ConversationID: "thread-1", CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	catalog, err := zotigoworkspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = catalog.Close() })
	privatePath := "/Users/private/.codex/internal.sock"
	handler := newHandler(newSessionRegistry(), sessionlog.StoredItemSource{Store: store}, handlerOptions{
		store: store, catalog: catalog, codexHost: &codexSyncHost{err: errors.New(privatePath)},
	})
	for _, path := range []string{"/sessions?sync_codex=true", "/catalog/sessions?sync_codex=true"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status = %d, body=%s", path, recorder.Code, recorder.Body.String())
		}
		var envelope struct {
			Diagnostics []sessionSyncDiagnostic `json:"diagnostics"`
		}
		if err := decodeAPIData(t, recorder.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if len(envelope.Diagnostics) != 1 || envelope.Diagnostics[0].Code != "codex_history_sync_failed" {
			t.Fatalf("%s diagnostics = %#v, body=%s", path, envelope.Diagnostics, recorder.Body.String())
		}
		if strings.Contains(recorder.Body.String(), privatePath) {
			t.Fatalf("%s leaked private path: %s", path, recorder.Body.String())
		}
	}
}

func TestCodexSessionSyncWithTypedNilHostReturnsError(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{ID: "session-missing-runtime", Agent: "codex", ConversationID: "thread-missing", CreatedAt: now, UpdatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	var host *codexapp.Host
	syncer := newCodexSessionSyncer(host, store, sessionlog.StoredItemSource{Store: store}, nil, newSessionRegistry(), newSessionOperationLocks())
	if err := syncer.Sync(context.Background()); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("sync error = %v", err)
	}
}

type codexSyncRPC struct {
	thread           codexworker.Thread
	calls            []string
	itemsUnsupported bool
}

func (r *codexSyncRPC) Call(_ context.Context, method string, params any, result any) error {
	var payload any
	switch method {
	case "thread/list":
		r.calls = append(r.calls, "list")
		thread := r.thread
		thread.Turns = nil
		payload = codexworker.ThreadList{Data: []codexworker.Thread{thread}}
	case "thread/read":
		request, ok := params.(map[string]any)
		if !ok || request["includeTurns"] != false {
			return fmt.Errorf("unexpected params %#v", params)
		}
		r.calls = append(r.calls, "read")
		thread := r.thread
		thread.Turns = nil
		payload = codexworker.ThreadSnapshot{Thread: thread}
	case "thread/turns/list":
		r.calls = append(r.calls, "turns")
		turns := append([]codexworker.Turn(nil), r.thread.Turns...)
		request := params.(map[string]any)
		if request["itemsView"] != "full" {
			for index := range turns {
				turns[index].Items = nil
			}
		}
		payload = codexworker.TurnList{Data: turns}
	case "thread/items/list":
		r.calls = append(r.calls, "items")
		if r.itemsUnsupported {
			return &codexapp.RPCError{Code: -32601, Message: "thread/items/list is not supported yet"}
		}
		entries := make([]codexworker.ThreadItemEntry, 0)
		for _, turn := range r.thread.Turns {
			for _, item := range turn.Items {
				entries = append(entries, codexworker.ThreadItemEntry{TurnID: turn.ID, Item: item})
			}
		}
		payload = codexworker.ThreadItemList{Data: entries}
	default:
		return fmt.Errorf("unexpected method %s", method)
	}
	encoded, err := sonic.Marshal(payload)
	if err != nil {
		return err
	}
	return sonic.Unmarshal(encoded, result)
}

func (*codexSyncRPC) Notify(string, any) error { return nil }
