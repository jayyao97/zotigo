package zotigod

import (
	"net/http"

	"github.com/jayyao97/zotigo/core/workspace"
)

func (h *handler) handleNavigation(w http.ResponseWriter, r *http.Request) {
	if !h.requireCatalog(w) {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	result, err := h.catalog.GetNavigation(r.Context())
	if err != nil {
		h.writeCatalogError(w, err)
		return
	}
	writeAPIJSON(w, http.StatusOK, result)
}

func (h *handler) handleNavigationImport(w http.ResponseWriter, r *http.Request) {
	if !h.requireCatalog(w) {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request struct {
		Orders []workspace.NavigationOrder `json:"orders"`
	}
	if err := readRequiredJSON(r, &request); err != nil || request.Orders == nil {
		writeAPIError(w, http.StatusBadRequest, "invalid navigation import")
		return
	}
	if err := h.catalog.ImportNavigation(r.Context(), request.Orders); err != nil {
		h.writeCatalogError(w, err)
		return
	}
	writeAPIJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *handler) handleNavigationOrder(w http.ResponseWriter, r *http.Request) {
	if !h.requireCatalog(w) {
		return
	}
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", "PUT")
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request struct {
		Scope    string                     `json:"scope"`
		ParentID string                     `json:"parent_id"`
		Items    []workspace.NavigationItem `json:"items"`
	}
	if err := readRequiredJSON(r, &request); err != nil || request.Items == nil {
		writeAPIError(w, http.StatusBadRequest, "invalid navigation order")
		return
	}
	if err := h.catalog.ReorderNavigation(r.Context(), request.Scope, request.ParentID, request.Items); err != nil {
		h.writeCatalogError(w, err)
		return
	}
	writeAPIJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *handler) handleNavigationPin(w http.ResponseWriter, r *http.Request) {
	if !h.requireCatalog(w) {
		return
	}
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", "PUT")
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request struct {
		Item   workspace.NavigationItem `json:"item"`
		Pinned *bool                    `json:"pinned"`
	}
	if err := readRequiredJSON(r, &request); err != nil || request.Pinned == nil {
		writeAPIError(w, http.StatusBadRequest, "invalid navigation pin")
		return
	}
	if err := h.catalog.SetNavigationPinned(r.Context(), request.Item, *request.Pinned); err != nil {
		h.writeCatalogError(w, err)
		return
	}
	writeAPIJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
