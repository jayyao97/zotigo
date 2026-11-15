package zotigod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

const (
	displayEventBatchSize       = 200
	displayEventCatchUpInterval = 250 * time.Millisecond
	displayEventHeartbeat       = 15 * time.Second
	displayEventWriteTimeout    = 5 * time.Second
)

type displayEventCursor struct {
	sequence uint64
	offset   int64
}

func (h *handler) handleSessionEvents(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	after, err := parseDisplayEventCursor(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAPIError(w, http.StatusInternalServerError, "streaming is not supported")
		return
	}

	events, previews, unsubscribe := h.events.SubscribeWithPreviews(id)
	defer unsubscribe()
	_, inRegistry := h.registry.Get(id)
	inStore, err := displayEventSessionExists(r.Context(), h.items, id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("load display items: %v", err))
		return
	}
	if !inRegistry && !inStore {
		writeAPIError(w, http.StatusNotFound, "session not found")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	if err := writeDisplayEventComment(w, flusher, "connected"); err != nil {
		return
	}

	cursor := displayEventCursor{sequence: after}
	catchUp := func() bool {
		exists, err := catchUpDisplayEvents(r.Context(), w, flusher, h.items, h.events, id, &cursor, r.Header.Get("X-Zotigo-Runtime-Updates") == "true")
		return err == nil && exists
	}
	if !catchUp() {
		return
	}

	// The UI reconciles these previews by item ID against the history above.
	// A block completed during catch-up is therefore never shown twice.
	for _, preview := range previews {
		if !h.events.HasPreview(id, preview.ItemID) {
			continue
		}
		if err := writeDisplayEventDelta(w, flusher, preview); err != nil {
			return
		}
	}

	catchUpTicker := time.NewTicker(displayEventCatchUpInterval)
	heartbeatTicker := time.NewTicker(displayEventHeartbeat)
	defer catchUpTicker.Stop()
	defer heartbeatTicker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case event := <-events:
			if event.Delta != nil {
				if err := writeDisplayEventDelta(w, flusher, *event.Delta); err != nil {
					return
				}
			} else if !catchUp() {
				return
			}
		case <-catchUpTicker.C:
			if !catchUp() {
				return
			}
		case <-heartbeatTicker.C:
			if err := writeDisplayEventComment(w, flusher, "heartbeat"); err != nil {
				return
			}
		}
	}
}

func displayEventSessionExists(ctx context.Context, source sessionlog.ItemSource, sessionID string) (bool, error) {
	if source, ok := source.(sessionlog.OffsetItemSource); ok {
		_, exists, _, err := source.LoadItemsFromOffset(ctx, sessionID, 0, 1)
		return exists, err
	}
	_, exists, err := source.LoadItems(ctx, sessionID)
	return exists, err
}

func catchUpDisplayEvents(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, source sessionlog.ItemSource, broker *sessionlog.EventBroker, sessionID string, cursor *displayEventCursor, runtimeUpdates ...bool) (bool, error) {
	if source, ok := source.(sessionlog.OffsetItemSource); ok {
		for {
			items, exists, nextOffset, err := source.LoadItemsFromOffset(ctx, sessionID, cursor.offset, displayEventBatchSize)
			if err != nil || !exists {
				return exists, err
			}
			for _, item := range items {
				retirement := sessionlog.PreviewRetirement(item)
				// Historical terminal events must not clear a newer generation.
				retirement.Terminal = false
				broker.Retire(sessionID, retirement)
				if item.Sequence <= cursor.sequence {
					continue
				}
				if !isPublicDisplayItem(item, runtimeUpdates...) {
					cursor.sequence = item.Sequence
					continue
				}
				if err := writeDisplayEventItem(w, flusher, item); err != nil {
					return true, err
				}
				cursor.sequence = item.Sequence
			}
			previousOffset := cursor.offset
			cursor.offset = nextOffset
			if nextOffset == previousOffset || len(items) < displayEventBatchSize {
				return true, nil
			}
		}
	}

	items, exists, err := source.LoadItems(ctx, sessionID)
	if err != nil || !exists {
		return exists, err
	}
	for _, item := range items {
		retirement := sessionlog.PreviewRetirement(item)
		// Historical terminal events must not clear a newer generation.
		retirement.Terminal = false
		broker.Retire(sessionID, retirement)
		if item.Sequence <= cursor.sequence {
			continue
		}
		if !isPublicDisplayItem(item, runtimeUpdates...) {
			cursor.sequence = item.Sequence
			continue
		}
		if err := writeDisplayEventItem(w, flusher, item); err != nil {
			return true, err
		}
		cursor.sequence = item.Sequence
	}
	return true, nil
}

func parseDisplayEventCursor(r *http.Request) (uint64, error) {
	if values, ok := r.URL.Query()["after"]; ok {
		if len(values) != 1 {
			return 0, fmt.Errorf("invalid after cursor")
		}
		cursor, err := parseDisplayCursor(values[0])
		if err != nil {
			return 0, fmt.Errorf("invalid after cursor")
		}
		return cursor, nil
	}
	raw := strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	if raw == "" {
		return 0, nil
	}
	cursor, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || cursor == 0 {
		return 0, fmt.Errorf("invalid Last-Event-ID")
	}
	return cursor, nil
}

func writeDisplayEventItem(w http.ResponseWriter, flusher http.Flusher, item zotigosession.DisplayItem) error {
	data, err := sonic.Marshal(publicDisplayItem(item))
	if err != nil {
		return err
	}
	if err := setDisplayEventWriteDeadline(w); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "id: %d\nevent: item\ndata: %s\n\n", item.Sequence, data); err != nil {
		return err
	}
	flusher.Flush()
	return clearDisplayEventWriteDeadline(w)
}

func writeDisplayEventDelta(w http.ResponseWriter, flusher http.Flusher, delta wire.DisplayDelta) error {
	data, err := sonic.Marshal(delta)
	if err != nil {
		return err
	}
	if err := setDisplayEventWriteDeadline(w); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: delta\ndata: %s\n\n", data); err != nil {
		return err
	}
	flusher.Flush()
	return clearDisplayEventWriteDeadline(w)
}

func writeDisplayEventComment(w http.ResponseWriter, flusher http.Flusher, comment string) error {
	if err := setDisplayEventWriteDeadline(w); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, ": %s\n\n", comment); err != nil {
		return err
	}
	flusher.Flush()
	return clearDisplayEventWriteDeadline(w)
}

func setDisplayEventWriteDeadline(w http.ResponseWriter) error {
	err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(displayEventWriteTimeout))
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

func clearDisplayEventWriteDeadline(w http.ResponseWriter) error {
	err := http.NewResponseController(w).SetWriteDeadline(time.Time{})
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}
