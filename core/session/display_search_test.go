package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/catalogschema"
	"github.com/jayyao97/zotigo/internal/testutil/catalogtest"
)

func TestDisplaySearchBoundsAndContent(t *testing.T) {
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
	var items []DisplayItem
	for n := 0; n < 300; n++ {
		items = append(items, DisplayItem{Type: DisplayItemAssistantMessage, Content: []DisplayContentPart{{Type: "text", Text: fmt.Sprintf("message %d CAFÉ 中文 100%%", n)}}})
	}
	items = append(items, DisplayItem{Type: DisplayItemAssistantMessage, Content: []DisplayContentPart{{Type: "tool_result", ToolResult: &DisplayToolResult{Text: "secret tool"}}, {Type: "reasoning", Text: "secret thought"}}})
	items, err = store.AppendDisplayItems(ctx, sess.ID, items)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.RebuildDisplayIndex(ctx); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"café", "中文", "100%"} {
		result, err := store.SearchDisplayMessages(ctx, sess.ID, q, 250)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Hits) != 250 || !result.Truncated || result.Hits[0].ID != items[299].ID {
			t.Fatalf("bad result for %q: %+v", q, result)
		}
	}
	for _, q := range []string{"secret", "' OR 1=1 --", "not present"} {
		result, err := store.SearchDisplayMessages(ctx, sess.ID, q, 250)
		if err != nil || len(result.Hits) != 0 {
			t.Fatalf("unexpected match %q: %+v %v", q, result, err)
		}
	}
	other, err := NewManagerWithStore(store).CreateNew(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.SearchDisplayMessages(ctx, other.ID, "中文", 250)
	if err != nil || len(result.Hits) != 0 {
		t.Fatalf("cross-session leak: %+v %v", result, err)
	}
	seq, err := store.DisplaySequence(ctx, sess.ID, items[40].ID)
	if err != nil || seq != items[40].Sequence {
		t.Fatalf("message lookup: %d %v", seq, err)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = store.SearchDisplayMessages(ctx, sess.ID, "中文", 250); err == nil {
		t.Fatal("cancel ignored")
	}
}

func TestDisplaySearchQueryPlan(t *testing.T) {
	store, err := NewFileStore(catalogtest.NewRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rows, err := store.index.db.Query(`EXPLAIN QUERY PLAN SELECT item_id,sequence FROM display_items WHERE session_id=? AND content_kind='conversation' AND instr(search_text,?)>0 ORDER BY sequence DESC LIMIT ?`, "s", "q", 251)
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
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "idx_display_content_sequence") || strings.Contains(plan, "TEMP B-TREE") {
		t.Fatal(plan)
	}
	t.Log(plan)
}

func BenchmarkDisplaySearch(b *testing.B) {
	for _, count := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
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
			tx, err := store.index.db.Begin()
			if err != nil {
				b.Fatal(err)
			}
			stmt, err := tx.Prepare(`INSERT INTO display_items(session_id,sequence,message_at,dialogue,offset,length,content_kind,item_id,search_text) VALUES(?,?,?,1,0,0,'conversation',?,?)`)
			if err != nil {
				b.Fatal(err)
			}
			for n := 0; n < count; n++ {
				if _, err = stmt.Exec(sess.ID, n+1, time.Now().UnixNano(), fmt.Sprint(n), strings.Repeat("ordinary conversational prose 中文 ", 32)); err != nil {
					b.Fatal(err)
				}
			}
			_ = stmt.Close()
			if err = tx.Commit(); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				if _, err = store.SearchDisplayMessages(ctx, sess.ID, "missing needle", 250); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestDisplaySearchMigrationBackfill(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := NewManagerWithStore(store).CreateNew(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.AppendDisplayItem(ctx, sess.ID, DisplayItem{Type: DisplayItemUserMessage, Content: []DisplayContentPart{{Type: "text", Text: "old searchable 中文"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := catalogschema.OpenDatabase(root)
	if err != nil {
		t.Fatal(err)
	}
	if err = catalogschema.Migrate(ctx, db, 11); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	store, err = NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err = store.SearchDisplayMessages(ctx, sess.ID, "中文", 250); !errors.Is(err, ErrDisplayIndexPending) {
		t.Fatalf("must not silently search incomplete index: %v", err)
	}
	for range 2 {
		if err = store.RebuildDisplayIndex(ctx); err != nil {
			t.Fatal(err)
		}
		result, err := store.SearchDisplayMessages(ctx, sess.ID, "中文", 250)
		if err != nil || len(result.Hits) != 1 {
			t.Fatalf("backfill: %+v %v", result, err)
		}
	}
}

func TestDisplaySearchDoesNotOccupyWriterConnection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store, err := NewFileStore(catalogtest.NewRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sess, err := NewManagerWithStore(store).CreateNew(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.AppendDisplayItem(ctx, sess.ID, DisplayItem{Type: DisplayItemUserMessage, Content: []DisplayContentPart{{Type: "text", Text: "needle"}}})
	if err != nil {
		t.Fatal(err)
	}
	// Reserve the sole writer connection. Search must use its independent reader.
	writer, err := store.index.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.SearchDisplayMessages(ctx, sess.ID, "needle", 250)
	_ = writer.Close()
	if err != nil || len(result.Hits) != 1 {
		t.Fatalf("search waited for writer connection: %+v %v", result, err)
	}
	// Keep a WAL read snapshot open while a display append commits.
	rows, err := store.index.searchDB.QueryContext(ctx, `SELECT search_text FROM display_items WHERE session_id=?`, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("missing fixture")
	}
	if _, err = store.AppendDisplayItem(ctx, sess.ID, DisplayItem{Type: DisplayItemUserMessage, Content: []DisplayContentPart{{Type: "text", Text: "concurrent append"}}}); err != nil {
		t.Fatal(err)
	}
}
