package zotigod

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/jayyao97/zotigo/core/session"
)

func TestSessionSearchAndBoundedWindow(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sess, err := session.NewManagerWithStore(store).CreateNew(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var items []session.DisplayItem
	for n := 0; n < 500; n++ {
		text := fmt.Sprintf("message %d", n)
		if n == 40 {
			text = "unique old needle"
		}
		items = append(items, session.DisplayItem{Type: session.DisplayItemAssistantMessage, Content: []session.DisplayContentPart{{Type: "text", Text: text}}})
	}
	items, err = store.AppendDisplayItems(ctx, sess.ID, items)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.RebuildDisplayIndex(ctx); err != nil {
		t.Fatal(err)
	}
	h := handler{store: store}
	w := httptest.NewRecorder()
	h.handleSessionSearch(w, httptest.NewRequest("GET", "/?q=old+needle", nil), sess.ID)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var envelope struct {
		Data session.DisplaySearchResult `json:"data"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	result := envelope.Data
	if len(result.Hits) != 1 || result.Hits[0].ID != items[40].ID {
		t.Fatal(result)
	}
	for _, query := range []string{fmt.Sprintf("sequence=%d", items[40].Sequence), "message_id=" + items[40].ID} {
		w = httptest.NewRecorder()
		h.handleSessionItemWindow(w, httptest.NewRequest("GET", "/?"+query, nil), sess.ID)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var window struct {
			Data itemsResponse `json:"data"`
		}
		if err = json.Unmarshal(w.Body.Bytes(), &window); err != nil {
			t.Fatal(err)
		}
		page := window.Data
		if len(page.Items) != 21 || page.Items[10].ID != items[40].ID {
			t.Fatalf("unbounded or wrong window: %+v", page)
		}
	}
	w = httptest.NewRecorder()
	h.handleSessionSearch(w, httptest.NewRequest("GET", "/?q=", nil), sess.ID)
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	if err = store.InvalidateDisplayIndex(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	h.handleSessionSearch(w, httptest.NewRequest("GET", "/?q=needle", nil), sess.ID)
	if w.Code != 503 || w.Header().Get("Retry-After") != "2" {
		t.Fatal(w.Code, w.Body.String())
	}
}
