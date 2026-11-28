package zotigod

import (
	"database/sql"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/testutil/sessionlogtest"
)

func TestCatalogResumedSessionActivity(t *testing.T) {
	handler, _, catalog, workspace := newCatalogSessionFixture(t)
	sessionlogtest.WriteProfileConfig(t, workspace.RootPath)
	var resumed Session
	for range 2 {
		response := requestCatalog(t, handler, http.MethodPost, "/sessions", `{"workspace_id":`+quotedJSON(t, workspace.ID)+`}`)
		decodeCatalogData(t, response, &resumed)
	}
	store, err := zotigosession.NewFileStore(catalog.RootDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snapshot, err := store.Get(t.Context(), resumed.ID)
	if err != nil || snapshot == nil {
		t.Fatalf("read session: %v", err)
	}
	nextDay := resumed.CreatedAt.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	for _, kind := range []zotigosession.DisplayItemType{zotigosession.DisplayItemUserMessage, zotigosession.DisplayItemSteeringMessage, zotigosession.DisplayItemAssistantMessage} {
		nextDay = nextDay.Add(time.Second)
		if _, err := store.AppendDisplayItems(t.Context(), resumed.ID, []zotigosession.DisplayItem{{Type: kind, CreatedAt: nextDay}}); err != nil {
			t.Fatal(err)
		}
		var page struct{ Sessions []sessionProjection }
		decodeCatalogData(t, requestCatalog(t, handler, http.MethodGet, "/catalog/sessions?page=true&since="+nextDay.Format(time.RFC3339Nano), ""), &page)
		if len(page.Sessions) != 1 || page.Sessions[0].Runtime.ID != resumed.ID || !page.Sessions[0].Runtime.UpdatedAt.Equal(nextDay) {
			t.Fatalf("%s: expected only the resumed session at %s, got %+v", kind, nextDay, page.Sessions)
		}
		// A worker can save a snapshot whose metadata predates the dialogue.
		if err := store.Put(t.Context(), snapshot); err != nil {
			t.Fatal(err)
		}
		var single sessionProjection
		decodeCatalogData(t, requestCatalog(t, handler, http.MethodGet, "/catalog/sessions/"+resumed.ID, ""), &single)
		if !single.Runtime.UpdatedAt.Equal(nextDay) {
			t.Fatalf("single projection: got %s, want %s", single.Runtime.UpdatedAt, nextDay)
		}
	}
}

func TestCatalogSessionPageAndMetadataOnlySnapshot(t *testing.T) {
	handler, _, catalog, workspace := newCatalogSessionFixture(t)
	sessionlogtest.WriteProfileConfig(t, workspace.RootPath)
	ids := []string{}
	for range 5 {
		response := requestCatalog(t, handler, http.MethodPost, "/sessions", `{"workspace_id":`+quotedJSON(t, workspace.ID)+`}`)
		if response.Code != http.StatusCreated {
			t.Fatal(response.Body.String())
		}
		var session Session
		decodeCatalogData(t, response, &session)
		ids = append(ids, session.ID)
	}
	db, err := sql.Open("sqlite", filepath.Join(catalog.RootDir(), "catalog.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// The registry keeps its original policy; indexed metadata is authoritative.
	if _, err := db.Exec(`UPDATE sessions SET approval_policy='full_access', model='durable-model' WHERE id=?`, ids[0]); err != nil {
		t.Fatal(err)
	}
	response := requestCatalog(t, handler, http.MethodGet, "/catalog/sessions?page=true&ids="+ids[0], "")
	var single struct {
		Sessions []sessionProjection `json:"sessions"`
	}
	decodeCatalogData(t, response, &single)
	if len(single.Sessions) != 1 || string(single.Sessions[0].Runtime.ApprovalPolicy) != "full_access" || single.Sessions[0].Runtime.Model != "durable-model" {
		t.Fatalf("durable metadata lost: %s", response.Body.String())
	}
	cursor := ""
	seen := map[string]bool{}
	for range 4 {
		path := "/catalog/sessions?page=true&limit=2&workspace_id=" + workspace.ID
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		response := requestCatalog(t, handler, http.MethodGet, path, "")
		if response.Code != http.StatusOK {
			t.Fatal(response.Body.String())
		}
		var page struct {
			Sessions []sessionProjection `json:"sessions"`
			Next     string              `json:"next_cursor"`
		}
		decodeCatalogData(t, response, &page)
		if len(page.Sessions) > 2 {
			t.Fatal("limit ignored")
		}
		for _, projection := range page.Sessions {
			id := projection.Runtime.ID
			if seen[id] {
				t.Fatal("duplicate", id)
			}
			seen[id] = true
			if projection.Runtime.ReadState == nil {
				t.Fatal("missing read state")
			}
		}
		cursor = page.Next
		if cursor == "" {
			break
		}
	}
	if len(seen) != 5 {
		t.Fatalf("lost sessions: %v", seen)
	}
	snapshotResponse := requestCatalog(t, handler, http.MethodGet, "/catalog/snapshot?sessions=none", "")
	var snapshot catalogSnapshot
	decodeCatalogData(t, snapshotResponse, &snapshot)
	if snapshot.SessionQueryVersion != 1 || len(snapshot.Sessions) != 0 || len(snapshot.Workspaces) != 1 {
		t.Fatalf("bad structure-only snapshot: %+v", snapshot)
	}
	for _, query := range []string{"limit=0", "limit=201", "limit=no", "cursor=bad", "channel=unknown", "since=yesterday", "since=2500-01-01T00:00:00Z", "unread=perhaps"} {
		response := requestCatalog(t, handler, http.MethodGet, "/catalog/sessions?page=true&"+query, "")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d", query, response.Code)
		}
	}
}
