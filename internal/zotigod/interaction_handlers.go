package zotigod

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
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

	// Serialize answers to each follow-up without holding the worker lifecycle lock during steering.
	lockKey := sessionID
	if strings.HasPrefix(interactionID, sessionlog.FollowupQuestionPrefix) {
		lockKey += "/" + interactionID
	}
	unlock := h.approvalOps.lock(lockKey)
	defer unlock()
	items, exists, err := h.items.LoadItems(r.Context(), sessionID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load interaction: %v", err))
		return
	}
	interaction, ok := sessionlog.InteractionFromItems(sessionID, interactionID, items)
	if !exists || !ok {
		writeAPIError(w, http.StatusNotFound, "interaction request not found")
		return
	}
	if interaction.ReplyAsMessage && interaction.Status == sessionlog.InteractionStatusResolved && maps.EqualFunc(body.Answers, interaction.Answers, slices.Equal[[]string]) {
		writeAPIJSON(w, http.StatusOK, interaction)
		return
	}
	if interaction.Status != sessionlog.InteractionStatusPending {
		writeAPIError(w, http.StatusConflict, "interaction request already resolved")
		return
	}
	if err := sessionlog.ValidateInteractionAnswers(interaction, body.Answers); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	if interaction.ReplyAsMessage {
		text := followupAnswerText(interaction.Questions, body.Answers)
		commandID := "reply-" + interaction.ID
		// Recover an accepted answer if its response record failed to persist.
		accepted := false
		for _, item := range items {
			if item.ID == commandID && item.Command != nil && item.Command.Type == wire.CommandSteering && item.Command.TurnID == interaction.TurnID {
				if item.Command.Text != text {
					writeAPIError(w, http.StatusConflict, "a different answer was already sent")
					return
				}
				accepted = true
				break
			}
		}
		if !accepted && (interaction.TurnID == "" || sessionlog.LastOpenTurnID(items) != interaction.TurnID) {
			writeAPIErrorCode(w, http.StatusConflict, "no_active_turn", "question turn has ended")
			return
		}
		if text != "" && !accepted {
			if _, ok := h.submitSteering(w, r, sessionID, commandID, interaction.TurnID, text, nil); !ok {
				return
			}
		}
		now := time.Now().UTC()
		interaction.Answers, interaction.Status, interaction.ResolvedAt = body.Answers, sessionlog.InteractionStatusResolved, &now
		var checkDismissal func([]zotigosession.DisplayItem) error
		if text == "" {
			checkDismissal = func(current []zotigosession.DisplayItem) error {
				if sessionlog.LastOpenTurnID(current) != interaction.TurnID {
					return sessionlog.ErrTurnMismatch
				}
				return nil
			}
		}
		if _, err := h.items.AppendItemIf(r.Context(), sessionID, sessionlog.InteractionItem(interaction, true), checkDismissal); errors.Is(err, sessionlog.ErrTurnMismatch) {
			writeAPIErrorCode(w, http.StatusConflict, "turn_mismatch", "question turn has ended")
			return
		} else if err != nil {
			writeAPIError(w, 500, "record question response: "+err.Error())
			return
		}
		writeAPIJSON(w, http.StatusOK, interaction)
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

func followupAnswerText(questions []wire.InteractionQuestion, answers map[string][]string) string {
	var lines []string
	for _, q := range questions {
		values := answers[q.ID]
		if len(values) == 0 {
			continue
		}
		parts := slices.Clone(values)
		last := len(parts) - 1
		// A selected option is literal text, even if the label begins with "user_note:".
		if last > 0 || !slices.ContainsFunc(q.Options, func(o wire.InteractionOption) bool { return o.Label == parts[last] }) {
			parts[last] = strings.TrimSpace(strings.TrimPrefix(parts[last], "user_note:"))
		}
		if last == 1 && parts[0] == "None of the above" {
			parts = parts[1:]
		}
		lines = append(lines, q.Question+"\n"+strings.Join(parts, "\n"))
	}
	return strings.Join(lines, "\n\n")
}
