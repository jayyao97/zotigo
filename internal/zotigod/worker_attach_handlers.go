package zotigod

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
)

func (h *handler) handleWorkerAttach(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req workerReadyRequest
	if err := readOptionalJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}
	req.Generation = strings.TrimSpace(req.Generation)
	if req.Generation == "" {
		writeAPIError(w, http.StatusBadRequest, "worker generation is required")
		return
	}
	if _, ok := h.registry.Get(id); !ok {
		h.writeTransition(w, Session{}, errSessionNotFound)
		return
	}
	if !h.workers.Matches(id, req.Generation) {
		waitCtx, cancel := context.WithTimeout(r.Context(), workerRegistrationWait)
		matched := h.workers.WaitForMatch(waitCtx.Done(), id, req.Generation)
		cancel()
		if !matched {
			writeAPIError(w, http.StatusConflict, "worker ready does not match the active connection")
			return
		}
	}

	unlock := h.sessionOps.lock(id)
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	session, ok := h.registry.Get(id)
	if !ok {
		h.writeTransition(w, Session{}, errSessionNotFound)
		return
	}
	if !h.workers.Matches(id, req.Generation) {
		writeAPIError(w, http.StatusConflict, "worker ready does not match the active connection")
		return
	}
	if zotigoruntime.AgentKind(session.Agent) == zotigoruntime.AgentCodex {
		if err := h.expireDisconnectedCodexRequests(r.Context(), id); err != nil {
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("expire stale Codex requests: %v", err))
			return
		}
	}
	if session.State == SessionStatePaused {
		var err error
		session, err = h.reconcileApprovalState(r.Context(), id, session)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("reconcile approval state: %v", err))
			return
		}
	}
	switch session.State {
	case SessionStateStarting:
		session, err := h.registry.MarkRunning(id)
		if err == nil {
			h.closeReadyWorkerWhenIdle(r.Context(), session, req.Generation)
		}
		unlock()
		locked = false
		if err == nil {
			h.dispatchSessionStart(session)
		}
		h.writeTransition(w, session, err)
		return
	case SessionStateRunning, SessionStatePausing:
		writeAPIJSON(w, http.StatusOK, session)
		h.closeReadyWorkerWhenIdle(r.Context(), session, req.Generation)
	case SessionStatePaused:
		writeAPIJSON(w, http.StatusOK, session)
	default:
		h.writeTransition(w, Session{}, errInvalidSessionTransition)
	}
}

func (h *handler) closeReadyWorkerWhenIdle(ctx context.Context, session Session, generation string) {
	items, _, err := h.items.LoadItems(ctx, session.ID)
	if err != nil || lastOpenTurnID(items) != "" || hasPendingMessageCommand(items) || hasPendingHumanRequest(items) {
		return
	}
	h.closeWorkerWhenIdle(session.ID, generation, workerIdle{CommandSequence: latestCommandSequence(items)})
}

func (h *handler) closeWorkerWhenIdle(sessionID string, generation string, idle workerIdle) bool {
	session, ok := h.registry.Get(sessionID)
	if !ok {
		return false
	}
	kind := zotigoruntime.AgentKind(session.Agent)
	if kind == "" {
		kind = zotigoruntime.AgentZotigo
	}
	adapter, err := h.runtimes.adapter(kind)
	if err != nil {
		return false
	}
	return h.workers.CloseWhenIdle(sessionID, generation, idle, adapter.WorkerLifecycle().IdleTimeout)
}

func (h *handler) handleWorkerDisconnect(id string, generation string) {
	unlock := h.sessionOps.lock(id)
	defer unlock()
	if !h.registry.WorkerGenerationMatches(id, generation) {
		return
	}
	if h.catalog != nil {
		if err := h.catalog.PauseQueuedInputs(context.Background(), id); err != nil && h.logger != nil {
			h.logger.Printf("pause queue after worker disconnect: session=%s error=%v", id, err)
		}
	}
	h.events.Retire(id, displayPreviewRetirement{Terminal: true})
	h.events.WakeBarrier(id)
	if session, ok := h.registry.Get(id); ok {
		if zotigoruntime.AgentKind(session.Agent) == zotigoruntime.AgentCodex {
			if err := h.expireDisconnectedCodexRequests(context.Background(), id); err != nil && h.logger != nil {
				h.logger.Printf("expire Codex requests on disconnect: session=%s error=%v", id, err)
			}
		}
		_, _ = h.reconcileApprovalState(context.Background(), id, session)
	}
	h.registry.MarkIdle(id)
	_, _ = h.registry.ResetStarting(id)
	h.registry.ClearWorkerGeneration(id, generation)
}

func (h *handler) reconcileApprovalState(ctx context.Context, id string, session Session) (Session, error) {
	if session.State != SessionStateStarting && session.State != SessionStateRunning && session.State != SessionStatePausing && session.State != SessionStatePaused {
		return session, nil
	}
	items, _, err := h.items.LoadItems(ctx, id)
	if err != nil {
		return Session{}, err
	}
	if hasPendingHumanRequest(items) {
		if session.State == SessionStatePaused {
			return session, nil
		}
		return h.registry.Pause(id)
	}
	if session.State == SessionStatePaused {
		return h.registry.ResumeAfterApproval(id)
	}
	return session, nil
}

func (h *handler) reconcileHumanRequestState(id string) {
	unlock := h.sessionOps.lockWorkerCallback(id)
	defer unlock()
	session, ok := h.registry.Get(id)
	if !ok {
		return
	}
	_, _ = h.reconcileApprovalState(context.Background(), id, session)
}

func (h *handler) handleWorkerFinish(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req finishSessionRequest
	if err := readOptionalJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}
	unlockApproval := h.approvalOps.lock(id)
	unlockSession := h.sessionOps.lock(id)
	if req.Generation != "" && !h.workers.Matches(id, req.Generation) {
		unlockSession()
		unlockApproval()
		writeAPIError(w, http.StatusConflict, "worker finish does not match the active connection")
		return
	}

	var session Session
	var err error
	if req.Error != "" {
		// An explicit runtime failure is authoritative even if a command was
		// accepted concurrently. The durable command remains available for the
		// failed-session retry path and diagnostics.
		session, err = h.registry.FailWithCode(id, req.ErrorCode, req.Error)
	} else {
		items, _, loadErr := h.items.LoadItems(r.Context(), id)
		switch {
		case loadErr != nil:
			err = fmt.Errorf("load session activity: %w", loadErr)
		case lastOpenTurnID(items) != "" || hasPendingMessageCommand(items) || hasPendingApproval(items):
			err = errInvalidSessionTransition
		default:
			session, err = h.registry.End(id)
		}
	}
	unlockSession()
	unlockApproval()
	if err == nil {
		h.workers.Close(id)
		h.dispatchSessionEnd(session)
	}
	h.writeTransition(w, session, err)
}
