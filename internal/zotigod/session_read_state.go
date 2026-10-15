package zotigod

import (
	"context"
	"net/http"

	zotigosession "github.com/jayyao97/zotigo/core/session"
)

type sessionReadStore interface {
	SessionReadState(context.Context, string) (zotigosession.ReadState, error)
	ListSessionReadStates(context.Context) (map[string]zotigosession.ReadState, error)
	SetSessionReadState(context.Context, string, *int64) (zotigosession.ReadState, error)
}

func (h *handler) handleSessionReadState(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", http.MethodPut)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	store, ok := h.store.(sessionReadStore)
	if !ok {
		writeAPIError(w, http.StatusServiceUnavailable, "session read state unavailable")
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
	var request struct {
		ReadVersion *int64 `json:"read_version"`
		Unread      bool   `json:"unread"`
	}
	if err := readRequiredJSON(r, &request); err != nil || (request.ReadVersion == nil) != request.Unread || request.ReadVersion != nil && (*request.ReadVersion < 0 || *request.ReadVersion > 9007199254740991) {
		writeAPIError(w, http.StatusBadRequest, "provide read_version or unread: true")
		return
	}
	state, err := store.SetSessionReadState(r.Context(), id, request.ReadVersion)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "save read state failed")
		return
	}
	writeAPIJSON(w, http.StatusOK, state)
}

func (h *handler) writeSessionWithReadState(w http.ResponseWriter, r *http.Request, session Session) {
	if store, ok := h.store.(sessionReadStore); ok {
		state, err := store.SessionReadState(r.Context(), session.ID)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, "load read state failed")
			return
		}
		session.ReadState = &state
	}
	writeAPIJSON(w, http.StatusOK, session)
}
