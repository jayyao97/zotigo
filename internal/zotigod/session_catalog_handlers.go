package zotigod

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
)

type sessionProjection struct {
	Runtime      *Session                             `json:"runtime"`
	Organization *zotigoworkspace.SessionOrganization `json:"organization"`
	Availability string                               `json:"availability"`
}

type sessionTitleRequest struct {
	Title string `json:"title"`
}

type sessionPinnedRequest struct {
	Pinned bool `json:"pinned"`
}

type sessionPositionRequest struct {
	Position int64 `json:"position"`
}

func (h *handler) handleCatalogSessions(w http.ResponseWriter, r *http.Request) {
	if !h.requireCatalog(w) {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	projections, err := h.listSessionProjections(r.Context())
	if err != nil {
		h.writeCatalogError(w, err)
		return
	}
	projectID := r.URL.Query().Get("project_id")
	workspaceID := r.URL.Query().Get("workspace_id")
	pinnedOnly := r.URL.Query().Get("pinned") == "true"
	includeArchived := r.URL.Query().Get("include_archived") == "true"
	filtered := make([]sessionProjection, 0, len(projections))
	for _, projection := range projections {
		organization := projection.Organization
		if projectID != "" && (organization == nil || organization.ProjectID == nil || *organization.ProjectID != projectID) {
			continue
		}
		if workspaceID != "" && (organization == nil || organization.WorkspaceID == nil || *organization.WorkspaceID != workspaceID) {
			continue
		}
		if pinnedOnly && (organization == nil || organization.PinnedAt == nil) {
			continue
		}
		if !includeArchived && projection.Availability == "archived" {
			continue
		}
		filtered = append(filtered, projection)
	}
	writeAPIJSON(w, http.StatusOK, map[string][]sessionProjection{"sessions": filtered})
}

func (h *handler) handleCatalogSession(w http.ResponseWriter, r *http.Request, id string) {
	if !h.requireCatalog(w) {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	projection, found, err := h.sessionProjection(r.Context(), id)
	if err != nil {
		h.writeCatalogError(w, err)
		return
	}
	if !found {
		writeAPIError(w, http.StatusNotFound, "session not found")
		return
	}
	writeAPIJSON(w, http.StatusOK, projection)
}

func (h *handler) handleCatalogSessionNotFound(w http.ResponseWriter, r *http.Request) {
	if !h.requireCatalog(w) {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeAPIError(w, http.StatusNotFound, "session not found")
}

func (h *handler) handleSessionOrganizationTitle(w http.ResponseWriter, r *http.Request, id string) {
	if !h.requireCatalog(w) {
		return
	}
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", http.MethodPut)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, found, err := h.catalogRuntime(r.Context(), id); err != nil || !found {
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "load session failed")
		} else {
			writeAPIError(w, http.StatusNotFound, "session not found")
		}
		return
	}
	if _, err := h.catalog.EnsureSessionOrganization(r.Context(), id); err != nil {
		h.writeCatalogError(w, err)
		return
	}
	var request sessionTitleRequest
	if err := readRequiredJSON(r, &request); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid session title request")
		return
	}
	if _, err := h.catalog.SetSessionTitle(r.Context(), id, request.Title); err != nil {
		h.writeCatalogError(w, err)
		return
	}
	h.writeSessionProjection(w, r, id)
}

func (h *handler) handleSessionOrganizationPinned(w http.ResponseWriter, r *http.Request, id string) {
	if !h.requireCatalog(w) {
		return
	}
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", http.MethodPut)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, found, err := h.catalogRuntime(r.Context(), id); err != nil || !found {
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "load session failed")
		} else {
			writeAPIError(w, http.StatusNotFound, "session not found")
		}
		return
	}
	if _, err := h.catalog.EnsureSessionOrganization(r.Context(), id); err != nil {
		h.writeCatalogError(w, err)
		return
	}
	var request sessionPinnedRequest
	if err := readRequiredJSON(r, &request); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid session pinned request")
		return
	}
	if _, err := h.catalog.SetSessionPinned(r.Context(), id, request.Pinned); err != nil {
		h.writeCatalogError(w, err)
		return
	}
	h.writeSessionProjection(w, r, id)
}

