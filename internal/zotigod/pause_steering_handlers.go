package zotigod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	zotigoruntime "github.com/jayyao97/zotigo/internal/runtime"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

func requireIdleSession(items []zotigosession.DisplayItem) error {
	if sessionlog.LastOpenTurnID(items) != "" || hasPendingMessageCommand(items) {
		return errSessionBusy
	}
	return nil
}

func (h *handler) handleSessionPause(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	session, ok := h.registry.Get(id)
	if !ok {
		h.writeSessionNotLiveOrMissing(w, r.Context(), id, "pause requires a live session")
		return
	}
	if session.State != SessionStateRunning {
		if session.State == SessionStatePausing {
			writeAPIErrorCode(w, http.StatusConflict, "turn_stopping", "the active turn is already stopping")
			return
		}
		writeAPIErrorCode(w, http.StatusConflict, "no_active_turn", "pause requires a running session")
		return
	}
	var req pauseSessionRequest
	if err := readOptionalJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}

	command, err := h.pauseActiveSession(r.Context(), id, session, strings.TrimSpace(req.TurnID))
	if err != nil {
		switch {
		case errors.Is(err, errWorkerInputStopping):
			writeAPIErrorCode(w, http.StatusConflict, "turn_stopping", "the active turn is already stopping")
		case errors.Is(err, errPauseInputStopTimeout):
			writeAPIErrorCode(w, http.StatusGatewayTimeout, "input_stop_timeout", "timed out waiting for in-flight input to stop")
		case errors.Is(err, errApprovalPending):
			writeAPIErrorCode(w, http.StatusConflict, "approval_pending", "pause rejected while approval is pending")
		case errors.Is(err, errTurnMismatch):
			writeAPIErrorCode(w, http.StatusConflict, "turn_mismatch", "pause turn_id does not match active turn")
		case errors.Is(err, errNoActiveTurn), errors.Is(err, errSessionBusy), errors.Is(err, errInvalidSessionTransition):
			writeAPIErrorCode(w, http.StatusConflict, "no_active_turn", "pause requires an active turn")
		case errors.Is(err, errWorkerOffline):
			writeAPIError(w, http.StatusServiceUnavailable, "pause requires an online worker")
		default:
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("pause session: %v", err))
		}
		return
	}
	writeAPIJSON(w, http.StatusAccepted, publicCommandFromCommand(command))
}

var errPauseInputStopTimeout = errors.New("timed out waiting for in-flight input to stop")

// pauseActiveSession is the shared pause boundary for HTTP and authenticated
// channel controls. Callers must resolve the live session first so the same
// agent kind and turn are fenced throughout command creation.
func (h *handler) pauseActiveSession(ctx context.Context, id string, session Session, requestedTurnID string) (wire.Command, error) {
	items, _, err := h.items.LoadItems(ctx, id)
	if err != nil {
		return wire.Command{}, fmt.Errorf("load display items: %w", err)
	}
	turnID := sessionlog.LastOpenTurnID(items)
	if turnID == "" {
		return wire.Command{}, errNoActiveTurn
	}
	if requestedTurnID != "" && requestedTurnID != turnID {
		return wire.Command{}, errTurnMismatch
	}
	if sessionlog.HasPendingApprovalForTurn(items, turnID) {
		return wire.Command{}, errApprovalPending
	}
	if !h.ensureWorkerOnline(ctx, id) {
		return wire.Command{}, errWorkerOffline
	}
	items, _, err = h.items.LoadItems(ctx, id)
	if err != nil {
		return wire.Command{}, fmt.Errorf("reload display items: %w", err)
	}
	if sessionlog.LastOpenTurnID(items) != turnID {
		return wire.Command{}, errNoActiveTurn
	}
	if sessionlog.HasPendingApprovalForTurn(items, turnID) {
		return wire.Command{}, errApprovalPending
	}
	var interrupt *wire.InterruptTurn
	if zotigoruntime.AgentKind(session.Agent) == zotigoruntime.AgentCodex {
		stored, loadErr := h.store.Get(ctx, id)
		if loadErr != nil || stored == nil || stored.ConversationID == "" {
			return wire.Command{}, errors.New("load Codex conversation for pause")
		}
		interrupt = &wire.InterruptTurn{ThreadID: stored.ConversationID, TurnID: turnID}
	}
	drained, releaseInputStop, err := h.workers.BeginInputStop(id, interrupt)
	if err != nil {
		return wire.Command{}, err
	}
	defer releaseInputStop()
	waitCtx, cancelWait := context.WithTimeout(ctx, h.inputStopTimeout)
	defer cancelWait()
	select {
	case <-drained:
	case <-waitCtx.Done():
		return wire.Command{}, fmt.Errorf("%w: %v", errPauseInputStopTimeout, waitCtx.Err())
	}

	item, err := func() (zotigosession.DisplayItem, error) {
		unlock := h.sessionOps.lock(id)
		defer unlock()
		current, ok := h.registry.Get(id)
		if !ok {
			return zotigosession.DisplayItem{}, errSessionNotFound
		}
		if current.State != SessionStateRunning {
			return zotigosession.DisplayItem{}, errSessionBusy
		}
		if h.catalog != nil {
			if err := h.catalog.PauseQueuedInputs(ctx, id); err != nil {
				return zotigosession.DisplayItem{}, fmt.Errorf("pause input queue: %w", err)
			}
		}
		item, appendErr := h.appendPauseCommand(ctx, id, turnID)
		if appendErr != nil {
			return zotigosession.DisplayItem{}, appendErr
		}
		if _, transitionErr := h.registry.BeginStopping(id); transitionErr != nil {
			return zotigosession.DisplayItem{}, transitionErr
		}
		return item, nil
	}()
	if err != nil {
		return wire.Command{}, err
	}
	command := pauseCommandFromItem(item)
	h.sendCommand(ctx, id, command)
	return command, nil
}

