package zotigod

import (
	"encoding/json"
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
