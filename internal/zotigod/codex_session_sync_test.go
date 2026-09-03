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

	"github.com/bytedance/sonic"
	"github.com/jayyao97/zotigo/core/agent"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
	"github.com/jayyao97/zotigo/internal/codexapp"
)

type codexSyncFuncRPC struct {
	call func(context.Context, string, any, any) error
}

func (r codexSyncFuncRPC) Call(ctx context.Context, method string, params any, result any) error {
	return r.call(ctx, method, params, result)
}

func (codexSyncFuncRPC) Notify(string, any) error { return nil }

type codexSyncHost struct {
	rpc      codexapp.RPC
	acquires int
	err      error
}

func (h *codexSyncHost) Acquire(context.Context) (*codexapp.Lease, error) {
	h.acquires++
	if h.err != nil {
		return nil, h.err
	}
	return &codexapp.Lease{RPC: h.rpc}, nil
}

type codexSyncRPC struct {
	thread codexThread
	calls  []string
}

func (r *codexSyncRPC) Call(_ context.Context, method string, params any, result any) error {
	var payload any
	switch method {
	case "thread/list":
		r.calls = append(r.calls, "list")
		thread := r.thread
		thread.Turns = nil
		payload = codexThreadList{Data: []codexThread{thread}}
	case "thread/read":
		request, ok := params.(map[string]any)
		if !ok || request["includeTurns"] != false {
			return fmt.Errorf("unexpected params %#v", params)
		}
		r.calls = append(r.calls, "read")
		thread := r.thread
		thread.Turns = nil
		payload = codexThreadSnapshot{Thread: thread}
	case "thread/turns/list":
		r.calls = append(r.calls, "turns")
		turns := append([]codexTurn(nil), r.thread.Turns...)
		for index := range turns {
			turns[index].Items = nil
		}
		payload = codexTurnList{Data: turns}
	case "thread/items/list":
		r.calls = append(r.calls, "items")
		entries := make([]codexThreadItemEntry, 0)
		for _, turn := range r.thread.Turns {
			for _, item := range turn.Items {
				entries = append(entries, codexThreadItemEntry{TurnID: turn.ID, Item: item})
			}
		}
		payload = codexThreadItemList{Data: entries}
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

func TestCodexSessionSyncReadsHistoryOnlyWhenMetadataChanges(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
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
	rpc := &codexSyncRPC{thread: codexThread{
		ID:        "thread-1",
		UpdatedAt: completedAt,
		Turns: []codexTurn{{
			ID: "turn-1", Status: "completed", StartedAt: &startedAt, CompletedAt: &completedAt,
			Items: []codexThreadItem{
				{ID: "user-1", Type: "userMessage", Content: []any{map[string]any{"type": "text", "text": "fix it"}}},
				{ID: "assistant-1", Type: "agentMessage", Text: "Done."},
				{ID: "command-1", Type: "commandExecution", Command: "go test ./...", CWD: "/tmp", Status: "completed", AggregatedOutput: &output},
			},
		}},
	}}
	syncer := &codexSessionSyncer{store: store, items: storedDisplayItemSource{store: store}}
	meta, err := store.Get(context.Background(), "session-1")
	if err != nil || meta == nil {
		t.Fatalf("load session: %#v %v", meta, err)
	}
	_, err = listCodexThreadActivity(context.Background(), rpc, []zotigosession.Metadata{meta.Metadata})
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
	activity, err := listCodexThreadActivity(context.Background(), rpc, []zotigosession.Metadata{stored.Metadata})
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
	store, err := zotigosession.NewFileStore(t.TempDir())
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
	rpc := &codexSyncRPC{thread: codexThread{ID: "thread-1", UpdatedAt: now.Add(time.Minute).Unix(), Turns: []codexTurn{{ID: "turn-1", Status: "completed"}}}}
	host := &codexSyncHost{rpc: rpc}
	syncer := newCodexSessionSyncer(host, store, storedDisplayItemSource{store: store}, nil, newSessionRegistry(), newSessionOperationLocks())
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
	store, err := zotigosession.NewFileStore(t.TempDir())
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
			payload = codexThreadList{Data: []codexThread{{ID: "thread-newer", UpdatedAt: updatedAt["thread-newer"]}, {ID: "thread-older", UpdatedAt: updatedAt["thread-older"]}}}
		case "thread/read":
			threadID := request["threadId"].(string)
			payload = codexThreadSnapshot{Thread: codexThread{ID: threadID, UpdatedAt: updatedAt[threadID]}}
		case "thread/turns/list":
			threadID := request["threadId"].(string)
			payload = codexTurnList{Data: []codexTurn{{ID: "turn-" + threadID, Status: "completed"}}}
		case "thread/items/list":
			payload = codexThreadItemList{}
		default:
			return fmt.Errorf("unexpected method %s", method)
		}
		encoded, _ := sonic.Marshal(payload)
		return sonic.Unmarshal(encoded, result)
	}}
	var appendOrder []string
	items := &fakeDisplayItemSource{items: make(map[string][]zotigosession.DisplayItem), appendHook: func(sessionID string, item zotigosession.DisplayItem) {
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

func TestSyncCompletedCodexTurnsIsIdempotent(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: "session-1", CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	startedAt := now.Unix()
	completedAt := now.Add(time.Second).Unix()
	turns := []codexTurn{{
		ID: "turn-1", Status: "completed", StartedAt: &startedAt, CompletedAt: &completedAt,
		Items: []codexThreadItem{{ID: "assistant-1", Type: "agentMessage", Text: "Done."}},
	}}
	source := storedDisplayItemSource{store: store}
	if err := syncCompletedCodexTurns(context.Background(), source, "session-1", turns); err != nil {
		t.Fatal(err)
	}
	if err := syncCompletedCodexTurns(context.Background(), source, "session-1", turns); err != nil {
		t.Fatal(err)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-1")
	if err != nil || len(items) != 3 {
		t.Fatalf("display items = %#v, err=%v", items, err)
	}
}

func TestSyncCompletedCodexTurnsIsIdempotentWithoutCompletedAt(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{ID: "session-1", CreatedAt: now, UpdatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	turns := []codexTurn{{ID: "turn-1", Status: "interrupted"}}
	source := storedDisplayItemSource{store: store}
	if err := syncCompletedCodexTurns(context.Background(), source, "session-1", turns); err != nil {
		t.Fatal(err)
	}
	if err := syncCompletedCodexTurns(context.Background(), source, "session-1", turns); err != nil {
		t.Fatal(err)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-1")
	if err != nil || len(items) != 2 {
		t.Fatalf("display items = %#v, err=%v", items, err)
	}
}

func TestSyncCompletedCodexTurnsBackfillsItemsAfterInterruptedTerminal(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: "session-1", CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	source := storedDisplayItemSource{store: store}
	for _, item := range []zotigosession.DisplayItem{
		{ID: "codex-turn-started-turn-1", Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
		{ID: "user-1", Type: zotigosession.DisplayItemUserMessage, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
		{ID: "codex-turn-finished-turn-1", Type: zotigosession.DisplayItemTurnInterrupted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
	} {
		if _, err := source.AppendItem(context.Background(), "session-1", item); err != nil {
			t.Fatal(err)
		}
	}
	startedAt := now.Unix()
	completedAt := now.Add(time.Second).Unix()
	turns := []codexTurn{{
		ID: "turn-1", Status: "completed", StartedAt: &startedAt, CompletedAt: &completedAt,
		Items: []codexThreadItem{
			{ID: "user-1", Type: "userMessage", Content: []any{map[string]any{"type": "text", "text": "question"}}},
			{ID: "assistant-1", Type: "agentMessage", Text: "Late answer."},
		},
	}}
	if err := syncCompletedCodexTurns(context.Background(), source, "session-1", turns); err != nil {
		t.Fatal(err)
	}
	if err := syncCompletedCodexTurns(context.Background(), source, "session-1", turns); err != nil {
		t.Fatal(err)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-1")
	if err != nil || len(items) != 5 {
		t.Fatalf("display items = %#v, err=%v", items, err)
	}
	if items[3].ID != "assistant-1" || items[3].Turn == nil || items[3].Turn.ID != "turn-1" {
		t.Fatalf("late assistant item = %#v", items[3])
	}
	if items[4].Type != zotigosession.DisplayItemTurnCompleted || items[4].Turn == nil || items[4].Turn.Status != "completed" {
		t.Fatalf("authoritative terminal item = %#v", items[4])
	}
}

func TestSyncCompletedCodexTurnsUsesClientIDForLocalSteering(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: "session-1", CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	source := storedDisplayItemSource{store: store}
	for _, item := range []zotigosession.DisplayItem{
		{ID: "turn-started", Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
		{ID: "steering-command", Type: zotigosession.DisplayItemSteeringMessage, Content: []zotigosession.DisplayContentPart{{Type: "text", Text: "more"}}, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
	} {
		if _, err := source.AppendItem(context.Background(), "session-1", item); err != nil {
			t.Fatal(err)
		}
	}
	clientID := "steering-command"
	completedAt := now.Add(time.Second).Unix()
	turns := []codexTurn{{
		ID: "turn-1", Status: "completed", CompletedAt: &completedAt,
		Items: []codexThreadItem{{ID: "remote-steering", ClientID: &clientID, Type: "userMessage", Content: []any{map[string]any{"type": "text", "text": "more"}}}},
	}}
	if err := syncCompletedCodexTurns(context.Background(), source, "session-1", turns); err != nil {
		t.Fatal(err)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || items[2].Type != zotigosession.DisplayItemTurnCompleted {
		t.Fatalf("display items = %#v", items)
	}
}

func TestSyncCompletedCodexTurnsMatchesPendingLocalUserWithoutClientID(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{ID: "session-1", CreatedAt: now, UpdatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	source := storedDisplayItemSource{store: store}
	if _, err := source.AppendItem(context.Background(), "session-1", zotigosession.DisplayItem{
		ID: "local-user", Type: zotigosession.DisplayItemUserMessage,
		Content: []zotigosession.DisplayContentPart{{Type: "text", Text: "question"}},
	}); err != nil {
		t.Fatal(err)
	}
	completedAt := now.Add(time.Second).Unix()
	turns := []codexTurn{{
		ID: "turn-1", Status: "completed", CompletedAt: &completedAt,
		Items: []codexThreadItem{{ID: "remote-user", Type: "userMessage", Content: []any{map[string]any{"type": "text", "text": "question"}}}},
	}}
	if err := syncCompletedCodexTurns(context.Background(), source, "session-1", turns); err != nil {
		t.Fatal(err)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || items[0].ID != "local-user" || items[1].Type != zotigosession.DisplayItemTurnStarted || items[2].Type != zotigosession.DisplayItemTurnCompleted {
		t.Fatalf("display items = %#v", items)
	}
}

func TestReadCodexThreadHistoryPaginatesTurnsAndItems(t *testing.T) {
	turnCursor := "more-turns"
	itemCursor := "more-items"
	rpc := codexSyncFuncRPC{call: func(_ context.Context, method string, params any, result any) error {
		request := params.(map[string]any)
		var payload any
		switch method {
		case "thread/turns/list":
			if request["itemsView"] != "notLoaded" || request["sortDirection"] != "asc" {
				return fmt.Errorf("unexpected turn params %#v", request)
			}
			if request["cursor"] == nil {
				payload = codexTurnList{Data: []codexTurn{{ID: "turn-1", Status: "completed"}}, NextCursor: &turnCursor}
			} else {
				payload = codexTurnList{Data: []codexTurn{{ID: "turn-2", Status: "completed"}}}
			}
		case "thread/items/list":
			if request["sortDirection"] != "asc" {
				return fmt.Errorf("unexpected item params %#v", request)
			}
			if request["cursor"] == nil {
				payload = codexThreadItemList{Data: []codexThreadItemEntry{{TurnID: "turn-1", Item: codexThreadItem{ID: "item-1"}}}, NextCursor: &itemCursor}
			} else {
				payload = codexThreadItemList{Data: []codexThreadItemEntry{{TurnID: "turn-2", Item: codexThreadItem{ID: "item-2"}}}}
			}
		default:
			return fmt.Errorf("unexpected method %s", method)
		}
		encoded, err := sonic.Marshal(payload)
		if err != nil {
			return err
		}
		return sonic.Unmarshal(encoded, result)
	}}
	turns, err := readCodexThreadHistory(context.Background(), rpc, "thread-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 || len(turns[0].Items) != 1 || turns[0].Items[0].ID != "item-1" || len(turns[1].Items) != 1 || turns[1].Items[0].ID != "item-2" {
		t.Fatalf("turn history = %#v", turns)
	}
}

func TestListCodexThreadActivityIncludesArchivedWithoutCWDFilter(t *testing.T) {
	archivedUpdatedAt := time.Now().UTC().Unix()
	var archivedRequests []bool
	rpc := codexSyncFuncRPC{call: func(_ context.Context, method string, params any, result any) error {
		if method != "thread/list" {
			return fmt.Errorf("unexpected method %s", method)
		}
		request := params.(map[string]any)
		if _, exists := request["cwd"]; exists {
			return fmt.Errorf("unexpected cwd filter %#v", request)
		}
		archived := request["archived"].(bool)
		archivedRequests = append(archivedRequests, archived)
		payload := codexThreadList{}
		if archived {
			payload.Data = []codexThread{{ID: "thread-1", UpdatedAt: archivedUpdatedAt}}
		}
		encoded, _ := sonic.Marshal(payload)
		return sonic.Unmarshal(encoded, result)
	}}
	activity, err := listCodexThreadActivity(context.Background(), rpc, []zotigosession.Metadata{{ConversationID: "thread-1", WorkingDirectory: "/moved"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := activity["thread-1"].Unix(); got != archivedUpdatedAt || fmt.Sprint(archivedRequests) != "[false true]" {
		t.Fatalf("activity = %#v, archived requests = %v", activity, archivedRequests)
	}
}

func TestCodexSyncCandidateHoldsOperationAndSessionLocks(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
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
			payload, _ := sonic.Marshal(codexThreadSnapshot{Thread: codexThread{ID: "thread-1", UpdatedAt: now.Add(time.Second).Unix()}})
			return sonic.Unmarshal(payload, result)
		case "thread/turns/list":
			payload, _ := sonic.Marshal(codexTurnList{})
			return sonic.Unmarshal(payload, result)
		case "thread/items/list":
			payload, _ := sonic.Marshal(codexThreadItemList{})
			return sonic.Unmarshal(payload, result)
		default:
			return fmt.Errorf("unexpected method %s", method)
		}
	}}
	operations := newSessionOperationLocks()
	syncer := &codexSessionSyncer{store: store, items: storedDisplayItemSource{store: store}, sessionOps: operations}
	done := make(chan error, 1)
	go func() { done <- syncer.syncCandidate(context.Background(), rpc, meta) }()
	<-readStarted
	locked, err := store.IsLocked(context.Background(), meta.ID)
	if err != nil || !locked {
		t.Fatalf("session lock = %v, err=%v", locked, err)
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

func TestCodexSyncFailureDoesNotStartThrottleWindow(t *testing.T) {
	syncer := &codexSessionSyncer{store: unavailableSessionStore{err: errors.New("index unavailable")}}
	if err := syncer.Sync(context.Background()); err == nil {
		t.Fatal("expected sync failure")
	}
	if !syncer.lastCheck.IsZero() {
		t.Fatalf("failed sync advanced throttle checkpoint to %s", syncer.lastCheck)
	}
}

func TestCodexSyncDoesNotAdvanceCheckpointWhenDisplayMergeFails(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	meta := zotigosession.Metadata{ID: "session-1", Agent: "codex", ConversationID: "thread-1", CreatedAt: now, UpdatedAt: now}
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: meta}); err != nil {
		t.Fatal(err)
	}
	rpc := &codexSyncRPC{thread: codexThread{ID: "thread-1", UpdatedAt: now.Add(time.Minute).Unix()}}
	syncer := &codexSessionSyncer{store: store, items: failingDisplayItemSource{err: errors.New("display unavailable")}}
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
	rpc := &codexSyncRPC{thread: codexThread{ID: "thread-1", UpdatedAt: now.Add(time.Minute).Unix()}}
	syncer := &codexSessionSyncer{store: store, items: storedDisplayItemSource{store: store}, catalog: catalog}
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

func TestBackendActivityCheckpointIsMonotonic(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
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
	handler := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{
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
		if len(envelope.Diagnostics) != 1 || envelope.Diagnostics[0].Code != "codex_sync_failed" {
			t.Fatalf("%s diagnostics = %#v, body=%s", path, envelope.Diagnostics, recorder.Body.String())
		}
		if strings.Contains(recorder.Body.String(), privatePath) {
			t.Fatalf("%s leaked private path: %s", path, recorder.Body.String())
		}
	}
}

func TestSyncCompletedCodexTurnsDoesNotDuplicateLocalUserPrompt(t *testing.T) {
	store, err := zotigosession.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{
		ID: "session-1", CreatedAt: now, UpdatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}
	source := storedDisplayItemSource{store: store}
	for _, item := range []zotigosession.DisplayItem{
		{ID: "local-user", Type: zotigosession.DisplayItemUserMessage, Role: "user", Content: []zotigosession.DisplayContentPart{{Type: "text", Text: "question"}}},
		{ID: "turn-started", Type: zotigosession.DisplayItemTurnStarted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
		{ID: "turn-finished", Type: zotigosession.DisplayItemTurnCompleted, Turn: &zotigosession.DisplayTurn{ID: "turn-1"}},
	} {
		if _, err := source.AppendItem(context.Background(), "session-1", item); err != nil {
			t.Fatal(err)
		}
	}
	startedAt := now.Unix()
	completedAt := now.Add(time.Second).Unix()
	turns := []codexTurn{{
		ID: "turn-1", Status: "completed", StartedAt: &startedAt, CompletedAt: &completedAt,
		Items: []codexThreadItem{
			{ID: "codex-user", Type: "userMessage", Content: []any{map[string]any{"type": "text", "text": "question"}}},
			{ID: "assistant-1", Type: "agentMessage", Text: "answer"},
		},
	}}
	if err := syncCompletedCodexTurns(context.Background(), source, "session-1", turns); err != nil {
		t.Fatal(err)
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 5 || items[3].ID != "assistant-1" || items[4].Type != zotigosession.DisplayItemTurnCompleted {
		t.Fatalf("display items = %#v", items)
	}
}

func TestSyncCompletedCodexTurnsPersistsGeneratedImage(t *testing.T) {
	root := t.TempDir()
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{ID: "session-image-sync", CreatedAt: now, UpdatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	startedAt := now.Unix()
	completedAt := now.Add(time.Second).Unix()
	source := storedDisplayItemSource{store: store}
	turns := []codexTurn{{
		ID: "turn-image", Status: "completed", StartedAt: &startedAt, CompletedAt: &completedAt,
		Items: []codexThreadItem{{ID: "image-sync-1", Type: "imageGeneration", Status: "completed", Result: tinyPNGBase64()}},
	}}
	for range 2 {
		if err := syncCompletedCodexTurns(context.Background(), source, "session-image-sync", turns); err != nil {
			t.Fatal(err)
		}
	}
	items, _, err := store.ListDisplayItems(context.Background(), "session-image-sync")
	if err != nil {
		t.Fatal(err)
	}
	imageItems := 0
	for _, item := range items {
		if item.ID != "image-sync-1" {
			continue
		}
		imageItems++
		if len(item.Content) != 1 || item.Content[0].Image == nil || item.Content[0].Image.URL == "" || item.Content[0].Image.MediaType != "image/png" {
			t.Fatalf("synced image item = %#v", item)
		}
	}
	if imageItems != 1 {
		t.Fatalf("generated image sync was not idempotent: %d items", imageItems)
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
	rpc := &codexSyncRPC{thread: codexThread{
		ID: meta.ConversationID, UpdatedAt: completedAt,
		Turns: []codexTurn{{
			ID: "turn-1", Status: "completed", StartedAt: &startedAt, CompletedAt: &completedAt,
			Items: []codexThreadItem{
				{ID: "missing-image", Type: "imageGeneration", Status: "completed", SavedPath: &missingPath},
				{ID: "no-source-image", Type: "imageGeneration", Status: "completed"},
				{ID: "bad-tool-image", Type: "dynamicToolCall", Tool: "image_tool", Status: "completed", ContentItems: []any{
					map[string]any{"type": "inputImage", "imageUrl": "data:image/png;base64,not-valid!"},
				}},
				{ID: "later-message", Type: "agentMessage", Text: "sync still progresses"},
			},
		}},
	}}
	syncer := &codexSessionSyncer{store: store, items: storedDisplayItemSource{store: store}}
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
