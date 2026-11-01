package zotigod

import (
	"net/http"
)

func (h *handler) handleUpdates(w http.ResponseWriter, r *http.Request) {
	if h.updater == nil {
		writeAPIError(w, http.StatusNotImplemented, "This installation does not support application updates; install an update-capable managed release first.")
		return
	}
	if r.Method == http.MethodGet {
		status, err := h.updater.Status()
		if err != nil {
			writeAPIError(w, 500, err.Error())
			return
		}
		writeAPIJSON(w, 200, status)
		return
	}
	if r.Method != http.MethodPost {
		writeAPIError(w, 405, "method not allowed")
		return
	}
	var request struct {
		Action  string `json:"action"`
		Channel string `json:"channel"`
	}
	if err := readOptionalJSON(r, &request); err != nil {
		writeAPIError(w, 400, "invalid update request")
		return
	}
	var result any
	var err error
	switch request.Action {
	case "check":
		result, err = h.updater.Check(r.Context())
	case "channel":
		result, err = h.updater.SetChannel(request.Channel)
	case "prepare", "apply":
		result, err = h.updater.Start(request.Action)
	case "cancel":
		result, err = h.updater.CancelWaiting()
	default:
		writeAPIError(w, 400, "invalid update action")
		return
	}
	if err != nil {
		writeAPIError(w, 409, err.Error())
		return
	}
	writeAPIJSON(w, 200, result)
}

func (h *handler) handleUpdateDrain(w http.ResponseWriter, r *http.Request) {
	if h.updater == nil {
		writeAPIError(w, 501, "updates unavailable")
		return
	}
	if r.Method != http.MethodPost {
		writeAPIError(w, 405, "method not allowed")
		return
	}
	h.updateMu.Lock()
	defer h.updateMu.Unlock()
	status, err := h.updater.Status()
	if err != nil || status.Phase != "waiting" {
		writeAPIError(w, 409, "no update waiting to activate")
		return
	}
	if err := h.updater.BeginDrain(); err != nil {
		writeAPIError(w, 500, err.Error())
		return
	}
	for _, session := range h.registry.List() {
		if session.Working || session.ActiveTool != "" || session.State == SessionStateStarting || session.State == SessionStatePausing || session.State == SessionStatePaused {
			writeAPIJSON(w, 200, false)
			return
		}
		items, _, err := h.items.LoadItems(r.Context(), session.ID)
		if err != nil {
			writeAPIError(w, 500, "cannot verify session is idle")
			return
		}
		if lastOpenTurnID(items) != "" || hasPendingMessageCommand(items) || hasPendingHumanRequest(items) {
			writeAPIJSON(w, 200, false)
			return
		}
	}
	writeAPIJSON(w, 200, true)
}
