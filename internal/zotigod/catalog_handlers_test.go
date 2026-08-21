package zotigod

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
)

func TestCatalogProjectSourceAndWorkspaceRoutes(t *testing.T) {
	root := t.TempDir()
	catalog, err := zotigoworkspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = catalog.Close() })
	handler := newHandler(
		newSessionRegistry(),
		&fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}},
		handlerOptions{catalog: catalog},
	)

	projectRec := requestCatalog(t, handler, http.MethodPost, "/projects", `{"name":"Zotigo"}`)
	if projectRec.Code != http.StatusCreated {
		t.Fatalf("create project status = %d: %s", projectRec.Code, projectRec.Body.String())
	}
	var project zotigoworkspace.Project
	decodeCatalogData(t, projectRec, &project)
	renameRec := requestCatalog(t, handler, http.MethodPut, "/projects/"+project.ID, `{"name":"  Renamed Project  "}`)
	if renameRec.Code != http.StatusOK {
		t.Fatalf("rename project status = %d: %s", renameRec.Code, renameRec.Body.String())
	}
	var renamed zotigoworkspace.Project
	decodeCatalogData(t, renameRec, &renamed)
	if renamed.ID != project.ID || renamed.Name != "Renamed Project" {
		t.Fatalf("renamed project = %+v", renamed)
	}
	invalidRename := requestCatalog(t, handler, http.MethodPut, "/projects/"+project.ID, `{"name":" "}`)
	if invalidRename.Code != http.StatusBadRequest {
		t.Fatalf("invalid rename status = %d: %s", invalidRename.Code, invalidRename.Body.String())
	}
	missingRename := requestCatalog(t, handler, http.MethodPut, "/projects/missing", `{"name":"Renamed"}`)
	if missingRename.Code != http.StatusNotFound {
		t.Fatalf("missing rename status = %d: %s", missingRename.Code, missingRename.Body.String())
	}

	folder := filepath.Join(root, "plain-source")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	inspectRec := requestCatalog(t, handler, http.MethodPost,
		"/projects/"+project.ID+"/sources/inspect", `{"path":`+quotedJSON(t, folder)+`}`)
	if inspectRec.Code != http.StatusOK {
		t.Fatalf("inspect source status = %d: %s", inspectRec.Code, inspectRec.Body.String())
	}
	var inspection zotigoworkspace.SourceInspection
	decodeCatalogData(t, inspectRec, &inspection)
	if inspection.Kind != zotigoworkspace.SourceKindFolder {
		t.Fatalf("inspection = %+v", inspection)
	}

	sourceRec := requestCatalog(t, handler, http.MethodPost,
		"/projects/"+project.ID+"/sources",
		`{"path":`+quotedJSON(t, folder)+`,"folder_mode":"reference"}`)
	if sourceRec.Code != http.StatusCreated {
		t.Fatalf("create source status = %d: %s", sourceRec.Code, sourceRec.Body.String())
	}
	var source zotigoworkspace.Source
	decodeCatalogData(t, sourceRec, &source)
	if source.ProjectID != project.ID || source.FolderMode != zotigoworkspace.FolderModeReference {
		t.Fatalf("source = %+v", source)
	}

	workspaceRec := requestCatalog(t, handler, http.MethodPost,
		"/projects/"+project.ID+"/workspaces",
		`{"title":"Catalog migration","sources":[{"source_id":`+quotedJSON(t, source.ID)+`,"mode":"reference"}]}`)
	if workspaceRec.Code != http.StatusCreated {
		t.Fatalf("create workspace status = %d: %s", workspaceRec.Code, workspaceRec.Body.String())
	}
	var workspace zotigoworkspace.Workspace
	decodeCatalogData(t, workspaceRec, &workspace)
	if workspace.ProjectID != project.ID || workspace.Status != zotigoworkspace.WorkspaceStatusReady {
		t.Fatalf("workspace = %+v", workspace)
	}
	for _, directory := range []string{"code", "artifacts", "notes"} {
		if info, err := os.Stat(filepath.Join(workspace.RootPath, directory)); err != nil || !info.IsDir() {
			t.Fatalf("workspace %s directory: %v", directory, err)
		}
	}
	if _, err := os.Stat(filepath.Join(workspace.RootPath, "notes", source.SourceKey, "content.txt")); err == nil || !os.IsNotExist(err) {
		// The source is intentionally empty; only its binding marker should exist.
		t.Fatalf("unexpected copied content: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace.RootPath, "notes", source.SourceKey, ".zotigo-binding.json")); err != nil {
		t.Fatalf("binding marker: %v", err)
	}

	detailRec := requestCatalog(t, handler, http.MethodGet, "/projects/"+project.ID, "")
	if detailRec.Code != http.StatusOK {
		t.Fatalf("project detail status = %d: %s", detailRec.Code, detailRec.Body.String())
	}
	var detail projectDetail
	decodeCatalogData(t, detailRec, &detail)
	if detail.Name != "Renamed Project" || len(detail.Sources) != 1 || detail.Sources[0].ID != source.ID {
		t.Fatalf("project detail = %+v", detail)
	}

	workspaceDetailRec := requestCatalog(t, handler, http.MethodGet, "/workspaces/"+workspace.ID, "")
	if workspaceDetailRec.Code != http.StatusOK {
		t.Fatalf("workspace detail status = %d: %s", workspaceDetailRec.Code, workspaceDetailRec.Body.String())
	}

	archivePreview := requestCatalog(t, handler, http.MethodGet, "/projects/"+project.ID+"/archive-preview", "")
	if archivePreview.Code != http.StatusOK {
		t.Fatalf("project archive preview status = %d: %s", archivePreview.Code, archivePreview.Body.String())
	}
	archiveProject := requestCatalog(t, handler, http.MethodPost, "/projects/"+project.ID+"/archive", "")
	if archiveProject.Code != http.StatusOK {
		t.Fatalf("project archive status = %d: %s", archiveProject.Code, archiveProject.Body.String())
	}
	activeProjects := requestCatalog(t, handler, http.MethodGet, "/projects", "")
	var activeList struct {
		Projects []zotigoworkspace.Project `json:"projects"`
	}
	decodeCatalogData(t, activeProjects, &activeList)
	if len(activeList.Projects) != 0 {
		t.Fatalf("active projects = %+v", activeList.Projects)
	}
	activeProjectRec := requestCatalog(t, handler, http.MethodPost, "/projects", `{"name":"Active Project"}`)
	if activeProjectRec.Code != http.StatusCreated {
		t.Fatalf("create active project status = %d: %s", activeProjectRec.Code, activeProjectRec.Body.String())
	}
	var activeProject zotigoworkspace.Project
	decodeCatalogData(t, activeProjectRec, &activeProject)
	activeProjects = requestCatalog(t, handler, http.MethodGet, "/projects?status=active", "")
	decodeCatalogData(t, activeProjects, &activeList)
	if len(activeList.Projects) != 1 || activeList.Projects[0].ID != activeProject.ID {
		t.Fatalf("filtered active projects = %+v", activeList.Projects)
	}
	archivedProjects := requestCatalog(t, handler, http.MethodGet, "/projects?status=archived", "")
	var archivedList struct {
		Projects []zotigoworkspace.Project `json:"projects"`
	}
	decodeCatalogData(t, archivedProjects, &archivedList)
	if len(archivedList.Projects) != 1 || archivedList.Projects[0].ID != project.ID {
		t.Fatalf("archived projects = %+v", archivedList.Projects)
	}
	allProjects := requestCatalog(t, handler, http.MethodGet, "/projects?status=all", "")
	var allList struct {
		Projects []zotigoworkspace.Project `json:"projects"`
	}
	decodeCatalogData(t, allProjects, &allList)
	if len(allList.Projects) != 2 {
		t.Fatalf("all projects = %+v", allList.Projects)
	}
	invalidStatus := requestCatalog(t, handler, http.MethodGet, "/projects?status=deleting", "")
	if invalidStatus.Code != http.StatusBadRequest {
		t.Fatalf("invalid project status filter = %d: %s", invalidStatus.Code, invalidStatus.Body.String())
	}
	deprecatedFilter := requestCatalog(t, handler, http.MethodGet, "/projects?include_archived=true", "")
	if deprecatedFilter.Code != http.StatusBadRequest {
		t.Fatalf("deprecated project filter = %d: %s", deprecatedFilter.Code, deprecatedFilter.Body.String())
	}
	blockedWorkspaceRestore := requestCatalog(t, handler, http.MethodPost, "/workspaces/"+workspace.ID+"/unarchive", "")
	if blockedWorkspaceRestore.Code != http.StatusConflict {
		t.Fatalf("workspace restore under archived project = %d: %s", blockedWorkspaceRestore.Code, blockedWorkspaceRestore.Body.String())
	}
	restoreProject := requestCatalog(t, handler, http.MethodPost, "/projects/"+project.ID+"/unarchive", "")
	if restoreProject.Code != http.StatusOK {
		t.Fatalf("project unarchive status = %d: %s", restoreProject.Code, restoreProject.Body.String())
	}
	restoreWorkspace := requestCatalog(t, handler, http.MethodPost, "/workspaces/"+workspace.ID+"/unarchive", "")
	if restoreWorkspace.Code != http.StatusOK {
		t.Fatalf("workspace unarchive status = %d: %s", restoreWorkspace.Code, restoreWorkspace.Body.String())
	}
	deletePreview := requestCatalog(t, handler, http.MethodGet, "/projects/"+project.ID+"/delete-preview", "")
	if deletePreview.Code != http.StatusOK {
		t.Fatalf("project delete preview status = %d: %s", deletePreview.Code, deletePreview.Body.String())
	}
	wrongDelete := requestCatalog(t, handler, http.MethodPost, "/projects/"+project.ID+"/delete", `{"confirmation":"wrong"}`)
	if wrongDelete.Code != http.StatusBadRequest {
		t.Fatalf("project delete confirmation status = %d: %s", wrongDelete.Code, wrongDelete.Body.String())
	}
	deleteProject := requestCatalog(t, handler, http.MethodPost, "/projects/"+project.ID+"/delete", `{"confirmation":"Renamed Project"}`)
	if deleteProject.Code != http.StatusOK {
		t.Fatalf("project delete status = %d: %s", deleteProject.Code, deleteProject.Body.String())
	}
	deletedDetail := requestCatalog(t, handler, http.MethodGet, "/projects/"+project.ID, "")
	if deletedDetail.Code != http.StatusNotFound {
		t.Fatalf("deleted project detail status = %d: %s", deletedDetail.Code, deletedDetail.Body.String())
	}
	if _, err := os.Stat(folder); err != nil {
		t.Fatalf("external source directory was removed: %v", err)
	}
}

