package zotigod

import (
	"context"
	"errors"
	"fmt"
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
	organization, err = catalog.RecordSessionActivity(context.Background(), session.ID, time.Now().UTC().Add(time.Minute))
	if err != nil || organization.WorkspacePosition == nil || *organization.WorkspacePosition >= 0 {
		t.Fatalf("activity organization = %+v, err=%v", organization, err)
	}
	position := requestCatalog(t, handler, http.MethodPut, "/sessions/"+session.ID+"/position", fmt.Sprintf(`{"position":%d}`, *organization.WorkspacePosition+1))
	if position.Code != http.StatusOK {
		t.Fatalf("set signed position status = %d: %s", position.Code, position.Body.String())
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

func TestIdleRunningSessionCanBeArchivedAndRestarted(t *testing.T) {
	handler, registry, catalog, workspace := newCatalogSessionFixture(t)
	writeTestProfileConfig(t, workspace.RootPath)
	create := requestCatalog(t, handler, http.MethodPost, "/sessions", `{"workspace_id":`+quotedJSON(t, workspace.ID)+`}`)
	var session Session
	decodeCatalogData(t, create, &session)
	if _, err := registry.Start(session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.MarkRunning(session.ID); err != nil {
		t.Fatal(err)
	}

	archive := requestCatalog(t, handler, http.MethodPost, "/sessions/"+session.ID+"/archive", "")
	if archive.Code != http.StatusOK {
		t.Fatalf("idle session archive status = %d: %s", archive.Code, archive.Body.String())
	}
	if runtime, _ := registry.Get(session.ID); runtime.State != SessionStateCreated {
		t.Fatalf("released session state = %q, want %q", runtime.State, SessionStateCreated)
	}
	organization, err := catalog.GetSessionOrganization(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if organization.SelfArchivedAt == nil {
		t.Fatal("session was not archived")
	}

	unarchive := requestCatalog(t, handler, http.MethodPost, "/sessions/"+session.ID+"/unarchive", "")
	if unarchive.Code != http.StatusOK {
		t.Fatalf("session unarchive status = %d: %s", unarchive.Code, unarchive.Body.String())
	}
	if _, err := registry.Start(session.ID); err != nil {
		t.Fatalf("restart unarchived session: %v", err)
	}
}

func TestMessageAppendSerializesWithSessionArchive(t *testing.T) {
	source := &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}
	workers := newWorkerRegistry()
	handler, registry, _, workspace := newCatalogSessionFixtureWithRuntime(t, newSessionOperationLocks(), source, workers)
	writeTestProfileConfig(t, workspace.RootPath)
	create := requestCatalog(t, handler, http.MethodPost, "/sessions", `{"workspace_id":`+quotedJSON(t, workspace.ID)+`}`)
	var session Session
	decodeCatalogData(t, create, &session)
	if _, err := registry.Start(session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.MarkRunning(session.ID); err != nil {
		t.Fatal(err)
	}
	workers.workers[session.ID] = newWorkerConnection(session.ID, "generation-1", nil, workers)
	appendStarted := make(chan struct{})
	releaseAppend := make(chan struct{})
	source.appendHook = func(sessionID string, item zotigosession.DisplayItem) {
		if sessionID == session.ID && item.Command != nil && item.Command.Type == sessionCommandMessage {
			close(appendStarted)
			<-releaseAppend
		}
	}

	messageDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		messageDone <- requestCatalog(t, handler, http.MethodPost, "/sessions/"+session.ID+"/messages", `{"text":"pending"}`)
	}()
	<-appendStarted
	archiveDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		archiveDone <- requestCatalog(t, handler, http.MethodPost, "/sessions/"+session.ID+"/archive", "")
	}()
	select {
	case archive := <-archiveDone:
		close(releaseAppend)
		t.Fatalf("archive bypassed message append lock: %d: %s", archive.Code, archive.Body.String())
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseAppend)
	message := <-messageDone
	if message.Code != http.StatusCreated {
		t.Fatalf("message status = %d: %s", message.Code, message.Body.String())
	}
	archive := <-archiveDone
	if archive.Code != http.StatusConflict {
		t.Fatalf("pending-message archive status = %d: %s", archive.Code, archive.Body.String())
	}
}

func TestIdleSessionArchiveFailurePreservesRunningState(t *testing.T) {
	handler, registry, catalog, workspace := newCatalogSessionFixture(t)
	writeTestProfileConfig(t, workspace.RootPath)
	create := requestCatalog(t, handler, http.MethodPost, "/sessions", `{"workspace_id":`+quotedJSON(t, workspace.ID)+`}`)
	var session Session
	decodeCatalogData(t, create, &session)
	if _, err := registry.Start(session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.MarkRunning(session.ID); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Close(); err != nil {
		t.Fatal(err)
	}

	archive := requestCatalog(t, handler, http.MethodPost, "/sessions/"+session.ID+"/archive", "")
	if archive.Code != http.StatusInternalServerError {
		t.Fatalf("archive status = %d, want %d: %s", archive.Code, http.StatusInternalServerError, archive.Body.String())
	}
	if runtime, _ := registry.Get(session.ID); runtime.State != SessionStateRunning {
		t.Fatalf("failed archive changed session state to %q", runtime.State)
	}
}

func TestDetachedWorkerCannotUnregisterReplacement(t *testing.T) {
	workers := newWorkerRegistry()
	old := newWorkerConnection("session-1", "old", nil, workers)
	workers.workers[old.sessionID] = old

	detached := workers.Detach(old.sessionID)
	if detached != old || !old.closing {
		t.Fatalf("detached worker = %#v, old closing = %v", detached, old.closing)
	}
	replacement := newWorkerConnection(old.sessionID, "replacement", nil, workers)
	workers.workers[old.sessionID] = replacement
	workers.unregister(old.sessionID, old)
	if got := workers.workers[old.sessionID]; got != replacement {
		t.Fatalf("old worker unregister removed replacement: %#v", got)
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
	return newCatalogSessionFixtureWithRuntime(t, workspaceOps, nil, nil)
}

func newCatalogSessionFixtureWithRuntime(t *testing.T, workspaceOps *sessionOperationLocks, items displayItemSource, workers *workerRegistry) (http.Handler, *sessionRegistry, *zotigoworkspace.Store, zotigoworkspace.Workspace) {
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
	if items == nil {
		items = storedDisplayItemSource{store: store}
	}
	handler := newHandler(registry, items, handlerOptions{store: store, catalog: catalog, workspaceOps: workspaceOps, workers: workers})
	return handler, registry, catalog, workspace
}
