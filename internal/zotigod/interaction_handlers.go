package zotigod

import (
	"fmt"
	"net/http"
)

type submitInteractionResponseRequest struct {
	Answers map[string][]string `json:"answers"`
}

func (h *handler) handleInteractionResponse(w http.ResponseWriter, r *http.Request, sessionID, interactionID string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body submitInteractionResponseRequest
	if err := readRequiredJSON(r, &body); err != nil {
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}

	unlock := h.approvalOps.lock(sessionID)
	defer unlock()
	items, exists, err := h.items.LoadItems(r.Context(), sessionID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load interaction: %v", err))
		return
	}
	interaction, ok := interactionFromDisplayItems(sessionID, interactionID, items)
	if !exists || !ok {
		writeAPIError(w, http.StatusNotFound, "interaction request not found")
		return
	}
	if interaction.Status != interactionStatusPending {
		writeAPIError(w, http.StatusConflict, "interaction request already resolved")
		return
	}
	if err := validateInteractionAnswers(interaction, body.Answers); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, live := h.registry.Get(sessionID); !live {
		h.writeSessionNotLiveOrMissing(w, r.Context(), sessionID, "interaction response requires a live session")
		return
	}
	resolved, err := h.workers.SubmitInteraction(r.Context(), sessionID, interactionID, body.Answers)
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, fmt.Sprintf("submit interaction response: %v", err))
		return
	}
	writeAPIJSON(w, http.StatusOK, resolved)
}
