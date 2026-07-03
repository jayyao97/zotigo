package zotigod

import (
	"net/http"

	"github.com/gorilla/websocket"
)

var workerUpgrader = websocket.Upgrader{}

func (h *handler) handleWorkerConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sessionID := r.URL.Query().Get("session_id")
	if err := validateWorkerSessionID(sessionID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	session, ok := h.registry.Get(sessionID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch session.State {
	case SessionStateStarting, SessionStateRunning, SessionStatePaused:
	default:
		http.Error(w, "worker connect requires a live session", http.StatusConflict)
		return
	}

	conn, err := workerUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	if session.State == SessionStateStarting {
		if _, err := h.registry.MarkRunning(sessionID); err != nil {
			_ = conn.Close()
			return
		}
	}
	h.workers.Register(sessionID, conn)
}
