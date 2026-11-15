package zotigod

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

func (h *handler) handleWorkerTurnInterrupted(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := h.registry.Get(id); !ok {
		writeAPIError(w, http.StatusNotFound, "session not found")
		return
	}

	var req interruptTurnRequest
	if err := readRequiredJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}
	turnID := strings.TrimSpace(req.TurnID)
	if turnID == "" {
		writeAPIError(w, http.StatusBadRequest, "turn_id is required")
		return
	}
	items, _, err := h.items.LoadItems(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load display items: %v", err))
		return
	}
	if openTurnID := sessionlog.LastOpenTurnID(items); openTurnID == "" || openTurnID != turnID {
		writeAPIError(w, http.StatusConflict, "turn_id does not match active turn")
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		reason = wire.UserPauseReason
	}

	item, err := h.items.AppendItem(r.Context(), id, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnInterrupted,
		Turn: &zotigosession.DisplayTurn{
			ID:         turnID,
			Status:     "interrupted",
			Reason:     reason,
			DurationMS: req.DurationMS,
		},
	})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("append turn interrupted item: %v", err))
		return
	}

	writeAPIJSON(w, http.StatusCreated, publicDisplayItem(item))
}

func (h *handler) handleWorkerCommands(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := h.registry.Get(id); !ok {
		writeAPIError(w, http.StatusNotFound, "session not found")
		return
	}

	query, err := parseCommandQuery(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if query.HasOffset {
		resp, err := buildCommandsResponseFromOffset(r.Context(), h.items, id, query, h.sessionStoreRoot())
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load display items: %v", err))
			return
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	items, _, err := h.items.LoadItems(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load display items: %v", err))
		return
	}

	resp, err := buildCommandsResponse(items, query, h.sessionStoreRoot())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("build commands: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func parseCommandQuery(r *http.Request) (commandQuery, error) {
	values := r.URL.Query()
	query := commandQuery{Limit: defaultCommandsLimit}

	if raw := values.Get("after"); raw != "" {
		after, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return commandQuery{}, fmt.Errorf("invalid after cursor")
		}
		query.After = after
	}
	if raw := values.Get("offset"); raw != "" {
		offset, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || offset < 0 {
			return commandQuery{}, fmt.Errorf("invalid offset")
		}
		query.Offset = offset
		query.HasOffset = true
	}
	if query.HasOffset && query.After != 0 {
		return commandQuery{}, fmt.Errorf("after and offset cannot both be set")
	}

	if raw := values.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit <= 0 || limit > wire.MaxCommandsLimit {
			return commandQuery{}, fmt.Errorf("limit must be between 1 and %d", wire.MaxCommandsLimit)
		}
		query.Limit = limit
	}
	return query, nil
}

func (h *handler) ensureWorkerOnline(ctx context.Context, id string) bool {
	if h.workers.Has(id) {
		return true
	}
	if err := h.launchWorker(ctx, id); err != nil {
		return false
	}
	return h.waitForWorker(ctx, id)
}

func (h *handler) sendCommand(ctx context.Context, id string, command wire.Command) bool {
	if h.workers.Send(id, command) {
		return true
	}
	if err := h.launchWorker(ctx, id); err != nil {
		return false
	}
	if !h.waitForWorker(ctx, id) {
		return false
	}
	return h.workers.Send(id, command)
}
