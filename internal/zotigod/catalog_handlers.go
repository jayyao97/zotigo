package zotigod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
)

type createProjectRequest struct {
	Name string `json:"name"`
}

type createSourceRequest struct {
	Path       string                     `json:"path"`
	FolderMode zotigoworkspace.FolderMode `json:"folder_mode,omitempty"`
}

type inspectSourceRequest struct {
	Path string `json:"path"`
}

type createWorkspaceRequest struct {
	Title   string                                 `json:"title"`
	Sources []zotigoworkspace.WorkspaceSourceInput `json:"sources,omitempty"`
}

type deleteCatalogRequest struct {
	Confirmation string `json:"confirmation"`
}

type projectDetail struct {
	zotigoworkspace.Project
	Sources []zotigoworkspace.Source `json:"sources"`
}

func (h *handler) handleProjects(w http.ResponseWriter, r *http.Request) {
	if !h.requireCatalog(w) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		projects, err := h.catalog.ListProjectsIncludingArchived(r.Context(), r.URL.Query().Get("include_archived") == "true")
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		writeAPIJSON(w, http.StatusOK, map[string][]zotigoworkspace.Project{"projects": projects})
	case http.MethodPost:
		var request createProjectRequest
		if err := readRequiredJSON(r, &request); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid project request")
			return
		}
		project, err := h.catalog.CreateProject(r.Context(), request.Name)
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		writeAPIJSON(w, http.StatusCreated, project)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (h *handler) handleSourceInspection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	h.inspectSource(w, r)
}

func (h *handler) handleProject(w http.ResponseWriter, r *http.Request) {
	if !h.requireCatalog(w) {
		return
	}
	parts := splitCatalogPath(r.URL.Path, "/projects/")
	if len(parts) == 0 {
		writeAPIError(w, http.StatusNotFound, "project not found")
		return
	}
	projectID := parts[0]
	switch {
	case len(parts) == 1 && r.Method == http.MethodPut:
		unlockProject := h.workspaceOps.lock(projectOperationLockKey(projectID))
		defer unlockProject()
		var request createProjectRequest
		if err := readRequiredJSON(r, &request); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid project rename request")
			return
		}
		project, err := h.catalog.RenameProject(r.Context(), projectID, request.Name)
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		writeAPIJSON(w, http.StatusOK, project)
	case len(parts) == 1 && r.Method == http.MethodGet:
		project, err := h.catalog.GetProject(r.Context(), projectID)
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		sources, err := h.catalog.ListSources(r.Context(), projectID)
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		writeAPIJSON(w, http.StatusOK, projectDetail{Project: project, Sources: sources})
	case len(parts) == 2 && parts[1] == "archive-preview" && r.Method == http.MethodGet:
		impact, err := h.catalog.PreviewProjectArchive(r.Context(), projectID)
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		writeAPIJSON(w, http.StatusOK, impact)
	case len(parts) == 2 && parts[1] == "archive" && r.Method == http.MethodPost:
		release, blocked, err := h.lockProjectLifecycle(r.Context(), projectID)
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		defer release()
		if blocked {
			writeAPIError(w, http.StatusConflict, "project has active sessions")
			return
		}
		project, err := h.catalog.ArchiveProject(r.Context(), projectID)
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		writeAPIJSON(w, http.StatusOK, project)
	case len(parts) == 2 && parts[1] == "unarchive" && r.Method == http.MethodPost:
		unlockProject := h.workspaceOps.lock(projectOperationLockKey(projectID))
		defer unlockProject()
		project, err := h.catalog.UnarchiveProject(r.Context(), projectID)
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		writeAPIJSON(w, http.StatusOK, project)
	case len(parts) == 2 && parts[1] == "delete-preview" && r.Method == http.MethodGet:
		impact, err := h.catalog.PreviewProjectDelete(r.Context(), projectID)
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		writeAPIJSON(w, http.StatusOK, impact)
	case len(parts) == 2 && parts[1] == "delete" && r.Method == http.MethodPost:
		release, blocked, err := h.lockProjectLifecycle(r.Context(), projectID)
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		defer release()
		if blocked {
			writeAPIError(w, http.StatusConflict, "project has active sessions")
			return
		}
		var request deleteCatalogRequest
		if err := readRequiredJSON(r, &request); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid project delete request")
			return
		}
		if err := h.catalog.DeleteProject(r.Context(), projectID, request.Confirmation); err != nil {
			h.writeCatalogError(w, err)
			return
		}
		writeAPIJSON(w, http.StatusOK, map[string]string{"project_id": projectID, "status": "deleted"})
	case len(parts) == 3 && parts[1] == "sources" && parts[2] == "inspect" && r.Method == http.MethodPost:
		h.inspectProjectSource(w, r, projectID)
	case len(parts) == 2 && parts[1] == "sources" && r.Method == http.MethodPost:
		unlockProject := h.workspaceOps.lock(projectOperationLockKey(projectID))
		defer unlockProject()
		h.addProjectSource(w, r, projectID)
	case len(parts) == 3 && parts[1] == "sources" && r.Method == http.MethodDelete:
		unlockProject := h.workspaceOps.lock(projectOperationLockKey(projectID))
		defer unlockProject()
		if err := h.catalog.DeleteSource(r.Context(), projectID, parts[2]); err != nil {
			h.writeCatalogError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 2 && parts[1] == "workspaces" && r.Method == http.MethodGet:
		includeArchived := r.URL.Query().Get("include_archived") == "true"
		workspaces, err := h.catalog.ListWorkspaces(r.Context(), projectID, includeArchived)
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		writeAPIJSON(w, http.StatusOK, map[string][]zotigoworkspace.Workspace{"workspaces": workspaces})
	case len(parts) == 2 && parts[1] == "workspaces" && r.Method == http.MethodPost:
		unlockProject := h.workspaceOps.lock(projectOperationLockKey(projectID))
		defer unlockProject()
		var request createWorkspaceRequest
		if err := readRequiredJSON(r, &request); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid workspace request")
			return
		}
		workspace, err := h.catalog.CreateWorkspacePlan(r.Context(), projectID, request.Title, request.Sources)
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		workspace, err = h.catalog.ProvisionWorkspaceScaffold(r.Context(), workspace.ID)
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		writeAPIJSON(w, http.StatusCreated, workspace)
	default:
		writeAPIError(w, http.StatusNotFound, "project route not found")
	}
}

