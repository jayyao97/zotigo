package zotigod

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gorilla/websocket"
)

const workerGenerationHeader = "X-Zotigo-Worker-Generation"
const workerWorkspaceBindingRevisionHeader = "X-Zotigo-Workspace-Binding-Revision"

var workerUpgrader = websocket.Upgrader{}

func (h *handler) handleWorkerConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	sessionID := r.URL.Query().Get("session_id")
	if err := validateWorkerSessionID(sessionID); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	session, ok := h.registry.Get(sessionID)
	if !ok {
		writeAPIError(w, http.StatusNotFound, "session not found")
		return
	}
	var workspaceRevision uint64
	if rawRevision := strings.TrimSpace(r.Header.Get(workerWorkspaceBindingRevisionHeader)); rawRevision != "" {
		parsedRevision, parseErr := strconv.ParseUint(rawRevision, 10, 64)
		if parseErr != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid workspace binding revision")
			return
		}
		workspaceRevision = parsedRevision
	}
	expectedRevision, err := h.expectedWorkspaceBindingRevision(r.Context(), sessionID)
	if err != nil {
		writeAPIError(w, http.StatusConflict, err.Error())
		return
	}
	if workspaceRevision != expectedRevision {
		writeAPIError(w, http.StatusConflict, "stale workspace binding revision")
		return
	}
	switch session.State {
	case SessionStateStarting, SessionStateRunning, SessionStatePaused:
	default:
		writeAPIError(w, http.StatusConflict, "worker connect requires a live session")
		return
	}

	generation := newZotigodID("worker")
	responseHeader := http.Header{}
	responseHeader.Set(workerGenerationHeader, generation)
	conn, err := workerUpgrader.Upgrade(w, r, responseHeader)
	if err != nil {
		return
	}
	unlock := h.sessionOps.lock(sessionID)
	defer unlock()
	session, ok = h.registry.Get(sessionID)
	if !ok || (session.State != SessionStateStarting && session.State != SessionStateRunning && session.State != SessionStatePaused) || h.workers.Has(sessionID) {
		_ = conn.Close()
		return
	}
	expectedRevision, err = h.expectedWorkspaceBindingRevision(r.Context(), sessionID)
	if err != nil || workspaceRevision != expectedRevision {
		_ = conn.Close()
		return
	}
	h.workers.Register(sessionID, generation, workspaceRevision, conn)
}

func (h *handler) expectedWorkspaceBindingRevision(ctx context.Context, sessionID string) (uint64, error) {
	if h.store == nil {
		session, ok := h.registry.Get(sessionID)
		if !ok {
			return 0, fmt.Errorf("load worker session binding")
		}
		if session.Agent == "" || session.Agent == "zotigo" {
			return 0, nil
		}
		return 0, fmt.Errorf("runtime %q requires persistent workspace binding", session.Agent)
	}
	stored, err := h.store.Get(ctx, sessionID)
	if err != nil || stored == nil {
		return 0, fmt.Errorf("load worker session binding")
	}
	if stored.Agent == "" || stored.Agent == "zotigo" {
		return 0, nil
	}
	organization, err := h.catalog.GetSessionOrganization(ctx, sessionID)
	if err != nil || organization.WorkspaceID == nil {
		return 0, fmt.Errorf("load worker workspace binding")
	}
	binding, err := h.catalog.GetRuntimeWorkspaceBinding(ctx, *organization.WorkspaceID, stored.Agent)
	if err != nil || binding.State != "bound" {
		return 0, fmt.Errorf("worker workspace binding is not ready")
	}
	return binding.Revision, nil
}
