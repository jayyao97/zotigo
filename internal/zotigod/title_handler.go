package zotigod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jayyao97/zotigo/core/config"
	"github.com/jayyao97/zotigo/core/debug"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/providers"
	"github.com/jayyao97/zotigo/core/services"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

const defaultTitleSuggestionTimeout = 15 * time.Second

var errTitleSourceNotReady = errors.New("no turn has completed successfully")

type titleSuggestionFunc func(context.Context, config.ProfileConfig, string, string) (string, error)

func generateTitleSuggestion(ctx context.Context, profile config.ProfileConfig, userMessage string, assistantMessage string) (string, error) {
	provider, err := providers.NewProvider(profile)
	if err != nil {
		return "", fmt.Errorf("create title provider: %w", err)
	}
	return services.GenerateTitle(ctx, provider, userMessage, assistantMessage)
}

func (h *handler) handleSessionTitleSuggestion(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	session, ok, err := h.sessionForTitleSuggestion(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load session: %v", err))
		return
	}
	if !ok {
		writeAPIError(w, http.StatusNotFound, "session not found")
		return
	}
	items, exists, err := h.items.LoadItems(r.Context(), id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load display items: %v", err))
		return
	}
	if !exists {
		writeAPIError(w, http.StatusConflict, errTitleSourceNotReady.Error())
		return
	}
	userMessage, assistantMessage, err := firstCompletedTurnTitleSource(items)
	if err != nil {
		writeAPIError(w, http.StatusConflict, err.Error())
		return
	}

	workingDirectory := session.WorkingDirectory
	if workingDirectory == "" {
		workingDirectory = h.sessionWorkingDirectory(r.Context(), id)
	}
	appConfig, err := config.NewManager().LoadForDir(workingDirectory)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load title profile configuration: %v", err))
		return
	}
	_, profile, err := appConfig.ResolveProfile(session.ProfileName)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("resolve title profile: %v", err))
		return
	}

	titleCtx, cancel := context.WithTimeout(r.Context(), h.titleTimeout)
	defer cancel()
	title, err := h.titleSuggestion(titleCtx, profile, userMessage, assistantMessage)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(titleCtx.Err(), context.DeadlineExceeded) {
			debug.Logf("title suggestion timed out session=%s provider=%s model=%s", id, profile.Provider, profile.Model)
			writeAPIError(w, http.StatusGatewayTimeout, "title generation timed out")
			return
		}
		debug.Logf("title suggestion failed session=%s provider=%s model=%s error_type=%T", id, profile.Provider, profile.Model, err)
		writeAPIError(w, http.StatusBadGateway, "title generation failed")
		return
	}
	writeAPIJSON(w, http.StatusOK, map[string]string{"title": title})
}

func (h *handler) sessionForTitleSuggestion(ctx context.Context, id string) (Session, bool, error) {
	if session, ok := h.registry.Get(id); ok {
		session, err := h.sessionWithStoredProfile(ctx, session)
		return session, true, err
	}
	return h.storedSession(ctx, id)
}

func firstCompletedTurnTitleSource(items []zotigosession.DisplayItem) (string, string, error) {
	userMessage := ""
	foundUserMessage := false
	turnID := ""
	turnUserMessage := ""
	assistantMessage := ""

	for _, item := range items {
		if item.Type == zotigosession.DisplayItemUserMessage {
			userMessage = displayText(item.Content)
			foundUserMessage = true
			continue
		}
		if turnID == "" {
			if foundUserMessage && item.Type == zotigosession.DisplayItemTurnStarted && item.Turn != nil && item.Turn.ID != "" {
				turnID = item.Turn.ID
				turnUserMessage = userMessage
				assistantMessage = ""
			}
			continue
		}

		switch item.Type {
		case zotigosession.DisplayItemAssistantMessage:
			if text := displayText(item.Content); text != "" {
				assistantMessage = text
			}
		case zotigosession.DisplayItemTurnCompleted:
			if sameDisplayTurn(item, turnID) {
				if turnUserMessage == "" && assistantMessage == "" {
					return "", "", errors.New("first completed turn has no usable text")
				}
				return turnUserMessage, assistantMessage, nil
			}
		case zotigosession.DisplayItemTurnFailed, zotigosession.DisplayItemTurnInterrupted:
			if sameDisplayTurn(item, turnID) {
				turnID = ""
				turnUserMessage = ""
				assistantMessage = ""
			}
		}
	}
	return "", "", errTitleSourceNotReady
}

func displayText(content []zotigosession.DisplayContentPart) string {
	parts := make([]string, 0, len(content))
	for _, part := range content {
		if part.Type == string(protocol.ContentTypeText) && strings.TrimSpace(part.Text) != "" {
			parts = append(parts, strings.TrimSpace(part.Text))
		}
	}
	return strings.Join(parts, "\n")
}

func sameDisplayTurn(item zotigosession.DisplayItem, turnID string) bool {
	return item.Turn != nil && item.Turn.ID == turnID
}
