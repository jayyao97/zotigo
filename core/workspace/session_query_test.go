package workspace

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestSessionQueryPaginationFiltersAndIndex(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := t.Context()
	for i := 0; i < 1000; i++ {
		id := fmt.Sprintf("s%04d", i)
		if _, err := store.db.Exec(`INSERT INTO sessions(id,working_directory,last_prompt,created_at,updated_at) VALUES(?,'/work',?,1,?)`, id, "title "+id, i/2+1); err != nil {
			t.Fatal(err)
		}
		if _, err := store.EnsureSessionOrganization(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	query := SessionQuery{Limit: 13, ChannelMode: "all"}
	seen := map[string]bool{}
	for {
		rows, err := store.QuerySessions(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			if seen[row.ID] {
				t.Fatalf("duplicate %s", row.ID)
			}
			seen[row.ID] = true
		}
		last := rows[len(rows)-1]
		query.Before, query.BeforeID = last.UpdatedAt, last.ID
	}
	if len(seen) != 1000 {
		t.Fatalf("lost rows: %d", len(seen))
	}
	if _, err := store.SetSessionPinned(ctx, "s0999", true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO session_read_state(session_id,version,read_version) VALUES('s0000',1,0)`); err != nil {
		t.Fatal(err)
	}
	check := func(q SessionQuery, want []string) {
		t.Helper()
		q.Limit = 20
		rows, err := store.QuerySessions(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		got := []string{}
		for _, row := range rows {
			got = append(got, row.ID)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("query %+v: %v != %v", q, got, want)
		}
	}
	unread := true
	check(SessionQuery{Unread: &unread, ChannelMode: "all"}, []string{"s0000"})
	check(SessionQuery{Since: 500, ChannelMode: "none", ChannelIDs: []string{"s0998", "s0999"}}, []string{})
	check(SessionQuery{Since: 500, ChannelMode: "pinned", ChannelIDs: []string{"s0998", "s0999"}}, []string{"s0999"})
	check(SessionQuery{Search: "title s0200", ChannelMode: "all"}, []string{"s0200"})
	check(SessionQuery{Search: "s0200 title", ChannelMode: "all"}, []string{"s0200"})
	if _, err := store.SetSessionArchived(ctx, "s0999", true); err != nil {
		t.Fatal(err)
	}
	check(SessionQuery{Pinned: true, ChannelMode: "all"}, []string{})
	check(SessionQuery{IDs: []string{"s0999"}, IncludeArchived: true, ChannelMode: "all"}, []string{"s0999"})
	sql, args := sessionQuerySQL(SessionQuery{Limit: 20, ChannelMode: "all"})
	plan, err := store.db.Query("EXPLAIN QUERY PLAN "+sql, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	usesIndex := false
	for plan.Next() {
		var id, parent, unused int
		var detail string
		if err := plan.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "idx_sessions_activity_page") {
			usesIndex = true
		}
		if strings.Contains(detail, "USE TEMP B-TREE FOR ORDER BY") {
			t.Fatalf("unbounded sort: %s", detail)
		}
	}
	if !usesIndex {
		t.Fatal("activity query did not use time index")
	}
}

func TestSessionQueryWorkspacePinsAndOrdering(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, "Project")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := store.CreateWorkspacePlan(ctx, project.ID, "Work", nil)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = store.ProvisionWorkspace(ctx, workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if _, err := store.db.Exec(`INSERT INTO sessions(id,working_directory,created_at,updated_at) VALUES(?,'/work',1,100)`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := store.AssignSession(ctx, id, workspace.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.SetSessionPosition(ctx, "a", -9000000000000); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE workspaces SET pinned_at=1,pinned_position=0 WHERE id=?`, workspace.ID); err != nil {
		t.Fatal(err)
	}
	q := SessionQuery{WorkspaceID: workspace.ID, ChannelMode: "pinned", ChannelIDs: []string{"a", "b", "c"}, Limit: 1}
	rows, err := store.QuerySessions(ctx, q)
	if err != nil || len(rows) != 1 || rows[0].ID != "a" {
		t.Fatalf("first: %+v %v", rows, err)
	}
	q.Before, q.BeforeID, q.BeforePosition = rows[0].UpdatedAt, rows[0].ID, rows[0].Position
	rows, err = store.QuerySessions(ctx, q)
	if err != nil || len(rows) != 1 || rows[0].ID == "a" {
		t.Fatalf("second: %+v %v", rows, err)
	}
}

func TestSparseUnreadQueryUsesUnreadIndex(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	unread := true
	query, args := sessionQuerySQL(SessionQuery{Unread: &unread, ChannelMode: "all", Limit: 20})
	rows, err := store.db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	plan := ""
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	if !strings.Contains(plan, "idx_session_read_state_unread") || strings.Contains(plan, "SCAN s ") {
		t.Fatalf("unread poll scans all sessions: %s", plan)
	}
}