func TestSourceInspectionDoesNotRequireProject(t *testing.T) {
	handler := newHandler(
		newSessionRegistry(),
		&fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}},
		handlerOptions{},
	)
	folder := t.TempDir()

	recorder := requestCatalog(t, handler, http.MethodPost, "/sources/inspect", `{"path":`+quotedJSON(t, folder)+`}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("inspect source status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var inspection zotigoworkspace.SourceInspection
	decodeCatalogData(t, recorder, &inspection)
	canonicalFolder, err := filepath.EvalSymlinks(folder)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Kind != zotigoworkspace.SourceKindFolder || inspection.CanonicalPath != canonicalFolder {
		t.Fatalf("inspection = %+v", inspection)
	}

	wrongMethod := requestCatalog(t, handler, http.MethodGet, "/sources/inspect", "")
	if wrongMethod.Code != http.StatusMethodNotAllowed || wrongMethod.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("wrong method response = %d allow=%q", wrongMethod.Code, wrongMethod.Header().Get("Allow"))
	}
}

func TestCatalogCreatesGitWorkspaceWithoutClientSideCommitResolution(t *testing.T) {
	root := t.TempDir()
	catalog, err := zotigoworkspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = catalog.Close() })
	handler := newHandler(newSessionRegistry(), &fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}}, handlerOptions{catalog: catalog})
	projectRec := requestCatalog(t, handler, http.MethodPost, "/projects", `{"name":"Git"}`)
	var project zotigoworkspace.Project
	decodeCatalogData(t, projectRec, &project)

	repository := t.TempDir()
	runCatalogGit(t, repository, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repository, "README.md"), []byte("source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCatalogGit(t, repository, "add", "README.md")
	runCatalogGit(t, repository, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
	sourceRec := requestCatalog(t, handler, http.MethodPost, "/projects/"+project.ID+"/sources", `{"path":`+quotedJSON(t, repository)+`}`)
	if sourceRec.Code != http.StatusCreated {
		t.Fatalf("create Git source status = %d: %s", sourceRec.Code, sourceRec.Body.String())
	}
	var source zotigoworkspace.Source
	decodeCatalogData(t, sourceRec, &source)
	workspaceRec := requestCatalog(t, handler, http.MethodPost, "/projects/"+project.ID+"/workspaces",
		`{"title":"Git workspace","sources":[{"source_id":`+quotedJSON(t, source.ID)+`}]}`)
	if workspaceRec.Code != http.StatusCreated {
		t.Fatalf("create Git workspace status = %d: %s", workspaceRec.Code, workspaceRec.Body.String())
	}
	var workspace zotigoworkspace.Workspace
	decodeCatalogData(t, workspaceRec, &workspace)
	if _, err := os.Stat(filepath.Join(workspace.RootPath, "code", source.SourceKey, "README.md")); err != nil {
		t.Fatalf("Git workspace checkout: %v", err)
	}
}

func runCatalogGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}

func TestCatalogRoutesUsePublicAuthentication(t *testing.T) {
	catalog, err := zotigoworkspace.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = catalog.Close() })
	handler := newHandler(
		newSessionRegistry(),
		&fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}},
		handlerOptions{catalog: catalog, publicAuthToken: "secret"},
	)

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/projects", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}

	authorized := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/projects", nil)
	request.Header.Set("Authorization", "Bearer secret")
	handler.ServeHTTP(authorized, request)
	if authorized.Code != http.StatusOK {
		t.Fatalf("authorized status = %d: %s", authorized.Code, authorized.Body.String())
	}
}

func TestCatalogProjectStatusAllIncludesDeletingProject(t *testing.T) {
	root := t.TempDir()
	catalog, err := zotigoworkspace.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = catalog.Close() })
	handler := newHandler(
		newSessionRegistry(),
		&fakeDisplayItemSource{items: map[string][]zotigosession.DisplayItem{}},
		handlerOptions{catalog: catalog},
	)
	projectRec := requestCatalog(t, handler, http.MethodPost, "/projects", `{"name":"Deleting Project"}`)
	if projectRec.Code != http.StatusCreated {
		t.Fatalf("create project status = %d: %s", projectRec.Code, projectRec.Body.String())
	}
	var project zotigoworkspace.Project
	decodeCatalogData(t, projectRec, &project)
	projectDir := filepath.Join(root, "projects", project.ID)
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "unknown.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	deleteRec := requestCatalog(t, handler, http.MethodPost, "/projects/"+project.ID+"/delete", `{"confirmation":"Deleting Project"}`)
	if deleteRec.Code != http.StatusConflict {
		t.Fatalf("partial delete status = %d: %s", deleteRec.Code, deleteRec.Body.String())
	}

	for _, path := range []string{"/projects", "/projects?status=archived"} {
		recorder := requestCatalog(t, handler, http.MethodGet, path, "")
		var response struct {
			Projects []zotigoworkspace.Project `json:"projects"`
		}
		decodeCatalogData(t, recorder, &response)
		if len(response.Projects) != 0 {
			t.Fatalf("%s projects = %+v, want none", path, response.Projects)
		}
	}
	allRec := requestCatalog(t, handler, http.MethodGet, "/projects?status=all", "")
	var all struct {
		Projects []zotigoworkspace.Project `json:"projects"`
	}
	decodeCatalogData(t, allRec, &all)
	if len(all.Projects) != 1 || all.Projects[0].ID != project.ID || all.Projects[0].Status != zotigoworkspace.ProjectStatusDeleting {
		t.Fatalf("all projects = %+v, want deleting project", all.Projects)
	}
}

func TestCatalogInternalErrorIsLoggedWithoutExposingDetails(t *testing.T) {
	var logs bytes.Buffer
	handler := &handler{logger: log.New(&logs, "", 0)}
	recorder := httptest.NewRecorder()
	handler.writeCatalogError(recorder, errors.New("database detail"))

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	if strings.Contains(recorder.Body.String(), "database detail") {
		t.Fatalf("response exposed internal error: %s", recorder.Body.String())
	}
	if !strings.Contains(logs.String(), "database detail") {
		t.Fatalf("log omitted internal error: %q", logs.String())
	}
}

func requestCatalog(t *testing.T, handler http.Handler, method string, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decodeCatalogData(t *testing.T, recorder *httptest.ResponseRecorder, target any) {
	t.Helper()
	var response struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if err := json.Unmarshal(response.Data, target); err != nil {
		t.Fatalf("decode response data: %v", err)
	}
}

func quotedJSON(t *testing.T, value string) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