func (h *handler) handleSessionOrganizationPosition(w http.ResponseWriter, r *http.Request, id string) {
	if !h.requireCatalog(w) {
		return
	}
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", http.MethodPut)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, found, err := h.catalogRuntime(r.Context(), id); err != nil || !found {
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "load session failed")
		} else {
			writeAPIError(w, http.StatusNotFound, "session not found")
		}
		return
	}
	var request sessionPositionRequest
	if err := readRequiredJSON(r, &request); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid session position request")
		return
	}
	if _, err := h.catalog.SetSessionPosition(r.Context(), id, request.Position); err != nil {
		h.writeCatalogError(w, err)
		return
	}
	h.writeSessionProjection(w, r, id)
}

func (h *handler) handleSessionOrganizationArchive(w http.ResponseWriter, r *http.Request, id string, archived bool) {
	if !h.requireCatalog(w) {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	unlock := h.sessionOps.lock(id)
	defer unlock()
	if archived {
		if session, ok := h.registry.Get(id); ok && sessionIsActive(session) {
			writeAPIError(w, http.StatusConflict, "session is active")
			return
		}
		if h.store != nil {
			locked, err := h.store.IsLocked(r.Context(), id)
			if err != nil {
				writeAPIError(w, http.StatusInternalServerError, "check session lock failed")
				return
			}
			if locked {
				writeAPIError(w, http.StatusConflict, "session is active")
				return
			}
		}
	}
	if _, found, err := h.catalogRuntime(r.Context(), id); err != nil || !found {
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "load session failed")
		} else {
			writeAPIError(w, http.StatusNotFound, "session not found")
		}
		return
	}
	if _, err := h.catalog.EnsureSessionOrganization(r.Context(), id); err != nil {
		h.writeCatalogError(w, err)
		return
	}
	if _, err := h.catalog.SetSessionArchived(r.Context(), id, archived); err != nil {
		h.writeCatalogError(w, err)
		return
	}
	h.writeSessionProjection(w, r, id)
}

func (h *handler) writeSessionProjection(w http.ResponseWriter, r *http.Request, id string) {
	projection, found, err := h.sessionProjection(r.Context(), id)
	if err != nil {
		h.writeCatalogError(w, err)
		return
	}
	if !found {
		writeAPIError(w, http.StatusNotFound, "session not found")
		return
	}
	writeAPIJSON(w, http.StatusOK, projection)
}

func (h *handler) listSessionProjections(ctx context.Context) ([]sessionProjection, error) {
	runtimes, err := h.listSessions(ctx)
	if err != nil {
		return nil, err
	}
	organizations, err := h.catalog.ListSessionOrganizations(ctx)
	if err != nil {
		return nil, err
	}
	runtimeByID := make(map[string]Session, len(runtimes))
	for _, runtime := range runtimes {
		runtimeByID[runtime.ID] = runtime
	}
	organizationByID := make(map[string]zotigoworkspace.SessionOrganization, len(organizations))
	for _, organization := range organizations {
		organizationByID[organization.SessionID] = organization
	}
	ids := make(map[string]struct{}, len(runtimes)+len(organizations))
	for id := range runtimeByID {
		ids[id] = struct{}{}
	}
	for id := range organizationByID {
		ids[id] = struct{}{}
	}
	result := make([]sessionProjection, 0, len(ids))
	for id := range ids {
		var runtimePointer *Session
		if runtime, ok := runtimeByID[id]; ok {
			runtimeCopy := runtime
			runtimePointer = &runtimeCopy
		}
		var organizationPointer *zotigoworkspace.SessionOrganization
		if organization, ok := organizationByID[id]; ok {
			organizationCopy := organization
			organizationPointer = &organizationCopy
		}
		availability, err := h.sessionAvailability(ctx, runtimePointer, organizationPointer)
		if err != nil {
			return nil, err
		}
		result = append(result, sessionProjection{Runtime: runtimePointer, Organization: organizationPointer, Availability: availability})
	}
	sort.Slice(result, func(i, j int) bool {
		left := projectionUpdatedAt(result[i])
		right := projectionUpdatedAt(result[j])
		if left.Equal(right) {
			return projectionID(result[i]) < projectionID(result[j])
		}
		return left.After(right)
	})
	return result, nil
}

