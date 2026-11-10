package zotigod

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
)

func TestCatalogSnapshotMatchesExistingCatalogReads(t *testing.T) {
	handler, _, _, workspace := newCatalogSessionFixture(t)
	writeTestProfileConfig(t, workspace.RootPath)
	created := requestCatalog(t, handler, http.MethodPost, "/sessions", `{"workspace_id":`+quotedJSON(t, workspace.ID)+`}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create session: %s", created.Body.String())
	}
	response := requestCatalog(t, handler, http.MethodGet, "/catalog/snapshot", "")
	if response.Code != http.StatusOK {
		t.Fatalf("snapshot: %s", response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing private response cache policy")
	}
	var snapshot catalogSnapshot
	decodeCatalogData(t, response, &snapshot)
	if len(snapshot.Projects) != 1 || len(snapshot.Workspaces) != 1 || len(snapshot.Sessions) != 1 {
		t.Fatalf("incomplete snapshot: %+v", snapshot)
	}
	var detail projectDetail
	decodeCatalogData(t, requestCatalog(t, handler, http.MethodGet, "/projects/"+workspace.ProjectID, ""), &detail)
	var workspaces struct {
		Workspaces []zotigoworkspace.Workspace `json:"workspaces"`
	}
	decodeCatalogData(t, requestCatalog(t, handler, http.MethodGet, "/projects/"+workspace.ProjectID+"/workspaces", ""), &workspaces)
	var navigation zotigoworkspace.Navigation
	decodeCatalogData(t, requestCatalog(t, handler, http.MethodGet, "/catalog/navigation", ""), &navigation)
	var sessions catalogSessionListResponse
	decodeCatalogData(t, requestCatalog(t, handler, http.MethodGet, "/catalog/sessions", ""), &sessions)
	if !reflect.DeepEqual(snapshot.Projects[0], detail) || !reflect.DeepEqual(snapshot.Workspaces, workspaces.Workspaces) || !reflect.DeepEqual(snapshot.Navigation, navigation) || !reflect.DeepEqual(snapshot.Sessions, sessions.Sessions) {
		t.Fatal("snapshot differs from original read endpoints")
	}
	if navigation.LegacyOrderImported {
		t.Fatal("read snapshot changed navigation migration state")
	}
	post := requestCatalog(t, handler, http.MethodPost, "/catalog/snapshot", "{}")
	if post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status: %d", post.Code)
	}
}

func TestCatalogSnapshotRequiresPublicAuthentication(t *testing.T) {
	h := newHandler(newSessionRegistry(), nil, handlerOptions{publicAuthToken: "secret"})
	response := httptest.NewRecorder()
	h.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/catalog/snapshot", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status: %d", response.Code)
	}
}
