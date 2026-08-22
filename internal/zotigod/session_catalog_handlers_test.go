package zotigod

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
)

func TestAssignedSessionOrganizationAndAvailability(t *testing.T) {
	handler, registry, catalog, workspace := newCatalogSessionFixture(t)
	writeTestProfileConfig(t, workspace.RootPath)

	create := requestCatalog(t, handler, http.MethodPost, "/sessions", `{"workspace_id":`+quotedJSON(t, workspace.ID)+`}`)
	if create.Code != http.StatusCreated {
		t.Fatalf("create assigned session status = %d: %s", create.Code, create.Body.String())
	}
	var session Session
	decodeCatalogData(t, create, &session)
	if session.WorkingDirectory != workspace.RootPath {
		t.Fatalf("session cwd = %q, want %q", session.WorkingDirectory, workspace.RootPath)
	}
	organization, err := catalog.GetSessionOrganization(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if organization.WorkspaceID == nil || *organization.WorkspaceID != workspace.ID {
		t.Fatalf("organization = %+v", organization)
	}

	conflict := requestCatalog(t, handler, http.MethodPost, "/sessions",
		`{"workspace_id":`+quotedJSON(t, workspace.ID)+`,"working_directory":`+quotedJSON(t, filepath.Dir(workspace.RootPath))+`}`)
	if conflict.Code != http.StatusBadRequest {
		t.Fatalf("conflicting cwd status = %d: %s", conflict.Code, conflict.Body.String())
	}

	title := requestCatalog(t, handler, http.MethodPut, "/sessions/"+session.ID+"/title", `{"title":"Catalog work"}`)
	if title.Code != http.StatusOK {
		t.Fatalf("set title status = %d: %s", title.Code, title.Body.String())
	}
	list := requestCatalog(t, handler, http.MethodGet, "/catalog/sessions?workspace_id="+workspace.ID, "")
	if list.Code != http.StatusOK {
		t.Fatalf("catalog session list status = %d: %s", list.Code, list.Body.String())
	}
	var listed struct {
		Sessions []sessionProjection `json:"sessions"`
	}
	decodeCatalogData(t, list, &listed)
	if len(listed.Sessions) != 1 || listed.Sessions[0].Availability != "ready" || listed.Sessions[0].Organization.Title == nil || *listed.Sessions[0].Organization.Title != "Catalog work" {
		t.Fatalf("catalog sessions = %+v", listed.Sessions)
	}
	for _, path := range []string{
		"/catalog/sessions/" + session.ID + "/",
		"/projects/" + workspace.ProjectID + "/",
		"/workspaces/" + workspace.ID + "/",
	} {
		response := requestCatalog(t, handler, http.MethodGet, path, "")
		if response.Code != http.StatusOK {
			t.Fatalf("trailing-slash route %q status = %d: %s", path, response.Code, response.Body.String())
		}
	}

	if _, err := registry.Start(session.ID); err != nil {
		t.Fatal(err)
	}
	archiveBlocked := requestCatalog(t, handler, http.MethodPost, "/workspaces/"+workspace.ID+"/archive", "")
	if archiveBlocked.Code != http.StatusConflict {
		t.Fatalf("active workspace archive status = %d: %s", archiveBlocked.Code, archiveBlocked.Body.String())
	}
	projectArchiveBlocked := requestCatalog(t, handler, http.MethodPost, "/projects/"+workspace.ProjectID+"/archive", "")
	if projectArchiveBlocked.Code != http.StatusConflict {
		t.Fatalf("active project archive status = %d: %s", projectArchiveBlocked.Code, projectArchiveBlocked.Body.String())
	}
}

func TestAssignedSessionCreationUsesWorkspaceLifecycleLock(t *testing.T) {
	workspaceOps := newSessionOperationLocks()
	handler, _, _, workspace := newCatalogSessionFixtureWithWorkspaceOps(t, workspaceOps)
	writeTestProfileConfig(t, workspace.RootPath)
	unlock := workspaceOps.lock(workspace.ID)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- requestCatalog(t, handler, http.MethodPost, "/sessions", `{"workspace_id":`+quotedJSON(t, workspace.ID)+`}`)
	}()
	select {
	case response := <-done:
		unlock()
		t.Fatalf("assigned creation bypassed workspace lock: %d", response.Code)
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	response := <-done
	if response.Code != http.StatusCreated {
		t.Fatalf("assigned creation status = %d: %s", response.Code, response.Body.String())
	}
}

func TestAssignedSessionCreationUsesProjectLifecycleLock(t *testing.T) {
	workspaceOps := newSessionOperationLocks()
	handler, _, _, workspace := newCatalogSessionFixtureWithWorkspaceOps(t, workspaceOps)
	writeTestProfileConfig(t, workspace.RootPath)
	unlock := workspaceOps.lock(projectOperationLockKey(workspace.ProjectID))
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- requestCatalog(t, handler, http.MethodPost, "/sessions", `{"workspace_id":`+quotedJSON(t, workspace.ID)+`}`)
	}()
	select {
	case response := <-done:
		unlock()
		t.Fatalf("assigned creation bypassed project lock: %d", response.Code)
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	response := <-done
	if response.Code != http.StatusCreated {
		t.Fatalf("assigned creation status = %d: %s", response.Code, response.Body.String())
	}
}

func TestArchivedWorkspaceBlocksSessionActivation(t *testing.T) {
	handler, _, catalog, workspace := newCatalogSessionFixture(t)
	writeTestProfileConfig(t, workspace.RootPath)
	create := requestCatalog(t, handler, http.MethodPost, "/sessions", `{"workspace_id":`+quotedJSON(t, workspace.ID)+`}`)
	var session Session
	decodeCatalogData(t, create, &session)

	if _, err := catalog.ArchiveWorkspace(context.Background(), workspace.ID); err != nil {
		t.Fatal(err)
	}
	start := requestCatalog(t, handler, http.MethodPost, "/sessions/"+session.ID+"/start", "")
	if start.Code != http.StatusConflict {
		t.Fatalf("archived session start status = %d: %s", start.Code, start.Body.String())
	}
}

func TestProjectDeletePreservesRuntimeSessionAndRemovesOrganization(t *testing.T) {
	handler, registry, catalog, workspace := newCatalogSessionFixture(t)
	writeTestProfileConfig(t, workspace.RootPath)
	create := requestCatalog(t, handler, http.MethodPost, "/sessions", `{"workspace_id":`+quotedJSON(t, workspace.ID)+`}`)
	if create.Code != http.StatusCreated {
		t.Fatalf("create assigned session status = %d: %s", create.Code, create.Body.String())
	}
	var session Session
	decodeCatalogData(t, create, &session)
	deleted := requestCatalog(t, handler, http.MethodPost, "/projects/"+workspace.ProjectID+"/delete", `{"confirmation":"Catalog"}`)
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete project status = %d: %s", deleted.Code, deleted.Body.String())
	}
	if _, ok := registry.Get(session.ID); !ok {
		t.Fatal("runtime session was removed")
	}
	if _, err := catalog.GetSessionOrganization(context.Background(), session.ID); !errors.Is(err, zotigoworkspace.ErrNotFound) {
		t.Fatalf("session organization error = %v, want not found", err)
	}
}

func newCatalogSessionFixture(t *testing.T) (http.Handler, *sessionRegistry, *zotigoworkspace.Store, zotigoworkspace.Workspace) {
	return newCatalogSessionFixtureWithWorkspaceOps(t, newSessionOperationLocks())
}

func newCatalogSessionFixtureWithWorkspaceOps(t *testing.T, workspaceOps *sessionOperationLocks) (http.Handler, *sessionRegistry, *zotigoworkspace.Store, zotigoworkspace.Workspace) {
	t.Helper()
	root := t.TempDir()
	store, err := zotigosession.NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	catalog, err := zotigoworkspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = catalog.Close() })
	project, err := catalog.CreateProject(context.Background(), "Catalog")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := catalog.CreateWorkspacePlan(context.Background(), project.ID, "Workspace", nil)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err = catalog.ProvisionWorkspace(context.Background(), workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	registry := newSessionRegistry()
	handler := newHandler(registry, storedDisplayItemSource{store: store}, handlerOptions{store: store, catalog: catalog, workspaceOps: workspaceOps})
	return handler, registry, catalog, workspace
}