func (h *handler) appendPauseCommand(ctx context.Context, id string, turnID string) (zotigosession.DisplayItem, error) {
	return h.items.AppendItemIf(ctx, id, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemSessionCommand,
		Command: &zotigosession.DisplayCommand{
			Type:   wire.CommandPause,
			TurnID: turnID,
			Reason: wire.UserPauseReason,
		},
	}, requireOpenTurnWithoutPendingApproval(turnID))
}

func (h *handler) handleSessionSteering(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req steeringRequest
	if err := readRequiredLimitedJSON(r, &req, maxMessageRequestBytes); err != nil {
		if errors.Is(err, errRequestBodyTooLarge) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, err.Error())
			return
		}
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}
	text := strings.TrimSpace(req.Text)
	images, err := validateMessageImages(req.Images)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if text == "" && len(images) == 0 {
		writeAPIError(w, http.StatusBadRequest, "steering requires text or images")
		return
	}
	expectedTurnID := strings.TrimSpace(req.ExpectedTurnID)
	legacyTurnID := strings.TrimSpace(req.TurnID)
	if expectedTurnID != "" && legacyTurnID != "" && expectedTurnID != legacyTurnID {
		writeAPIErrorCode(w, http.StatusBadRequest, "invalid_turn_precondition", "turn_id and expected_turn_id must match")
		return
	}
	if expectedTurnID == "" {
		expectedTurnID = legacyTurnID
	}
	if _, live := h.registry.Get(id); !live {
		_, stored, err := h.storedSession(r.Context(), id)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load session: %v", err))
			return
		}
		if !stored {
			writeAPIError(w, http.StatusNotFound, "session not found")
			return
		}
		writeAPIErrorCode(w, http.StatusConflict, "no_active_turn", "steering requires an active turn")
		return
	}
	if !h.workers.Has(id) {
		writeAPIErrorCode(w, http.StatusConflict, "no_active_turn", "steering requires an active turn")
		return
	}
	command, err := h.acceptSessionInputCommand(r.Context(), id, strings.TrimSpace(req.ClientMessageID), text, images, nil, expectedTurnID, true, false, nil)
	if err != nil {
		switch {
		case errors.Is(err, errWorkerInputStopping):
			writeAPIErrorCode(w, http.StatusConflict, "turn_stopping", "the active turn is stopping")
		case errors.Is(err, errNoActiveTurn):
			writeAPIErrorCode(w, http.StatusConflict, "no_active_turn", "steering requires an active turn")
		case errors.Is(err, errActiveTurn):
			writeAPIErrorCode(w, http.StatusConflict, "active_turn", err.Error())
		case errors.Is(err, errInputPolicyMismatch):
			writeAPIErrorCode(w, http.StatusConflict, "input_policy_mismatch", err.Error())
		case errors.Is(err, errWorkerOffline):
			writeAPIErrorCode(w, http.StatusConflict, "no_active_turn", "steering requires an active turn")
		case errors.Is(err, errTurnMismatch):
			writeAPIErrorCode(w, http.StatusConflict, "turn_mismatch", "expected_turn_id does not match active turn")
		case errors.Is(err, errCommandIDConflict):
			writeAPIErrorCode(w, http.StatusConflict, "command_id_conflict", err.Error())
		case errors.Is(err, errSessionNotFound):
			writeAPIError(w, http.StatusNotFound, "session not found")
		case errors.Is(err, errInvalidSessionTransition), errors.Is(err, errSessionUnavailable):
			writeAPIErrorCode(w, http.StatusConflict, "no_active_turn", "steering requires an active turn")
		default:
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("append steering command: %v", err))
		}
		return
	}
	writeAPIJSON(w, http.StatusCreated, publicCommandFromCommand(command))
}

func (h *handler) writeSessionNotLiveOrMissing(w http.ResponseWriter, ctx context.Context, id string, message string) {
	_, inStore, err := h.storedSession(ctx, id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load session: %v", err))
		return
	}
	if inStore {
		writeAPIErrorCode(w, http.StatusConflict, "session_not_live", message)
		return
	}
	writeAPIError(w, http.StatusNotFound, "session not found")
}

func requireOpenTurnWithoutPendingApproval(turnID string) func([]zotigosession.DisplayItem) error {
	return func(items []zotigosession.DisplayItem) error {
		openTurnID := sessionlog.LastOpenTurnID(items)
		if openTurnID != turnID && (openTurnID != "" || !hasTerminalTurn(items, turnID)) {
			return errSessionBusy
		}
		if sessionlog.HasPendingApprovalForTurn(items, turnID) {
			return errApprovalPending
		}
		return nil
	}
}

func hasTerminalTurn(items []zotigosession.DisplayItem, turnID string) bool {
	for index := len(items) - 1; index >= 0; index-- {
		item := items[index]
		if item.Turn == nil || item.Turn.ID != turnID {
			continue
		}
		switch item.Type {
		case zotigosession.DisplayItemTurnCompleted, zotigosession.DisplayItemTurnFailed, zotigosession.DisplayItemTurnInterrupted:
			return true
		}
	}
	return false
}
