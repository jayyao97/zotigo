package zotigod

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/internal/testutil/sessionlogtest"

	"github.com/bytedance/sonic"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/codexapp"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
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

func TestSyncCompletedCodexTurnsDoesNotExposePartialTurnOnAppendFailure(t *testing.T) {
	source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}
	appendCount := 0
	source.AppendErr = func(string, zotigosession.DisplayItem) error {
		appendCount++
		if appendCount == 2 {
			return errors.New("injected batch failure")
		}
		return nil
	}
	completedAt := time.Now().Unix()
	startedAt := completedAt - 1
	err := syncCompletedCodexTurns(context.Background(), source, "session-atomic", []codexTurn{{
		ID: "turn-atomic", Status: "completed", StartedAt: &startedAt, CompletedAt: &completedAt,
		Items: []codexThreadItem{{ID: "message-atomic", Type: "agentMessage", Text: "must stay hidden"}},
	}})
	if err == nil || !strings.Contains(err.Error(), "injected batch failure") {
		t.Fatalf("sync error = %v", err)
	}
	items, _, loadErr := source.LoadItems(context.Background(), "session-atomic")
	if loadErr != nil || len(items) != 0 {
		t.Fatalf("partial turn became visible: %#v, err=%v", items, loadErr)
	}
}

func TestSyncCompletedCodexTurnsRejectsConcurrentLiveAppend(t *testing.T) {
	source := &sessionlogtest.ItemSource{Items: map[string][]zotigosession.DisplayItem{}}
	injected := false
	source.AppendErr = func(sessionID string, _ zotigosession.DisplayItem) error {
		if injected {
			return nil
		}
		injected = true
		source.Mu.Lock()
		source.Items[sessionID] = append(source.Items[sessionID], zotigosession.DisplayItem{
			ID: "live-item", Sequence: 1, Type: zotigosession.DisplayItemAssistantMessage,
		})
		source.Mu.Unlock()
		return nil
	}
	completedAt := time.Now().Unix()
	err := syncCompletedCodexTurns(context.Background(), source, "session-race", []codexTurn{{
		ID: "turn-race", Status: "completed", CompletedAt: &completedAt,
		Items: []codexThreadItem{{ID: "synced-item", Type: "agentMessage", Text: "history"}},
	}})
	if !errors.Is(err, zotigosession.ErrDisplayLogChanged) {
		t.Fatalf("concurrent append error = %v", err)
	}
	items, _, loadErr := source.LoadItems(context.Background(), "session-race")
	if loadErr != nil || len(items) != 1 || items[0].ID != "live-item" {
		t.Fatalf("sync interleaved with live append: %#v, err=%v", items, loadErr)
	}
}

func TestSyncCompletedCodexTurnsIsIdempotent(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
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
	source := sessionlog.StoredItemSource{Store: store}
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
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{ID: "session-1", CreatedAt: now, UpdatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	turns := []codexTurn{{ID: "turn-1", Status: "interrupted"}}
	source := sessionlog.StoredItemSource{Store: store}
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
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
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
	source := sessionlog.StoredItemSource{Store: store}
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
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
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
	source := sessionlog.StoredItemSource{Store: store}
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
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	if err := store.Put(context.Background(), &zotigosession.Session{Metadata: zotigosession.Metadata{ID: "session-1", CreatedAt: now, UpdatedAt: now}}); err != nil {
		t.Fatal(err)
	}
	source := sessionlog.StoredItemSource{Store: store}
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

func TestSyncCompletedCodexTurnsDoesNotDuplicateLocalUserPrompt(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
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
	source := sessionlog.StoredItemSource{Store: store}
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
	source := sessionlog.StoredItemSource{Store: store}
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
