package zotigod

import (
	"net/http"

	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
)

// This is a catalog read projection, not client UI state. Selection and local
// preferences remain client-owned. It does not fetch Git or synchronize runtimes.
type catalogSnapshot struct {
	SessionQueryVersion int                         `json:"session_query_version"`
	Projects            []projectDetail             `json:"projects"`
	Workspaces          []zotigoworkspace.Workspace `json:"workspaces"`
	Navigation          zotigoworkspace.Navigation  `json:"navigation"`
	Sessions            []sessionProjection         `json:"sessions"`
}

func (h *handler) handleCatalogSnapshot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !h.requireCatalog(w) {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctx := r.Context()
	projects, err := h.catalog.ListProjects(ctx)
	if err != nil {
		h.writeCatalogError(w, err)
		return
	}
	result := catalogSnapshot{SessionQueryVersion: 1, Projects: []projectDetail{}, Workspaces: []zotigoworkspace.Workspace{}, Sessions: []sessionProjection{}}
	for _, project := range projects {
		sources, err := h.catalog.ListSources(ctx, project.ID)
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		workspaces, err := h.catalog.ListWorkspaces(ctx, project.ID, false)
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		result.Projects = append(result.Projects, projectDetail{Project: project, Sources: sources})
		result.Workspaces = append(result.Workspaces, workspaces...)
	}
	result.Navigation, err = h.catalog.GetNavigation(ctx)
	if err != nil {
		h.writeCatalogError(w, err)
		return
	}
	if r.URL.Query().Get("sessions") == "none" {
		writeAPIJSON(w, http.StatusOK, result)
		return
	}
	projections, err := h.listSessionProjections(ctx)
	if err != nil {
		h.writeCatalogError(w, err)
		return
	}
	for _, projection := range projections {
		if projection.Availability != "archived" {
			result.Sessions = append(result.Sessions, projection)
		}
	}
	writeAPIJSON(w, http.StatusOK, result)
}
