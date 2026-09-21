package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestDisplayIndexWindowAndPagination(t *testing.T) {
	ctx := context.Background()
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sess, err := NewManagerWithStore(store).CreateNew(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	yesterday := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)
	today := yesterday.Add(24 * time.Hour)
	items := []DisplayItem{{Type: DisplayItemUserMessage, CreatedAt: yesterday}, {Type: DisplayItemAssistantMessage, CreatedAt: yesterday.Add(time.Hour)}, {Type: DisplayItemUserMessage, CreatedAt: today}, {Type: DisplayItemProfileChanged, CreatedAt: today.Add(time.Hour)}}
	if _, err = store.AppendDisplayItems(ctx, sess.ID, items); err != nil {
		t.Fatal(err)
	}
	window := DisplayTimeWindow{Since: &yesterday, Until: &today}
	matched, err := store.SessionDialogueActivity(ctx, sess.ID, window)
	if err != nil || matched == nil || !matched.Equal(yesterday.Add(time.Hour)) {
		t.Fatalf("match=%v err=%v", matched, err)
	}
	page, exists, err := store.ReadDisplayPage(ctx, sess.ID, DisplayPageQuery{Limit: 1}, window)
	if err != nil || !exists || len(page.Items) != 1 || page.Items[0].Sequence != 2 || page.PrevCursor != "2" || page.NextCursor != "" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	page, _, err = store.ReadDisplayPage(ctx, sess.ID, DisplayPageQuery{Limit: 1, HasBefore: true, Before: 2}, window)
	if err != nil || len(page.Items) != 1 || page.Items[0].Sequence != 1 || page.NextCursor != "1" {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	start := today.Add(time.Minute)
	matched, err = store.SessionDialogueActivity(ctx, sess.ID, DisplayTimeWindow{Since: &start})
	if err != nil || matched != nil {
		t.Fatalf("configuration is not dialogue: %v %v", matched, err)
	}
}

func TestDisplayIndexBackfillRepairAndBoundedRead(t *testing.T) {
	ctx := context.Background()
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sess, err := NewManagerWithStore(store).CreateNew(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	items := make([]DisplayItem, 2000)
	for index := range items {
		items[index] = DisplayItem{Type: DisplayItemUserMessage, Content: []DisplayContentPart{{Type: "text", Text: strings.Repeat("x", 1024)}}}
	}
	if _, err = store.AppendDisplayItems(ctx, sess.ID, items); err != nil {
		t.Fatal(err)
	}
	if err = store.InvalidateDisplayIndex(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.ReadDisplayPage(ctx, sess.ID, DisplayPageQuery{Limit: 2}, DisplayTimeWindow{}); !errors.Is(err, ErrDisplayIndexPending) {
		t.Fatalf("pending: %v", err)
	}
	if err = store.RebuildDisplayIndex(ctx); err != nil {
		t.Fatal(err)
	}
	// Corrupt an unselected record while preserving the freshness marker. The
	// newest page still succeeds: its reader never decodes that older record.
	file, err := os.OpenFile(store.displayLogPath(sess.ID), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteAt([]byte("!"), 0); err != nil {
		t.Fatal(err)
	}
	file.Close()
	info, err := os.Stat(store.displayLogPath(sess.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.index.db.Exec(`UPDATE display_index_files SET mtime=? WHERE session_id=?`, info.ModTime().UnixNano(), sess.ID); err != nil {
		t.Fatal(err)
	}
	page, _, err := store.ReadDisplayPage(ctx, sess.ID, DisplayPageQuery{Limit: 2}, DisplayTimeWindow{})
	if err != nil || len(page.Items) != 2 || page.Items[0].Sequence != 1999 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
	rows, err := store.index.db.Query(`EXPLAIN QUERY PLAN SELECT MAX(message_at) FROM display_items WHERE session_id=? AND dialogue=1 AND message_at>=? AND message_at<?`, sess.ID, 0, time.Now().UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan string
	for rows.Next() {
		var a, b, c int
		var detail string
		if err = rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail
	}
	if !strings.Contains(plan, "COVERING INDEX idx_display_dialogue_time") {
		t.Fatalf("unindexed activity query: %s", plan)
	}
}

func BenchmarkDisplayIndexRecentPage(b *testing.B) {
	ctx := context.Background()
	store, err := NewFileStore(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	sess, err := NewManagerWithStore(store).CreateNew(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	items := make([]DisplayItem, 20000)
	for index := range items {
		items[index] = DisplayItem{Type: DisplayItemUserMessage, Content: []DisplayContentPart{{Type: "text", Text: strings.Repeat("x", 1024)}}}
	}
	if _, err = store.AppendDisplayItems(ctx, sess.ID, items); err != nil {
		b.Fatal(err)
	}
	if err = store.RebuildDisplayIndex(ctx); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for index := 0; index < b.N; index++ {
		page, _, err := store.ReadDisplayPage(ctx, sess.ID, DisplayPageQuery{Limit: 20}, DisplayTimeWindow{})
		if err != nil || len(page.Items) != 20 {
			b.Fatalf("read: %v", err)
		}
	}
}

func TestDisplayIndexReplacementInvalidation(t *testing.T) {
	ctx := context.Background()
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sess, err := NewManagerWithStore(store).CreateNew(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.AppendDisplayItem(ctx, sess.ID, DisplayItem{Type: DisplayItemUserMessage}); err != nil {
		t.Fatal(err)
	}
	if err = store.InvalidateDisplayIndex(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	replacement := DisplayItem{Sequence: 7, Type: DisplayItemAssistantMessage, CreatedAt: time.Now(), Content: []DisplayContentPart{{Type: "text", Text: strings.Repeat("replacement", 100)}}}
	data, err := json.Marshal(replacement)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(store.displayLogPath(sess.ID), append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.ReadDisplayPage(ctx, sess.ID, DisplayPageQuery{Limit: 20}, DisplayTimeWindow{}); !errors.Is(err, ErrDisplayIndexPending) {
		t.Fatalf("replacement must not reuse offsets: %v", err)
	}
	if err = store.RebuildDisplayIndex(ctx); err != nil {
		t.Fatal(err)
	}
	page, _, err := store.ReadDisplayPage(ctx, sess.ID, DisplayPageQuery{Limit: 20}, DisplayTimeWindow{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Sequence != 7 {
		t.Fatalf("replacement page=%+v err=%v", page, err)
	}
	if err = store.Delete(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err = store.index.db.QueryRow(`SELECT COUNT(*) FROM display_items WHERE session_id=?`, sess.ID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("deleted index rows=%d err=%v", remaining, err)
	}
}

func TestDisplayIndexIgnoresIncompleteCrashTail(t *testing.T) {
	ctx := context.Background()
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sess, err := NewManagerWithStore(store).CreateNew(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.AppendDisplayItem(ctx, sess.ID, DisplayItem{Type: DisplayItemUserMessage}); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(store.displayLogPath(sess.ID), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteString(`{"unfinished":`); err != nil {
		t.Fatal(err)
	}
	file.Close()
	if err = store.RebuildDisplayIndex(ctx); err != nil {
		t.Fatal(err)
	}
	page, _, err := store.ReadDisplayPage(ctx, sess.ID, DisplayPageQuery{Limit: 20}, DisplayTimeWindow{})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("crash-tail page=%+v err=%v", page, err)
	}
	if _, err = store.AppendDisplayItem(ctx, sess.ID, DisplayItem{Type: DisplayItemAssistantMessage}); err != nil {
		t.Fatal(err)
	}
	page, _, err = store.ReadDisplayPage(ctx, sess.ID, DisplayPageQuery{Limit: 20}, DisplayTimeWindow{})
	if err != nil || len(page.Items) != 2 {
		t.Fatalf("recovered page=%+v err=%v", page, err)
	}
}
