package session

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/protocol"
)

func TestManagerAppendDisplayItemAssignsMonotonicSequence(t *testing.T) {
	manager := newDisplayLogTestManager(t)
	sess, err := manager.CreateNew(t.TempDir())
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	first, err := manager.AppendDisplayItem(sess.ID, DisplayItem{Type: DisplayItemUserMessage})
	if err != nil {
		t.Fatalf("append first item: %v", err)
	}
	second, err := manager.AppendDisplayItem(sess.ID, DisplayItem{Type: DisplayItemAssistantMessage})
	if err != nil {
		t.Fatalf("append second item: %v", err)
	}

	if first.Sequence != 1 || second.Sequence != 2 {
		t.Fatalf("expected sequences 1,2 got %d,%d", first.Sequence, second.Sequence)
	}
	if first.ID != "item_"+sess.ID+"_1" || second.ID != "item_"+sess.ID+"_2" {
		t.Fatalf("unexpected ids: %q %q", first.ID, second.ID)
	}
	if first.CreatedAt.IsZero() || second.CreatedAt.IsZero() {
		t.Fatal("expected created_at")
	}
}

func TestManagerAppendDisplayItemTruncatesPartialTail(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	manager := NewManagerWithStore(store)
	sess, err := manager.CreateNew(t.TempDir())
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := manager.AppendDisplayItem(sess.ID, DisplayItem{Type: DisplayItemUserMessage}); err != nil {
		t.Fatalf("append first item: %v", err)
	}
	if err := appendRawDisplayLog(store.displayLogPath(sess.ID), `{"id":"partial"`); err != nil {
		t.Fatalf("append partial item: %v", err)
	}

	second, err := manager.AppendDisplayItem(sess.ID, DisplayItem{Type: DisplayItemAssistantMessage})
	if err != nil {
		t.Fatalf("append second item: %v", err)
	}
	if second.Sequence != 2 {
		t.Fatalf("expected second sequence 2, got %d", second.Sequence)
	}
	items, ok, err := manager.ListDisplayItems(sess.ID)
	if err != nil {
		t.Fatalf("list display items: %v", err)
	}
	if !ok {
		t.Fatal("expected session to exist")
	}
	assertDisplaySequences(t, items, []uint64{1, 2})
}

func TestFileStoreAppendDisplayItemSerializesAcrossStoreInstances(t *testing.T) {
	root := t.TempDir()
	firstStore, err := NewFileStore(root)
	if err != nil {
		t.Fatalf("create first store: %v", err)
	}
	secondStore, err := NewFileStore(root)
	if err != nil {
		t.Fatalf("create second store: %v", err)
	}
	manager := NewManagerWithStore(firstStore)
	sess, err := manager.CreateNew(t.TempDir())
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	const perStore = 50
	var wg sync.WaitGroup
	appendMany := func(store *FileStore) {
		defer wg.Done()
		for i := 0; i < perStore; i++ {
			if _, err := store.AppendDisplayItem(context.Background(), sess.ID, DisplayItem{Type: DisplayItemUserMessage}); err != nil {
				t.Errorf("append display item: %v", err)
				return
			}
		}
	}
	wg.Add(2)
	go appendMany(firstStore)
	go appendMany(secondStore)
	wg.Wait()

	items, ok, err := manager.ListDisplayItems(sess.ID)
	if err != nil {
		t.Fatalf("list display items: %v", err)
	}
	if !ok {
		t.Fatal("expected session to exist")
	}
	if len(items) != perStore*2 {
		t.Fatalf("expected %d items, got %d", perStore*2, len(items))
	}
	for idx, item := range items {
		want := uint64(idx + 1)
		if item.Sequence != want {
			t.Fatalf("expected sequence %d at index %d, got %d", want, idx, item.Sequence)
		}
	}
}

func TestManagerListDisplayItemsIgnoresPartialFinalLine(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	manager := NewManagerWithStore(store)
	sess, err := manager.CreateNew(t.TempDir())
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := manager.AppendDisplayItem(sess.ID, DisplayItem{Type: DisplayItemUserMessage}); err != nil {
		t.Fatalf("append item: %v", err)
	}
	if err := appendRawDisplayLog(store.displayLogPath(sess.ID), `{"id":"partial"`); err != nil {
		t.Fatalf("append partial item: %v", err)
	}

	items, ok, err := manager.ListDisplayItems(sess.ID)
	if err != nil {
		t.Fatalf("list display items: %v", err)
	}
	if !ok {
		t.Fatal("expected session to exist")
	}
	assertDisplaySequences(t, items, []uint64{1})
}

