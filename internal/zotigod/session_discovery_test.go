package zotigod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/session"
)

func TestSessionDiscoverySQLPagingAndPlan(t *testing.T) {
	h, caller := newSessionToolFixture(t)
	ctx := context.Background()
	stamp := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	var expected []string
	for i := 0; i < 12; i++ {
		target := newSession(caller.Stored.WorkingDirectory, "channel-default")
		target.ID = fmt.Sprintf("zz_discovery_%02d", i)
		if err := h.persistSession(ctx, target); err != nil {
			t.Fatal(err)
		}
		if _, err := h.catalog.AssignSession(ctx, target.ID, caller.WorkspaceID); err != nil {
			t.Fatal(err)
		}
		at := stamp
		if i%2 == 0 {
			at = stamp.Add(-48 * time.Hour)
		} else {
			expected = append(expected, target.ID)
		}
		if _, err := h.store.AppendDisplayItem(ctx, target.ID, session.DisplayItem{Type: session.DisplayItemAssistantMessage, CreatedAt: at, Content: []session.DisplayContentPart{{Type: "text", Text: "result"}}}); err != nil {
			t.Fatal(err)
		}
	}
	query := sessionDiscoveryQuery{Limit: 2, WorkspaceID: caller.WorkspaceID, ActivitySince: "2026-10-01T00:00:00Z", ActivityUntil: "2026-10-02T00:00:00Z"}
	var got []string
	for pages := 0; ; pages++ {
		if pages > 4 {
			t.Fatal("pagination did not terminate")
		}
		raw, _ := json.Marshal(query)
		result, err := h.listToolSessions(ctx, caller, raw)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _ := json.Marshal(result)
		var page struct {
			Sessions []toolSessionSummary `json:"sessions"`
			Next     string               `json:"next_after_id"`
		}
		if err := json.Unmarshal(encoded, &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Sessions) > 2 {
			t.Fatalf("unbounded page: %s", encoded)
		}
		for _, row := range page.Sessions {
			got = append(got, row.ID)
			if row.LastMatchedMessageAt == nil || !row.LastMatchedMessageAt.Equal(stamp) {
				t.Fatalf("wrong activity: %+v", row)
			}
		}
		if page.Next == "" {
			break
		}
		query.AfterID = page.Next
	}
	if strings.Join(got, ",") != strings.Join(expected, ",") {
		t.Fatalf("got %v want %v", got, expected)
	}
	lastLog := filepath.Join(h.sessionStoreRoot(), "sessions", "zz_discovery_11.display.jsonl")
	log, err := os.OpenFile(lastLog, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	query.AfterID = ""
	raw, _ := json.Marshal(query)
	if _, err := h.listToolSessions(ctx, caller, raw); err != nil {
		t.Fatalf("later stale log blocked first page: %v", err)
	}
	query.AfterID = "zz_discovery_07"
	raw, _ = json.Marshal(query)
	if _, err := h.listToolSessions(ctx, caller, raw); !errors.Is(err, session.ErrDisplayIndexPending) {
		t.Fatalf("later page failed to detect stale log: %v", err)
	}
	db, err := openSessionDiscovery(ctx, h.catalog.RootDir(), h.sessionStoreRoot())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	where, args := sessionDiscoveryScope(caller, query)
	window, err := toolTimeWindow(query.ActivitySince, query.ActivityUntil)
	if err != nil {
		t.Fatal(err)
	}
	sqlQuery, params := sessionDiscoverySQL(where, args, window, query.Limit)
	rows, err := db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+sqlQuery, params...)
	if err != nil {
		t.Fatal(err)
	}
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	t.Log(plan.String())
	if strings.Contains(plan.String(), "TEMP B-TREE") || !strings.Contains(plan.String(), "idx_display_dialogue_time") || !strings.Contains(plan.String(), "session_id>?") {
		t.Fatalf("query lost indexed seek/time lookup: %s", plan.String())
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM history.sessions`); err == nil {
		t.Fatal("history connection must be read-only")
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM session_organization`); err == nil {
		t.Fatal("catalog connection must be read-only")
	}
}

func TestSessionDiscoveryRejectsStaleTimeIndex(t *testing.T) {
	h, caller := newSessionToolFixture(t)
	ctx := context.Background()
	path := filepath.Join(h.sessionStoreRoot(), "sessions", caller.SessionID+".display.jsonl")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	// Even an index with no matching old messages must not silently exclude a
	// potentially matching message that has not been indexed yet.
	_, err = h.listToolSessions(ctx, caller, json.RawMessage(`{"activity_until":"2000-01-01T00:00:00Z"}`))
	if !errors.Is(err, session.ErrDisplayIndexPending) {
		t.Fatalf("stale index accepted: %v", err)
	}
	// Scope filtering happens in SQL before integrity checks.
	_, err = h.listToolSessions(ctx, caller, json.RawMessage(`{"workspace_id":"missing","activity_until":"2000-01-01T00:00:00Z"}`))
	if err != nil {
		t.Fatalf("unrelated stale index blocked query: %v", err)
	}
	if _, err := h.listToolSessions(ctx, caller, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("unfiltered discovery needs no display index: %v", err)
	}
}
