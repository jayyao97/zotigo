package zotigod

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
)

type codexSettingsRequest struct {
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
}

type codexSettingsStore interface {
	UpdateCodexSettings(context.Context, string, string, string, time.Time) error
}

func (h *handler) handleSessionCodexSettings(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", http.MethodPut)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var request codexSettingsRequest
	if err := readRequiredJSON(r, &request); err != nil {
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}
	request.Model = strings.TrimSpace(request.Model)
	request.ReasoningEffort = strings.TrimSpace(request.ReasoningEffort)
	if request.Model == "" || request.ReasoningEffort == "" {
		writeAPIError(w, http.StatusBadRequest, "model and reasoning_effort are required")
		return
	}
	unlock := h.sessionOps.lock(id)
	defer unlock()
	session, live := h.registry.Get(id)
	if !live {
		var found bool
		var err error
		session, found, err = h.storedSession(r.Context(), id)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load session: %v", err))
			return
		}
		if !found {
			writeAPIError(w, http.StatusNotFound, "session not found")
			return
		}
	}
	if zotigoruntime.AgentKind(session.Agent) != zotigoruntime.AgentCodex {
		writeAPIError(w, http.StatusBadRequest, "codex settings require agent codex")
		return
	}
	if session.State == SessionStateEnded || session.State == SessionStateFailed {
		writeAPIError(w, http.StatusConflict, "codex settings require a resumable session")
		return
	}
	if err := h.validateCodexSettings(r.Context(), request.Model, request.ReasoningEffort); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	store, ok := h.store.(codexSettingsStore)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, "session store does not support codex settings")
		return
	}
	if err := store.UpdateCodexSettings(r.Context(), id, request.Model, request.ReasoningEffort, time.Now().UTC()); err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("update codex settings: %v", err))
		return
	}
	if live {
		updated, err := h.registry.UpdateCodexSettings(id, request.Model, request.ReasoningEffort)
		if err != nil {
			writeAPIError(w, http.StatusConflict, err.Error())
			return
		}
		session = updated
	} else {
		session.Model = request.Model
		session.ReasoningEffort = request.ReasoningEffort
	}
	writeAPIJSON(w, http.StatusOK, session)
}
