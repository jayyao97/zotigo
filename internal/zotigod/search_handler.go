package zotigod

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	zotigosession "github.com/jayyao97/zotigo/core/session"
)

type sessionSearchStore interface {
	SearchDisplayMessages(context.Context, string, string, int) (zotigosession.DisplaySearchResult, error)
	DisplaySequence(context.Context, string, string) (uint64, error)
	ReadDisplayPage(context.Context, string, zotigosession.DisplayPageQuery, zotigosession.DisplayTimeWindow) (zotigosession.DisplayPage, bool, error)
}

func (h *handler) handleSessionSearch(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeAPIError(w, 405, "method not allowed")
		return
	}
	query := r.URL.Query().Get("q")
	if strings.TrimSpace(query) == "" || len([]rune(query)) > 256 {
		writeAPIError(w, 400, "q must contain 1 to 256 characters")
		return
	}
	store, ok := h.store.(sessionSearchStore)
	if !ok {
		writeAPIError(w, 503, "session storage unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	result, err := store.SearchDisplayMessages(ctx, id, query, 250)
	if err != nil {
		writeSearchError(w, err)
		return
	}
	writeAPIJSON(w, 200, result)
}

func writeSearchError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, os.ErrNotExist) {
		status = http.StatusNotFound
	}
	if errors.Is(err, zotigosession.ErrDisplayIndexPending) {
		status = http.StatusServiceUnavailable
		w.Header().Set("Retry-After", "2")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
	}
	writeAPIError(w, status, err.Error())
}

// Return a small independent history window, without loading all intervening
// messages or treating a gap as contiguous pagination in the client cache.
func (h *handler) handleSessionItemWindow(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeAPIError(w, 405, "method not allowed")
		return
	}
	store, ok := h.store.(sessionSearchStore)
	if !ok {
		writeAPIError(w, 503, "session storage unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	var sequence uint64
	var err error
	if messageID := r.URL.Query().Get("message_id"); messageID != "" {
		sequence, err = store.DisplaySequence(ctx, id, messageID)
		if errors.Is(err, sql.ErrNoRows) {
			writeAPIError(w, 404, "message not found")
			return
		}
		if err != nil {
			writeSearchError(w, err)
			return
		}
	} else {
		sequence, err = strconv.ParseUint(r.URL.Query().Get("sequence"), 10, 63)
		if err != nil || sequence == 0 {
			writeAPIError(w, 400, "sequence must be a positive integer")
			return
		}
	}
	before, exists, err := store.ReadDisplayPage(ctx, id, zotigosession.DisplayPageQuery{Limit: 10, HasBefore: true, Before: sequence, ContentKind: zotigosession.DisplayContentConversation}, zotigosession.DisplayTimeWindow{})
	if err != nil {
		writeSearchError(w, err)
		return
	}
	if !exists {
		writeAPIError(w, 404, "session not found")
		return
	}
	after, _, err := store.ReadDisplayPage(ctx, id, zotigosession.DisplayPageQuery{Limit: 11, HasAfter: true, After: sequence - 1, ContentKind: zotigosession.DisplayContentConversation}, zotigosession.DisplayTimeWindow{})
	if err != nil {
		writeSearchError(w, err)
		return
	}
	response := itemsResponse{Items: []itemResponse{}, PrevCursor: before.PrevCursor, NextCursor: after.NextCursor}
	for _, item := range append(before.Items, after.Items...) {
		response.Items = append(response.Items, publicDisplayItem(item))
	}
	response.HasMore = response.PrevCursor != "" || response.NextCursor != ""
	writeAPIJSON(w, 200, response)
}
