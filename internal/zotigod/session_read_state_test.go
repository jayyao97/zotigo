package zotigod

import (
	"fmt"
	"net/http"
	"testing"

	zotigosession "github.com/jayyao97/zotigo/core/session"
)

func TestSessionReadStateAPISharedAcrossClients(t *testing.T) {
	store, err := zotigosession.NewFileStore(newTestStoreRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	sess, err := zotigosession.NewManagerWithStore(store).CreateNew(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := newHandler(newSessionRegistry(), storedDisplayItemSource{store: store}, handlerOptions{store: store})
	appendTurn := func() {
		t.Helper()
		if _, err := store.AppendDisplayItem(t.Context(), sess.ID, zotigosession.DisplayItem{Type: zotigosession.DisplayItemTurnCompleted}); err != nil {
			t.Fatal(err)
		}
	}
	read := func() zotigosession.ReadState {
		t.Helper()
		response := requestCatalog(t, server, http.MethodGet, "/sessions/"+sess.ID, "")
		if response.Code != http.StatusOK {
			t.Fatalf("get: %s", response.Body.String())
		}
		var result Session
		decodeCatalogData(t, response, &result)
		if result.ReadState == nil {
			t.Fatal("read state missing")
		}
		return *result.ReadState
	}
	put := func(body string) zotigosession.ReadState {
		t.Helper()
		response := requestCatalog(t, server, http.MethodPut, "/sessions/"+sess.ID+"/read-state", body)
		if response.Code != http.StatusOK {
			t.Fatalf("put: %s", response.Body.String())
		}
		var result zotigosession.ReadState
		decodeCatalogData(t, response, &result)
		return result
	}
	appendTurn()
	first := read()
	if !first.Unread {
		t.Fatal("completion not unread")
	}
	appendTurn()
	if !put(fmt.Sprintf(`{"read_version":%d}`, first.Version)).Unread {
		t.Fatal("old client cleared new completion")
	}
	latest := read()
	if put(fmt.Sprintf(`{"read_version":%d}`, latest.Version)).Unread || read().Unread {
		t.Fatal("read not shared")
	}
	if !put(`{"unread":true}`).Unread || !read().Unread {
		t.Fatal("manual unread not shared")
	}
	listed := requestCatalog(t, server, http.MethodGet, "/sessions", "")
	var list struct {
		Sessions []Session `json:"sessions"`
	}
	decodeCatalogData(t, listed, &list)
	if len(list.Sessions) != 1 || list.Sessions[0].ReadState == nil || !list.Sessions[0].ReadState.Unread {
		t.Fatalf("list missing unread: %+v", list)
	}
	for _, body := range []string{`{}`, `{"read_version":-1}`, `{"read_version":1,"unread":true}`, `{"read_version":1.5}`} {
		if response := requestCatalog(t, server, http.MethodPut, "/sessions/"+sess.ID+"/read-state", body); response.Code != http.StatusBadRequest {
			t.Fatalf("invalid request %s = %d", body, response.Code)
		}
	}
	if response := requestCatalog(t, server, http.MethodPut, "/sessions/missing/read-state", `{"unread":true}`); response.Code != http.StatusNotFound {
		t.Fatalf("missing session = %d", response.Code)
	}
}
