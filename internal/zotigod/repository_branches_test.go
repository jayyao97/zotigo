package zotigod

import (
	"context"
	"net/http"
	"testing"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
)

func TestSourceBranchesHTTP(t *testing.T) {
	catalog, err := zotigoworkspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = catalog.Close() })
	handler := newHandler(newSessionRegistry(), &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}, handlerOptions{catalog: catalog})
	project, err := catalog.CreateProject(context.Background(), "Branches")
	if err != nil {
		t.Fatal(err)
	}
	local := t.TempDir()
	runCatalogGit(t, local, "init", "-b", "trunk")
	runCatalogGit(t, local, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "initial")
	added := requestCatalog(t, handler, http.MethodPost, "/projects/"+project.ID+"/sources", `{"path":`+quotedJSON(t, local)+`}`)
	if added.Code != http.StatusCreated {
		t.Fatalf("add=%d %s", added.Code, added.Body.String())
	}
	var source zotigoworkspace.Source
	decodeCatalogData(t, added, &source)
	path := "/projects/" + project.ID + "/sources/" + source.ID + "/branches"
	response := requestCatalog(t, handler, http.MethodGet, path, "")
	if response.Code != http.StatusOK {
		t.Fatalf("branches=%d %s", response.Code, response.Body.String())
	}
	var data struct {
		Branches []zotigoworkspace.RepositoryBranch `json:"branches"`
	}
	decodeCatalogData(t, response, &data)
	if len(data.Branches) != 1 || data.Branches[0].Ref != "refs/heads/trunk" || data.Branches[0].Kind != "local" {
		t.Fatalf("branches=%+v", data.Branches)
	}
	if response := requestCatalog(t, handler, http.MethodPost, path, "{}"); response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post=%d", response.Code)
	}
	if response := requestCatalog(t, handler, http.MethodGet, "/projects/missing/sources/"+source.ID+"/branches", ""); response.Code != http.StatusNotFound {
		t.Fatalf("cross-project=%d", response.Code)
	}
}
