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
	session, err := h.changeSessionCodexSettings(r.Context(), id, request.Model, request.ReasoningEffort, false)
	if err != nil {
		writeSessionRuntimeError(w, err)
		return
	}
	writeAPIJSON(w, http.StatusOK, session)
}

// changeSessionCodexSettings switches a Codex Session's model and reasoning
// effort; the next turn uses them.
// changeSessionCodexSettings sets a Codex Session's model. requireIdle also
// rejects the change while the Session has work in progress or queued.
func (h *handler) changeSessionCodexSettings(ctx context.Context, id, model, reasoningEffort string, requireIdle bool) (Session, error) {
	model = strings.TrimSpace(model)
	reasoningEffort = strings.TrimSpace(reasoningEffort)
	if model == "" || reasoningEffort == "" {
		return Session{}, runtimeChangeError(http.StatusBadRequest, "model and reasoning_effort are required")
	}
	unlock := h.sessionOps.lock(id)
	defer unlock()
	session, live := h.registry.Get(id)
	if !live {
		var found bool
		var err error
		session, found, err = h.storedSession(ctx, id)
		if err != nil {
			return Session{}, runtimeChangeError(http.StatusInternalServerError, "load session: %v", err)
		}
		if !found {
			return Session{}, runtimeChangeError(http.StatusNotFound, "session not found")
		}
	}
	if zotigoruntime.AgentKind(session.Agent) != zotigoruntime.AgentCodex {
		return Session{}, runtimeChangeError(http.StatusBadRequest, "codex settings require agent codex")
	}
	if session.State == SessionStateEnded || session.State == SessionStateFailed {
		return Session{}, runtimeChangeError(http.StatusConflict, "codex settings require a resumable session")
	}
	if requireIdle {
		if err := h.requireIdleForRuntimeChange(ctx, id, session, "wait for the current Channel turn to finish before changing the model"); err != nil {
			return Session{}, err
		}
	}
	if err := h.validateCodexSettings(ctx, model, reasoningEffort); err != nil {
		return Session{}, runtimeChangeError(http.StatusBadRequest, "%s", err.Error())
	}
	store, ok := h.store.(codexSettingsStore)
	if !ok {
		return Session{}, runtimeChangeError(http.StatusInternalServerError, "session store does not support codex settings")
	}
	if err := store.UpdateCodexSettings(ctx, id, model, reasoningEffort, time.Now().UTC()); err != nil {
		return Session{}, runtimeChangeError(http.StatusInternalServerError, "update codex settings: %v", err)
	}
	if live {
		updated, err := h.registry.UpdateCodexSettings(id, model, reasoningEffort)
		if err != nil {
			return Session{}, runtimeChangeError(http.StatusConflict, "%s", err.Error())
		}
		return updated, nil
	}
	session.Model = model
	session.ReasoningEffort = reasoningEffort
	return session, nil
}
