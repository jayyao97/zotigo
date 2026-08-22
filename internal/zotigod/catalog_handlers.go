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
		if r.URL.Query().Has("include_archived") {
			writeAPIError(w, http.StatusBadRequest, "include_archived is not supported; use status=active, status=archived, or status=all")
			return
		}
		var projects []zotigoworkspace.Project
		var err error
		switch r.URL.Query().Get("status") {
		case "", "active":
			projects, err = h.catalog.ListProjects(r.Context())
		case "archived":
			projects, err = h.catalog.ListArchivedProjects(r.Context())
		case "all":
			projects, err = h.catalog.ListAllProjects(r.Context())
		default:
			writeAPIError(w, http.StatusBadRequest, "status must be active, archived, or all")
			return
		}
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

func (h *handler) handleProject(w http.ResponseWriter, r *http.Request, projectID string) {
	if !h.requireCatalog(w) {
		return
	}
	switch r.Method {
	case http.MethodPut:
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
	case http.MethodGet:
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
	default:
		writeAPIError(w, http.StatusNotFound, "project route not found")
	}
}

func (h *handler) handleProjectArchivePreview(w http.ResponseWriter, r *http.Request, projectID string) {
	if !h.requireCatalog(w) {
		return
	}
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusNotFound, "project route not found")
		return
	}
	impact, err := h.catalog.PreviewProjectArchive(r.Context(), projectID)
	if err != nil {
		h.writeCatalogError(w, err)
		return
	}
	writeAPIJSON(w, http.StatusOK, impact)
}

func (h *handler) handleProjectArchive(w http.ResponseWriter, r *http.Request, projectID string) {
	if !h.requireCatalog(w) {
		return
	}
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusNotFound, "project route not found")
		return
	}
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
}

func (h *handler) handleProjectUnarchive(w http.ResponseWriter, r *http.Request, projectID string) {
	if !h.requireCatalog(w) {
		return
	}
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusNotFound, "project route not found")
		return
	}
	unlockProject := h.workspaceOps.lock(projectOperationLockKey(projectID))
	defer unlockProject()
	project, err := h.catalog.UnarchiveProject(r.Context(), projectID)
	if err != nil {
		h.writeCatalogError(w, err)
		return
	}
	writeAPIJSON(w, http.StatusOK, project)
}

func (h *handler) handleProjectDeletePreview(w http.ResponseWriter, r *http.Request, projectID string) {
	if !h.requireCatalog(w) {
		return
	}
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusNotFound, "project route not found")
		return
	}
	impact, err := h.catalog.PreviewProjectDelete(r.Context(), projectID)
	if err != nil {
		h.writeCatalogError(w, err)
		return
	}
	writeAPIJSON(w, http.StatusOK, impact)
}

func (h *handler) handleProjectDelete(w http.ResponseWriter, r *http.Request, projectID string) {
	if !h.requireCatalog(w) {
		return
	}
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusNotFound, "project route not found")
		return
	}
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
}

func (h *handler) handleProjectSourceInspection(w http.ResponseWriter, r *http.Request, projectID string) {
	if !h.requireCatalog(w) {
		return
	}
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusNotFound, "project route not found")
		return
	}
	h.inspectProjectSource(w, r, projectID)
}

func (h *handler) handleProjectSources(w http.ResponseWriter, r *http.Request, projectID string) {
	if !h.requireCatalog(w) {
		return
	}
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusNotFound, "project route not found")
		return
	}
	unlockProject := h.workspaceOps.lock(projectOperationLockKey(projectID))
	defer unlockProject()
	h.addProjectSource(w, r, projectID)
}

func (h *handler) handleProjectSource(w http.ResponseWriter, r *http.Request, projectID string, sourceID string) {
	if !h.requireCatalog(w) {
		return
	}
	if strings.Contains(sourceID, "/") {
		writeAPIError(w, http.StatusNotFound, "project route not found")
		return
	}
	if r.Method != http.MethodDelete {
		writeAPIError(w, http.StatusNotFound, "project route not found")
		return
	}
	unlockProject := h.workspaceOps.lock(projectOperationLockKey(projectID))
	defer unlockProject()
	if err := h.catalog.DeleteSource(r.Context(), projectID, sourceID); err != nil {
		h.writeCatalogError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) handleProjectWorkspaces(w http.ResponseWriter, r *http.Request, projectID string) {
	if !h.requireCatalog(w) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		includeArchived := r.URL.Query().Get("include_archived") == "true"
		workspaces, err := h.catalog.ListWorkspaces(r.Context(), projectID, includeArchived)
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		writeAPIJSON(w, http.StatusOK, map[string][]zotigoworkspace.Workspace{"workspaces": workspaces})
	case http.MethodPost:
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

func (h *handler) handleProjectRouteNotFound(w http.ResponseWriter, _ *http.Request) {
	if !h.requireCatalog(w) {
		return
	}
	writeAPIError(w, http.StatusNotFound, "project route not found")
}

func (h *handler) handleProjectNotFound(w http.ResponseWriter, _ *http.Request) {
	if !h.requireCatalog(w) {
		return
	}
	writeAPIError(w, http.StatusNotFound, "project not found")
}

func (h *handler) handleWorkspace(w http.ResponseWriter, r *http.Request, workspaceID string) {
	if !h.requireCatalog(w) {
		return
	}
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusNotFound, "workspace route not found")
		return
	}
	workspace, err := h.catalog.GetWorkspace(r.Context(), workspaceID)
	if err != nil {
		h.writeCatalogError(w, err)
		return
	}
	writeAPIJSON(w, http.StatusOK, workspace)
}