func (h *handler) sessionProjection(ctx context.Context, id string) (sessionProjection, bool, error) {
	runtime, runtimeFound, err := h.catalogRuntime(ctx, id)
	if err != nil {
		return sessionProjection{}, false, err
	}
	organization, organizationErr := h.catalog.GetSessionOrganization(ctx, id)
	organizationFound := organizationErr == nil
	if organizationErr != nil && !errors.Is(organizationErr, zotigoworkspace.ErrNotFound) {
		return sessionProjection{}, false, organizationErr
	}
	if !runtimeFound && !organizationFound {
		return sessionProjection{}, false, nil
	}
	var organizationPointer *zotigoworkspace.SessionOrganization
	if organizationFound {
		organizationPointer = &organization
	}
	availability, err := h.sessionAvailability(ctx, runtime, organizationPointer)
	return sessionProjection{Runtime: runtime, Organization: organizationPointer, Availability: availability}, true, err
}

func (h *handler) catalogRuntime(ctx context.Context, id string) (*Session, bool, error) {
	if runtime, ok := h.registry.Get(id); ok {
		runtime.Live = true
		return &runtime, true, nil
	}
	runtime, found, err := h.storedSession(ctx, id)
	if err != nil || !found {
		return nil, found, err
	}
	return &runtime, true, nil
}

func (h *handler) sessionAvailability(ctx context.Context, runtime *Session, organization *zotigoworkspace.SessionOrganization) (string, error) {
	if organization != nil && organization.EffectiveArchived() {
		return "archived", nil
	}
	if runtime == nil {
		return "runtime_missing", nil
	}
	if organization != nil && organization.WorkspaceID != nil {
		workspace, err := h.catalog.GetWorkspace(ctx, *organization.WorkspaceID)
		if err != nil {
			if errors.Is(err, zotigoworkspace.ErrNotFound) {
				return "workspace_not_ready", nil
			}
			return "", err
		}
		project, err := h.catalog.GetProject(ctx, workspace.ProjectID)
		if err != nil {
			if errors.Is(err, zotigoworkspace.ErrNotFound) {
				return "workspace_not_ready", nil
			}
			return "", err
		}
		if project.Status != zotigoworkspace.ProjectStatusActive {
			return "workspace_not_ready", nil
		}
		if workspace.Status != zotigoworkspace.WorkspaceStatusReady {
			return "workspace_not_ready", nil
		}
		if !sameFilesystemPath(runtime.WorkingDirectory, workspace.RootPath) {
			return "cwd_mismatch", nil
		}
	} else {
		deleted, err := h.catalog.DeletedWorkspaceOwnsPath(ctx, runtime.WorkingDirectory)
		if err != nil {
			return "", err
		}
		if deleted {
			return "cwd_unavailable", nil
		}
	}
	info, err := os.Stat(runtime.WorkingDirectory)
	if err != nil || !info.IsDir() {
		return "cwd_unavailable", nil
	}
	return "ready", nil
}

func (h *handler) ensureSessionActivatable(ctx context.Context, id string) (string, error) {
	if h.catalog == nil {
		return "", nil
	}
	projection, found, err := h.sessionProjection(ctx, id)
	if err != nil || !found || projection.Runtime == nil {
		return "", err
	}
	if projection.Availability != "ready" {
		return projection.Availability, nil
	}
	return "", nil
}

func projectionUpdatedAt(projection sessionProjection) time.Time {
	if projection.Organization != nil {
		return projection.Organization.UpdatedAt
	}
	if projection.Runtime != nil {
		return projection.Runtime.CreatedAt
	}
	return time.Time{}
}

func projectionID(projection sessionProjection) string {
	if projection.Runtime != nil {
		return projection.Runtime.ID
	}
	if projection.Organization != nil {
		return projection.Organization.SessionID
	}
	return ""
}

func sessionIsActive(session Session) bool {
	return session.State == SessionStateStarting || session.State == SessionStateRunning || session.State == SessionStatePaused
}

func sameFilesystemPath(left string, right string) bool {
	return filepath.Clean(left) == filepath.Clean(right)
}
