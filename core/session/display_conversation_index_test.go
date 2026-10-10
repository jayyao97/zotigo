package session

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/catalogschema"
	"github.com/jayyao97/zotigo/internal/testutil/catalogtest"
)

func TestConversationIndexFiltersBeforePagination(t *testing.T) {
	ctx := context.Background()
	store, err := NewFileStore(catalogtest.NewRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sess, err := NewManagerWithStore(store).CreateNew(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	items := []DisplayItem{{Type: DisplayItemUserMessage, CreatedAt: stamp, Content: []DisplayContentPart{{Type: "image", Image: &DisplayMediaPart{FileID: "photo"}}}}}
	for range 120 {
		items = append(items, DisplayItem{Type: DisplayItemAssistantMessage, CreatedAt: stamp, Content: []DisplayContentPart{{Type: "tool_result", ToolResult: &DisplayToolResult{Text: "tool body"}}}})
	}
	items = append(items, DisplayItem{Type: DisplayItemAssistantMessage, CreatedAt: stamp, Content: []DisplayContentPart{{Type: "tool_call"}, {Type: "text", Text: "answer"}}}, DisplayItem{Type: DisplayItemTurnCompleted, CreatedAt: stamp})
	items, err = store.AppendDisplayItems(ctx, sess.ID, items)
	if err != nil {
		t.Fatal(err)
	}
	end := stamp.Add(time.Hour)
	window := DisplayTimeWindow{Since: &stamp, Until: &end}
	for _, query := range []DisplayPageQuery{
		{Limit: 2, ContentKind: DisplayContentConversation},
		{Limit: 1, ContentKind: DisplayContentConversation},
		{Limit: 1, ContentKind: DisplayContentConversation, HasBefore: true, Before: 122},
		{Limit: 1, ContentKind: DisplayContentConversation, HasAfter: true, After: 1},
	} {
		page, exists, err := store.ReadDisplayPage(ctx, sess.ID, query, window)
		if err != nil || !exists {
			t.Fatalf("read: %v", err)
		}
		want := PageDisplayItems(items, query)
		if !reflect.DeepEqual(page, want) {
			t.Fatalf("page=%+v want %+v", page, want)
		}
	}
	page, _, err := store.ReadDisplayPage(ctx, sess.ID, DisplayPageQuery{Limit: 1000}, window)
	if err != nil || len(page.Items) != len(items) {
		t.Fatalf("UI history changed: %d, %v", len(page.Items), err)
	}
	rows, err := store.index.db.Query(`EXPLAIN QUERY PLAN SELECT offset,length FROM display_items WHERE session_id=? AND content_kind=? AND message_at>=? AND message_at<? AND sequence<? ORDER BY sequence DESC LIMIT ?`, sess.ID, DisplayContentConversation, stamp.UnixNano(), end.UnixNano(), 123, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan string
	for rows.Next() {
		var a, b, c int
		var detail string
		if err := rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "idx_display_content_sequence") || strings.Contains(plan, "TEMP B-TREE") {
		t.Fatalf("inefficient query plan: %s", plan)
	}
}

func TestConversationIndexMigrationBackfillResumes(t *testing.T) {
	root, ctx := t.TempDir(), context.Background()
	store, err := NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := NewManagerWithStore(store).CreateNew(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	items := make([]DisplayItem, 600)
	for i := range items {
		items[i] = DisplayItem{Type: DisplayItemAssistantMessage, Content: []DisplayContentPart{{Type: "tool_result"}}}
	}
	items[0] = DisplayItem{Type: DisplayItemUserMessage, Content: []DisplayContentPart{{Type: "image"}}}
	items[599] = DisplayItem{Type: DisplayItemAssistantMessage, Content: []DisplayContentPart{{Type: "text", Text: "done"}}}
	if _, err := store.AppendDisplayItems(ctx, sess.ID, items); err != nil {
		t.Fatal(err)
	}
	if err := store.RebuildDisplayIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// Exercise the actual SQL down/up pair, not a hand-built approximate schema.
	db, err := catalogschema.OpenDatabase(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := catalogschema.Migrate(ctx, db, 10); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM display_items`).Scan(&count); err != nil || count != 600 {
		t.Fatalf("downgrade lost rows: %d, %v", count, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	query := DisplayPageQuery{Limit: 100, ContentKind: DisplayContentConversation}
	if _, _, err := store.ReadDisplayPage(ctx, sess.ID, query, DisplayTimeWindow{}); !errors.Is(err, ErrDisplayIndexPending) {
		t.Fatalf("upgrade exposed incomplete history: %v", err)
	}
	// One maintenance batch then process restart: its committed offset resumes.
	if err := store.syncDisplayIndex(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ReadDisplayPage(ctx, sess.ID, query, DisplayTimeWindow{}); !errors.Is(err, ErrDisplayIndexPending) {
		t.Fatalf("partial backfill exposed history: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.RebuildDisplayIndex(ctx); err != nil {
		t.Fatal(err)
	}
	page, _, err := store.ReadDisplayPage(ctx, sess.ID, query, DisplayTimeWindow{})
	if err != nil || len(page.Items) != 2 || page.Items[0].Sequence != 1 || page.Items[1].Sequence != 600 {
		t.Fatalf("backfilled page=%+v err=%v", page, err)
	}
	if err := store.index.db.QueryRow(`SELECT COUNT(*) FROM display_items WHERE content_kind='unknown'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unclassified rows: %d, %v", count, err)
	}
	if err := store.RebuildDisplayIndex(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.index.db.QueryRow(`SELECT COUNT(*) FROM display_items`).Scan(&count); err != nil || count != 600 {
		t.Fatalf("rebuild not idempotent: %d, %v", count, err)
	}
}