func (h *handler) handleWorkspaceSources(w http.ResponseWriter, r *http.Request, workspaceID string) {
	if !h.requireCatalog(w) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		sources, err := h.catalog.ListWorkspaceSources(r.Context(), workspaceID)
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		writeAPIJSON(w, http.StatusOK, map[string][]zotigoworkspace.WorkspaceSource{"sources": sources})
	case http.MethodPost:
		workspace, release, err := h.lockWorkspaceForUse(r.Context(), workspaceID)
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		defer release()
		if workspace.Status != zotigoworkspace.WorkspaceStatusReady {
			h.writeCatalogError(w, fmt.Errorf("%w: workspace is %s", zotigoworkspace.ErrConflict, workspace.Status))
			return
		}
		var request zotigoworkspace.WorkspaceSourceInput
		if err := readRequiredJSON(r, &request); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid workspace source request")
			return
		}
		source, err := h.catalog.AddWorkspaceSource(r.Context(), workspaceID, request)
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		writeAPIJSON(w, http.StatusCreated, source)
	default:
		writeAPIError(w, http.StatusNotFound, "workspace route not found")
	}
}

func (h *handler) handleWorkspaceArchivePreview(w http.ResponseWriter, r *http.Request, workspaceID string) {
	if !h.requireCatalog(w) {
		return
	}
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusNotFound, "workspace route not found")
		return
	}
	impact, err := h.catalog.PreviewArchive(r.Context(), workspaceID)
	if err != nil {
		h.writeCatalogError(w, err)
		return
	}
	writeAPIJSON(w, http.StatusOK, impact)
}

func (h *handler) handleWorkspaceArchive(w http.ResponseWriter, r *http.Request, workspaceID string) {
	if !h.requireCatalog(w) {
		return
	}
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusNotFound, "workspace route not found")
		return
	}
	unlockWorkspace := h.workspaceOps.lock(workspaceID)
	defer unlockWorkspace()
	release, blocked, err := h.lockWorkspaceSessions(r.Context(), workspaceID)
	if err != nil {
		h.writeCatalogError(w, err)
		return
	}
	defer release()
	if blocked {
		writeAPIError(w, http.StatusConflict, "workspace has active sessions")
		return
	}
	workspace, err := h.catalog.ArchiveWorkspace(r.Context(), workspaceID)
	if err != nil {
		h.writeCatalogError(w, err)
		return
	}
	writeAPIJSON(w, http.StatusOK, workspace)
}

func (h *handler) handleWorkspaceUnarchive(w http.ResponseWriter, r *http.Request, workspaceID string) {
	if !h.requireCatalog(w) {
		return
	}
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusNotFound, "workspace route not found")
		return
	}
	_, release, err := h.lockWorkspaceForUse(r.Context(), workspaceID)
	if err != nil {
		h.writeCatalogError(w, err)
		return
	}
	defer release()
	workspace, err := h.catalog.UnarchiveWorkspace(r.Context(), workspaceID)
	if err != nil {
		h.writeCatalogError(w, err)
		return
	}
	writeAPIJSON(w, http.StatusOK, workspace)
}

func (h *handler) handleWorkspaceDeletePreview(w http.ResponseWriter, r *http.Request, workspaceID string) {
	if !h.requireCatalog(w) {
		return
	}
	if r.Method != http.MethodGet {
		writeAPIError(w, http.StatusNotFound, "workspace route not found")
		return
	}
	impact, err := h.catalog.PreviewDelete(r.Context(), workspaceID)
	if err != nil {
		h.writeCatalogError(w, err)
		return
	}
	writeAPIJSON(w, http.StatusOK, impact)
}

func (h *handler) handleWorkspaceDelete(w http.ResponseWriter, r *http.Request, workspaceID string) {
	if !h.requireCatalog(w) {
		return
	}
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusNotFound, "workspace route not found")
		return
	}
	unlockWorkspace := h.workspaceOps.lock(workspaceID)
	defer unlockWorkspace()
	release, blocked, err := h.lockWorkspaceSessions(r.Context(), workspaceID)
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
	if err := h.catalog.DeleteWorkspace(r.Context(), workspaceID, request.Confirmation); err != nil {
		h.writeCatalogError(w, err)
		return
	}
	writeAPIJSON(w, http.StatusOK, map[string]string{"workspace_id": workspaceID, "status": "deleted"})
}

func (h *handler) handleWorkspaceRetry(w http.ResponseWriter, r *http.Request, workspaceID string) {
	if !h.requireCatalog(w) {
		return
	}
	if r.Method != http.MethodPost {
		writeAPIError(w, http.StatusNotFound, "workspace route not found")
		return
	}
	_, release, err := h.lockWorkspaceForUse(r.Context(), workspaceID)
	if err != nil {
		h.writeCatalogError(w, err)
		return
	}
	defer release()
	workspace, err := h.catalog.ProvisionWorkspace(r.Context(), workspaceID)
	if err != nil {
		h.writeCatalogError(w, err)
		return
	}
	writeAPIJSON(w, http.StatusOK, workspace)
}

func (h *handler) handleWorkspaceRouteNotFound(w http.ResponseWriter, _ *http.Request) {
	if !h.requireCatalog(w) {
		return
	}
	writeAPIError(w, http.StatusNotFound, "workspace route not found")
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
