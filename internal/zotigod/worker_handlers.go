package zotigod

import (
	"net/http"

	"github.com/gorilla/websocket"
)

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
	case SessionStateStarting, SessionStateRunning, SessionStatePaused:
	default:
		writeAPIError(w, http.StatusConflict, "worker connect requires a live session")
		return
	}

	conn, err := workerUpgrader.Upgrade(w, r, nil)
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
	worker := h.workers.Register(sessionID, conn)
	if session.State == SessionStateStarting {
		if _, err := h.registry.MarkRunning(sessionID); err != nil {
			worker.close()
			return
		}
	}
}