func TestFileStoreAppendDisplayItemIfIgnoresValidNoNewlineTail(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	manager := NewManagerWithStore(store)
	sess, err := manager.CreateNew(t.TempDir())
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := manager.AppendDisplayItem(sess.ID, DisplayItem{Type: DisplayItemUserMessage}); err != nil {
		t.Fatalf("append first item: %v", err)
	}
	rawTail := `{"id":"item_partial","sequence":2,"type":"turn_started","turn":{"id":"turn-partial"}}`
	if err := appendRawDisplayLog(store.displayLogPath(sess.ID), rawTail); err != nil {
		t.Fatalf("append valid no-newline tail: %v", err)
	}

	second, err := store.AppendDisplayItemIf(context.Background(), sess.ID, DisplayItem{Type: DisplayItemAssistantMessage}, func(items []DisplayItem) error {
		if len(items) != 1 {
			t.Fatalf("condition should see only durable items, got %#v", items)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("append conditional item: %v", err)
	}
	if second.Sequence != 2 {
		t.Fatalf("expected no-newline tail to be truncated before sequence assignment, got %d", second.Sequence)
	}
	items, ok, err := manager.ListDisplayItems(sess.ID)
	if err != nil {
		t.Fatalf("list display items: %v", err)
	}
	if !ok {
		t.Fatal("expected session to exist")
	}
	assertDisplaySequences(t, items, []uint64{1, 2})
}

func TestManagerListDisplayItemsRejectsMalformedCompleteLine(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	manager := NewManagerWithStore(store)
	sess, err := manager.CreateNew(t.TempDir())
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := manager.AppendDisplayItem(sess.ID, DisplayItem{Type: DisplayItemUserMessage}); err != nil {
		t.Fatalf("append item: %v", err)
	}
	if err := appendRawDisplayLog(store.displayLogPath(sess.ID), "{bad-json}\n"); err != nil {
		t.Fatalf("append malformed item: %v", err)
	}

	if _, _, err := manager.ListDisplayItems(sess.ID); err == nil {
		t.Fatal("expected malformed complete line to fail")
	}
}

func TestFileStoreListDisplayItemsFromOffset(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	manager := NewManagerWithStore(store)
	sess, err := manager.CreateNew(t.TempDir())
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := manager.AppendDisplayItem(sess.ID, DisplayItem{Type: DisplayItemUserMessage}); err != nil {
		t.Fatalf("append first item: %v", err)
	}

	first, ok, offset, err := store.ListDisplayItemsFromOffset(context.Background(), sess.ID, 0, 1)
	if err != nil {
		t.Fatalf("list from offset: %v", err)
	}
	if !ok {
		t.Fatal("expected session to exist")
	}
	assertDisplaySequences(t, first, []uint64{1})
	if offset <= 0 {
		t.Fatalf("expected positive next offset, got %d", offset)
	}

	if _, err := manager.AppendDisplayItem(sess.ID, DisplayItem{Type: DisplayItemAssistantMessage}); err != nil {
		t.Fatalf("append second item: %v", err)
	}
	if err := appendRawDisplayLog(store.displayLogPath(sess.ID), `{"id":"partial"`); err != nil {
		t.Fatalf("append partial item: %v", err)
	}

	second, ok, nextOffset, err := store.ListDisplayItemsFromOffset(context.Background(), sess.ID, offset, 10)
	if err != nil {
		t.Fatalf("list second from offset: %v", err)
	}
	if !ok {
		t.Fatal("expected session to exist")
	}
	assertDisplaySequences(t, second, []uint64{2})
	if nextOffset <= offset {
		t.Fatalf("expected offset to advance, got %d after %d", nextOffset, offset)
	}

	none, ok, finalOffset, err := store.ListDisplayItemsFromOffset(context.Background(), sess.ID, nextOffset, 10)
	if err != nil {
		t.Fatalf("list after complete tail: %v", err)
	}
	if !ok {
		t.Fatal("expected session to exist")
	}
	if len(none) != 0 {
		t.Fatalf("expected partial tail to be ignored, got %#v", none)
	}
	if finalOffset != nextOffset {
		t.Fatalf("partial tail should not advance offset: got %d want %d", finalOffset, nextOffset)
	}
}

func TestPageDisplayItemsDefaultsToRecentItemsInAscendingOrder(t *testing.T) {
	items := makeDisplayItems(60)

	page := PageDisplayItems(items, DisplayPageQuery{Limit: 50})

	if len(page.Items) != 50 {
		t.Fatalf("expected 50 items, got %d", len(page.Items))
	}
	if page.Items[0].Sequence != 11 || page.Items[49].Sequence != 60 {
		t.Fatalf("expected recent 11..60, got %d..%d", page.Items[0].Sequence, page.Items[49].Sequence)
	}
	if page.PrevCursor != "11" || page.NextCursor != "" || !page.HasMore {
		t.Fatalf("unexpected cursors: %#v", page)
	}
}

func TestPageDisplayItemsAfterCursor(t *testing.T) {
	items := makeDisplayItems(5)

	page := PageDisplayItems(items, DisplayPageQuery{Limit: 2, After: 2, HasAfter: true})

	assertDisplaySequences(t, page.Items, []uint64{3, 4})
	if page.PrevCursor != "3" || page.NextCursor != "4" {
		t.Fatalf("unexpected cursors: %#v", page)
	}
}

func TestPageDisplayItemsBeforeCursor(t *testing.T) {
	items := makeDisplayItems(5)

	page := PageDisplayItems(items, DisplayPageQuery{Limit: 2, Before: 5, HasBefore: true})

	assertDisplaySequences(t, page.Items, []uint64{3, 4})
	if page.PrevCursor != "3" || page.NextCursor != "4" {
		t.Fatalf("unexpected cursors: %#v", page)
	}
}

func TestDisplayLogDoesNotFollowRuntimeHistoryReplacement(t *testing.T) {
	manager := newDisplayLogTestManager(t)
	sess, err := manager.CreateNew(t.TempDir())
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	sess.AgentSnapshot.History = []protocol.Message{protocol.NewUserMessage("old prompt")}
	if err := manager.Save(sess); err != nil {
		t.Fatalf("save session: %v", err)
	}
	if _, err := manager.AppendDisplayItem(sess.ID, DisplayItem{
		Type:    DisplayItemUserMessage,
		Role:    string(protocol.RoleUser),
		Content: []DisplayContentPart{{Type: string(protocol.ContentTypeText), Text: "old prompt"}},
	}); err != nil {
		t.Fatalf("append display item: %v", err)
	}

	sess.AgentSnapshot.History = []protocol.Message{protocol.NewUserMessage("compressed summary")}
	if err := manager.Save(sess); err != nil {
		t.Fatalf("save compressed session: %v", err)
	}

	items, ok, err := manager.ListDisplayItems(sess.ID)
	if err != nil {
		t.Fatalf("list display items: %v", err)
	}
	if !ok {
		t.Fatal("expected session to exist")
	}
	if len(items) != 1 {
		t.Fatalf("expected display log to remain, got %d items", len(items))
	}
	if got := items[0].Content[0].Text; got != "old prompt" {
		t.Fatalf("expected original display text, got %q", got)
	}
}

func TestDisplayLogIsStoredOutsideSessionJSON(t *testing.T) {
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	manager := NewManagerWithStore(store)
	sess, err := manager.CreateNew(t.TempDir())
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := manager.AppendDisplayItem(sess.ID, DisplayItem{
		Type:    DisplayItemUserMessage,
		Role:    string(protocol.RoleUser),
		Content: []DisplayContentPart{{Type: string(protocol.ContentTypeText), Text: "display prompt"}},
	}); err != nil {
		t.Fatalf("append display item: %v", err)
	}

	sessionJSON, err := os.ReadFile(store.sessionPath(sess.ID))
	if err != nil {
		t.Fatalf("read session json: %v", err)
	}
	if strings.Contains(string(sessionJSON), "display prompt") || strings.Contains(string(sessionJSON), "display_log") {
		t.Fatalf("session json should not contain display log: %s", string(sessionJSON))
	}
	logData, err := os.ReadFile(store.displayLogPath(sess.ID))
	if err != nil {
		t.Fatalf("read display log: %v", err)
	}
	if !strings.Contains(string(logData), "display prompt") {
		t.Fatalf("display log should contain item, got %s", string(logData))
	}
}

func newDisplayLogTestManager(t *testing.T) *Manager {
	t.Helper()
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	return NewManagerWithStore(store)
}

func makeDisplayItems(count int) []DisplayItem {
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	items := make([]DisplayItem, 0, count)
	for i := 1; i <= count; i++ {
		items = append(items, DisplayItem{
			ID:        "item",
			Sequence:  uint64(i),
			Type:      DisplayItemUserMessage,
			CreatedAt: base.Add(time.Duration(i) * time.Second),
		})
	}
	return items
}

func assertDisplaySequences(t *testing.T, items []DisplayItem, expected []uint64) {
	t.Helper()
	if len(items) != len(expected) {
		t.Fatalf("expected %d items, got %d", len(expected), len(items))
	}
	for idx, sequence := range expected {
		if items[idx].Sequence != sequence {
			t.Fatalf("item %d: expected sequence %d, got %d", idx, sequence, items[idx].Sequence)
		}
	}
}

func appendRawDisplayLog(path string, data string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	_, err = file.WriteString(data)
	return err
}
