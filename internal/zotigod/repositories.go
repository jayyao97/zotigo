package zotigod

import (
	"net/http"

	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
)

func (h *handler) handleSourceBranches(w http.ResponseWriter, r *http.Request, projectID, sourceID string) {
	if !h.requireCatalog(w) {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var branches []zotigoworkspace.RepositoryBranch
	var err error
	if r.Method == http.MethodPost {
		branches, err = h.catalog.RefreshSourceBranches(r.Context(), projectID, sourceID)
	} else {
		branches, err = h.catalog.ListSourceBranches(r.Context(), projectID, sourceID)
	}
	if err != nil {
		h.writeCatalogError(w, err)
		return
	}
	writeAPIJSON(w, http.StatusOK, map[string][]zotigoworkspace.RepositoryBranch{"branches": branches})
}

func (h *handler) handleRepositories(w http.ResponseWriter, r *http.Request) {
	if !h.requireCatalog(w) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		repos, err := h.catalog.ListRepositories(r.Context())
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		writeAPIJSON(w, http.StatusOK, map[string][]zotigoworkspace.Repository{"repositories": repos})
	case http.MethodPost:
		var request struct {
			RemoteURL string `json:"remote_url"`
			Name      string `json:"name,omitempty"`
		}
		if err := readRequiredJSON(r, &request); err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid repository request")
			return
		}
		repo, err := h.catalog.CreateRepository(r.Context(), request.RemoteURL, request.Name)
		if err != nil {
			h.writeCatalogError(w, err)
			return
		}
		writeAPIJSON(w, http.StatusCreated, repo)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
