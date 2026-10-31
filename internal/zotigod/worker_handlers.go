package zotigod

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gorilla/websocket"
	zotigoworkspace "github.com/jayyao97/zotigo/core/workspace"
	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
)

const workerGenerationHeader = "X-Zotigo-Worker-Generation"
const workerWorkspaceHeader = "X-Zotigo-Workspace-ID"

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
	switch session.State {
	case SessionStateStarting, SessionStateRunning, SessionStatePausing, SessionStatePaused:
	default:
		writeAPIError(w, http.StatusConflict, "worker connect requires a live session")
		return
	}
	activation := r.URL.Query().Get("activation")
	if !workerActivationMatches(session, activation) {
		writeAPIError(w, http.StatusConflict, "worker activation does not match the active session")
		return
	}

	generation := newZotigodID("worker")
	responseHeader := http.Header{}
	responseHeader.Set(workerGenerationHeader, generation)
	if h.catalog != nil {
		organization, err := h.catalog.GetSessionOrganization(r.Context(), sessionID)
		if err != nil && !errors.Is(err, zotigoworkspace.ErrNotFound) {
			writeAPIError(w, http.StatusInternalServerError, "load worker workspace identity")
			return
		}
		if err == nil && organization.WorkspaceID != nil {
			responseHeader.Set(workerWorkspaceHeader, *organization.WorkspaceID)
		}
	}
	conn, err := workerUpgrader.Upgrade(w, r, responseHeader)
	if err != nil {
		return
	}
	unlock := h.sessionOps.lock(sessionID)
	defer unlock()
	session, ok = h.registry.Get(sessionID)
	if !ok || (session.State != SessionStateStarting && session.State != SessionStateRunning && session.State != SessionStatePausing && session.State != SessionStatePaused) || !workerActivationMatches(session, activation) || h.workers.Has(sessionID) {
		_ = conn.Close()
		return
	}
	if err := h.registry.SetWorkerGeneration(sessionID, generation); err != nil {
		_ = conn.Close()
		return
	}
	h.workers.Register(sessionID, generation, conn)
}

func workerActivationMatches(session Session, value string) bool {
	if zotigoruntime.AgentKind(session.Agent) != zotigoruntime.AgentCodex {
		return true
	}
	activation, err := strconv.ParseUint(value, 10, 64)
	return err == nil && activation == session.workerActivation
}