func (h *handler) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	if !h.requireCatalog(w) {
		return
	}
	parts := splitCatalogPath(r.URL.Path, "/workspaces/")
	if len(parts) == 2 && parts[1] == "archive-preview" && r.Method == http.MethodGet {
		impact, err := h.catalog.PreviewArchive(r.Context(), parts[0])
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		writeAPIJSON(w, http.StatusOK, impact)
		return
	}
	if len(parts) == 2 && parts[1] == "archive" && r.Method == http.MethodPost {
		unlockWorkspace := h.workspaceOps.lock(parts[0])
		defer unlockWorkspace()
		release, blocked, err := h.lockWorkspaceSessions(r.Context(), parts[0])
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		defer release()
		if blocked {
			writeAPIError(w, http.StatusConflict, "workspace has active sessions")
			return
		}
		workspace, err := h.catalog.ArchiveWorkspace(r.Context(), parts[0])
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		writeAPIJSON(w, http.StatusOK, workspace)
		return
	}
	if len(parts) == 2 && parts[1] == "unarchive" && r.Method == http.MethodPost {
		_, release, err := h.lockWorkspaceForUse(r.Context(), parts[0])
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		defer release()
		workspace, err := h.catalog.UnarchiveWorkspace(r.Context(), parts[0])
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		writeAPIJSON(w, http.StatusOK, workspace)
		return
	}
	if len(parts) == 2 && parts[1] == "delete-preview" && r.Method == http.MethodGet {
		impact, err := h.catalog.PreviewDelete(r.Context(), parts[0])
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		writeAPIJSON(w, http.StatusOK, impact)
		return
	}
	if len(parts) == 2 && parts[1] == "delete" && r.Method == http.MethodPost {
		unlockWorkspace := h.workspaceOps.lock(parts[0])
		defer unlockWorkspace()
		release, blocked, err := h.lockWorkspaceSessions(r.Context(), parts[0])
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		defer release()
		if blocked {
			writeAPIError(w, http.StatusConflict, "workspace has active sessions")
			return
		}
		var request deleteCatalogRequest
		if err := readRequiredJSON(r, &request); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid workspace delete request")
			return
		}
		if err := h.catalog.DeleteWorkspace(r.Context(), parts[0], request.Confirmation); err != nil {
			h.writeCatalogError(w, err)
			return
		}
		writeAPIJSON(w, http.StatusOK, map[string]string{"workspace_id": parts[0], "status": "deleted"})
		return
	}
	if len(parts) == 2 && parts[1] == "retry" && r.Method == http.MethodPost {
		_, release, err := h.lockWorkspaceForUse(r.Context(), parts[0])
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		defer release()
		workspace, err := h.catalog.ProvisionWorkspace(r.Context(), parts[0])
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		writeAPIJSON(w, http.StatusOK, workspace)
		return
	}
	if len(parts) != 1 || r.Method != http.MethodGet {
		writeAPIError(w, http.StatusNotFound, "workspace route not found")
		return
	}
	workspace, err := h.catalog.GetWorkspace(r.Context(), parts[0])
	if err != nil {
		h.writeCatalogError(w, err)
		return
	}
	writeAPIJSON(w, http.StatusOK, workspace)
}

