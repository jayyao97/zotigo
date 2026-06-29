package session

import (
	"os"
	"strings"
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
