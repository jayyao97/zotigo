package zotigod

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
)

func TestRepositoryCatalogHTTP(t *testing.T) {
	catalog, err := zotigoworkspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = catalog.Close() })
	handler := newHandler(newSessionRegistry(), &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}, handlerOptions{catalog: catalog})
	remote := t.TempDir()
	runCatalogGit(t, remote, "init", "-b", "trunk")
	runCatalogGit(t, remote, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "initial")
	created := requestCatalog(t, handler, http.MethodPost, "/repositories", `{"remote_url":`+quotedJSON(t, remote)+`}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	var repo zotigoworkspace.Repository
	decodeCatalogData(t, created, &repo)
	if repo.LastSyncedAt != nil {
		t.Fatalf("repo=%+v", repo)
	}
	catalog.StartRepositoryMaintenance(context.Background(), nil)
	deadline := time.Now().Add(10 * time.Second)
	for repo.LastSyncedAt == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		repo, err = catalog.GetRepository(context.Background(), repo.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if repo.DefaultBranch != "trunk" || repo.LastSyncedAt == nil {
		t.Fatalf("repository preparation did not complete: %+v", repo)
	}
	var list struct {
		Repositories []zotigoworkspace.Repository `json:"repositories"`
	}
	decodeCatalogData(t, requestCatalog(t, handler, http.MethodGet, "/repositories", ""), &list)
	if len(list.Repositories) != 1 || list.Repositories[0].ID != repo.ID {
		t.Fatalf("list=%+v", list)
	}
	var project zotigoworkspace.Project
	decodeCatalogData(t, requestCatalog(t, handler, http.MethodPost, "/projects", `{"name":"Managed"}`), &project)
	attached := requestCatalog(t, handler, http.MethodPost, "/projects/"+project.ID+"/sources", `{"repository_id":`+quotedJSON(t, repo.ID)+`}`)
	if attached.Code != http.StatusCreated {
		t.Fatalf("attach=%d %s", attached.Code, attached.Body.String())
	}
	var source zotigoworkspace.Source
	decodeCatalogData(t, attached, &source)
	if source.RepositoryID != repo.ID || source.DefaultBranch != "trunk" {
		t.Fatalf("source=%+v", source)
	}
	createdWorkspace := requestCatalog(t, handler, http.MethodPost, "/projects/"+project.ID+"/workspaces", `{"title":"Automatic","sources":[{"source_id":`+quotedJSON(t, source.ID)+`}]}`)
	if createdWorkspace.Code != http.StatusCreated {
		t.Fatalf("workspace=%d %s", createdWorkspace.Code, createdWorkspace.Body.String())
	}
	var workspace zotigoworkspace.Workspace
	decodeCatalogData(t, createdWorkspace, &workspace)
	if workspace.Status != zotigoworkspace.WorkspaceStatusReady {
		t.Fatalf("workspace=%+v", workspace)
	}
	mixed := requestCatalog(t, handler, http.MethodPost, "/projects/"+project.ID+"/sources", `{"repository_id":`+quotedJSON(t, repo.ID)+`,"path":`+quotedJSON(t, remote)+`}`)
	if mixed.Code != http.StatusBadRequest {
		t.Fatalf("mixed source=%d", mixed.Code)
	}
	credential := requestCatalog(t, handler, http.MethodPost, "/repositories", `{"remote_url":"https://secret@example.com/repo.git"}`)
	if credential.Code != http.StatusBadRequest {
		t.Fatalf("credential URL=%d", credential.Code)
	}
	if wrong := requestCatalog(t, handler, http.MethodDelete, "/repositories", ""); wrong.Code != http.StatusMethodNotAllowed {
		t.Fatalf("wrong method=%d", wrong.Code)
	}
}

func TestRepositoryCatalogRequiresAuthentication(t *testing.T) {
	catalog, err := zotigoworkspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = catalog.Close() })
	handler := newHandler(newSessionRegistry(), &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}, handlerOptions{catalog: catalog, publicAuthToken: "secret"})
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(method, "/repositories", nil))
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s unauthenticated=%d", method, recorder.Code)
		}
	}
}