func projectOperationLockKey(projectID string) string {
	return "project:" + projectID
}

func (h *handler) lockWorkspaceForUse(ctx context.Context, workspaceID string) (zotigoworkspace.Workspace, func(), error) {
	workspace, err := h.catalog.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return zotigoworkspace.Workspace{}, func() {}, err
	}
	unlocks := []func(){
		h.workspaceOps.lock(projectOperationLockKey(workspace.ProjectID)),
		h.workspaceOps.lock(workspace.ID),
	}
	release := func() {
		for index := len(unlocks) - 1; index >= 0; index-- {
			unlocks[index]()
		}
	}
	workspace, err = h.catalog.GetWorkspace(ctx, workspaceID)
	if err != nil {
		release()
		return zotigoworkspace.Workspace{}, func() {}, err
	}
	project, err := h.catalog.GetProject(ctx, workspace.ProjectID)
	if err != nil {
		release()
		return zotigoworkspace.Workspace{}, func() {}, err
	}
	if project.Status != zotigoworkspace.ProjectStatusActive {
		release()
		return zotigoworkspace.Workspace{}, func() {}, fmt.Errorf("%w: project is %s", zotigoworkspace.ErrConflict, project.Status)
	}
	return workspace, release, nil
}

func (h *handler) lockProjectLifecycle(ctx context.Context, projectID string) (func(), bool, error) {
	unlocks := []func(){h.workspaceOps.lock(projectOperationLockKey(projectID))}
	release := func() {
		for index := len(unlocks) - 1; index >= 0; index-- {
			unlocks[index]()
		}
	}
	workspaces, err := h.catalog.ListWorkspaces(ctx, projectID, true)
	if err != nil {
		release()
		return func() {}, false, err
	}
	sort.Slice(workspaces, func(left int, right int) bool { return workspaces[left].ID < workspaces[right].ID })
	for _, workspace := range workspaces {
		unlocks = append(unlocks, h.workspaceOps.lock(workspace.ID))
	}
	sessionIDs := make([]string, 0)
	for _, workspace := range workspaces {
		ids, err := h.catalog.WorkspaceSessionIDs(ctx, workspace.ID)
		if err != nil {
			release()
			return func() {}, false, err
		}
		sessionIDs = append(sessionIDs, ids...)
	}
	sort.Strings(sessionIDs)
	for _, sessionID := range sessionIDs {
		unlocks = append(unlocks, h.sessionOps.lock(sessionID))
	}
	for _, sessionID := range sessionIDs {
		if session, ok := h.registry.Get(sessionID); ok && sessionIsActive(session) {
			return release, true, nil
		}
		if h.store != nil {
			locked, err := h.store.IsLocked(ctx, sessionID)
			if err != nil {
				release()
				return func() {}, false, err
			}
			if locked {
				return release, true, nil
			}
		}
	}
	return release, false, nil
}

