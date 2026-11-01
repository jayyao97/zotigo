package zotigod

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
)

func TestDirectorySessionOwnershipFollowsContainingWorkspace(t *testing.T) {
	handler, _, catalog, workspace := newCatalogSessionFixture(t)
	backend := filepath.Join(workspace.RootPath, "code", "backend")
	if err := os.MkdirAll(backend, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	for _, dir := range []string{workspace.RootPath, backend, outside} {
		writeTestProfileConfig(t, dir)
	}

	create := func(t *testing.T, body string) Session {
		t.Helper()
		response := requestCatalog(t, handler, http.MethodPost, "/sessions", body)
		if response.Code != http.StatusCreated {
			t.Fatalf("create %s = %d: %s", body, response.Code, response.Body.String())
		}
		var session Session
		decodeCatalogData(t, response, &session)
		return session
	}
	availability := func(t *testing.T, id string) sessionProjection {
		t.Helper()
		response := requestCatalog(t, handler, http.MethodGet, "/catalog/sessions/"+id, "")
		if response.Code != http.StatusOK {
			t.Fatalf("projection = %d: %s", response.Code, response.Body.String())
		}
		var projection sessionProjection
		decodeCatalogData(t, response, &projection)
		return projection
	}

	t.Run("workspace subdirectory binds and keeps cwd", func(t *testing.T) {
		session := create(t, `{"working_directory":`+quotedJSON(t, backend)+`}`)
		if session.WorkingDirectory != backend {
			t.Fatalf("cwd = %q, want %q", session.WorkingDirectory, backend)
		}
		projection := availability(t, session.ID)
		if projection.Organization == nil || projection.Organization.WorkspaceID == nil || *projection.Organization.WorkspaceID != workspace.ID {
			t.Fatalf("organization = %+v", projection.Organization)
		}
		if projection.Organization.ProjectID == nil || *projection.Organization.ProjectID != workspace.ProjectID {
			t.Fatalf("project = %+v", projection.Organization)
		}
		if projection.Availability != "ready" {
			t.Fatalf("availability = %q", projection.Availability)
		}
	})

	t.Run("explicit workspace accepts subdirectory", func(t *testing.T) {
		session := create(t, `{"workspace_id":`+quotedJSON(t, workspace.ID)+`,"working_directory":`+quotedJSON(t, backend)+`}`)
		if session.WorkingDirectory != backend {
			t.Fatalf("cwd = %q", session.WorkingDirectory)
		}
	})

	t.Run("explicit workspace rejects outside directory", func(t *testing.T) {
		response := requestCatalog(t, handler, http.MethodPost, "/sessions", `{"workspace_id":`+quotedJSON(t, workspace.ID)+`,"working_directory":`+quotedJSON(t, outside)+`}`)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("status = %d: %s", response.Code, response.Body.String())
		}
	})

	t.Run("ordinary directory stays unowned", func(t *testing.T) {
		session := create(t, `{"working_directory":`+quotedJSON(t, outside)+`}`)
		if _, err := catalog.GetSessionOrganization(t.Context(), session.ID); !errors.Is(err, zotigoworkspace.ErrNotFound) {
			t.Fatalf("organization err = %v, want not found", err)
		}
		projection := availability(t, session.ID)
		if projection.Availability != "ready" || projection.Organization != nil {
			t.Fatalf("projection = %+v", projection)
		}
	})

	t.Run("archived owner is rejected instead of silently unowned", func(t *testing.T) {
		archived := requestCatalog(t, handler, http.MethodPost, "/workspaces/"+workspace.ID+"/archive", "")
		if archived.Code != http.StatusOK {
			t.Fatalf("archive = %d: %s", archived.Code, archived.Body.String())
		}
		current, err := catalog.GetWorkspace(t.Context(), workspace.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status == zotigoworkspace.WorkspaceStatusReady {
			t.Fatalf("workspace still ready after archive")
		}
		if _, err := os.Stat(backend); err != nil {
			t.Skipf("archived workspace directory no longer exists: %v", err)
		}
		response := requestCatalog(t, handler, http.MethodPost, "/sessions", `{"working_directory":`+quotedJSON(t, backend)+`}`)
		if response.Code != http.StatusConflict || !containsAPICode(response.Body.Bytes(), "workspace_not_ready") {
			t.Fatalf("status = %d: %s", response.Code, response.Body.String())
		}
	})
}

func containsAPICode(body []byte, code string) bool {
	var response apiErrorResponse
	return json.Unmarshal(body, &response) == nil && response.Code == code
}