func (h *handler) lockWorkspaceSessions(ctx context.Context, workspaceID string) (func(), bool, error) {
	sessionIDs, err := h.catalog.WorkspaceSessionIDs(ctx, workspaceID)
	if err != nil {
		return func() {}, false, err
	}
	unlocks := make([]func(), 0, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		unlocks = append(unlocks, h.sessionOps.lock(sessionID))
	}
	release := func() {
		for index := len(unlocks) - 1; index >= 0; index-- {
			unlocks[index]()
		}
	}
	for _, sessionID := range sessionIDs {
		if session, ok := h.registry.Get(sessionID); ok && sessionIsActive(session) {
			return release, true, nil
		}
		if h.store != nil {
			locked, err := h.store.IsLocked(ctx, sessionID)
			if err != nil {
				release()
				return func() {}, false, err
			}
			if locked {
				return release, true, nil
			}
		}
	}
	return release, false, nil
}

func (h *handler) inspectProjectSource(w http.ResponseWriter, r *http.Request, projectID string) {
	if _, err := h.catalog.GetProject(r.Context(), projectID); err != nil {
		h.writeCatalogError(w, err)
		return
	}
	h.inspectSource(w, r)
}

func (h *handler) inspectSource(w http.ResponseWriter, r *http.Request) {
	var request inspectSourceRequest
	if err := readRequiredJSON(r, &request); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid source inspection request")
		return
	}
	inspection, err := zotigoworkspace.InspectSource(r.Context(), request.Path)
	if err != nil {
		h.writeCatalogError(w, err)
		return
	}
	writeAPIJSON(w, http.StatusOK, inspection)
}

func (h *handler) addProjectSource(w http.ResponseWriter, r *http.Request, projectID string) {
	var request createSourceRequest
	if err := readRequiredJSON(r, &request); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid source request")
		return
	}
	inspection, err := zotigoworkspace.InspectSource(r.Context(), request.Path)
	if err != nil {
		h.writeCatalogError(w, err)
		return
	}
	input := zotigoworkspace.SourceInput{
		Kind:            inspection.Kind,
		CanonicalPath:   inspection.CanonicalPath,
		GitCommonDir:    inspection.GitCommonDir,
		GitObjectFormat: inspection.GitObjectFormat,
		SourceKey:       inspection.SourceKey,
	}
	if inspection.Kind == zotigoworkspace.SourceKindFolder {
		input.FolderMode = request.FolderMode
	} else if request.FolderMode != "" {
		writeAPIError(w, http.StatusBadRequest, "folder_mode is only valid for folder sources")
		return
	}
	source, err := h.catalog.AddSource(r.Context(), projectID, input)
	if err != nil {
		h.writeCatalogError(w, err)
		return
	}
	writeAPIJSON(w, http.StatusCreated, source)
}

func (h *handler) requireCatalog(w http.ResponseWriter) bool {
	if h.catalog != nil {
		return true
	}
	message := "workspace catalog is unavailable"
	if h.catalogErr != nil {
		message += ": " + h.catalogErr.Error()
	}
	writeAPIError(w, http.StatusServiceUnavailable, message)
	return false
}

func (h *handler) writeCatalogError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, zotigoworkspace.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "catalog record not found")
	case errors.Is(err, zotigoworkspace.ErrInvalid):
		writeAPIError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, zotigoworkspace.ErrConflict), errors.Is(err, zotigoworkspace.ErrSourceInUse):
		writeAPIError(w, http.StatusConflict, err.Error())
	default:
		if h.logger != nil {
			h.logger.Printf("Workspace catalog operation failed: %v", err)
		}
		writeAPIError(w, http.StatusInternalServerError, "workspace catalog operation failed")
	}
}

func splitCatalogPath(path string, prefix string) []string {
	value := strings.Trim(strings.TrimPrefix(path, prefix), "/")
	if value == "" {
		return nil
	}
	return strings.Split(value, "/")
}
