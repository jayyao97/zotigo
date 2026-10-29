package zotigod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

const (
	displayEventBatchSize       = 200
	displayEventCatchUpInterval = 250 * time.Millisecond
	displayEventHeartbeat       = 15 * time.Second
	displayEventWriteTimeout    = 5 * time.Second
	displayEventSubscriberQueue = 32
)

type displayEventBroker struct {
	mu          sync.Mutex
	subscribers map[string]map[chan displayBrokerEvent]struct{}
	previews    map[string][]*displayPreview
}

func newDisplayEventBroker() *displayEventBroker {
	return &displayEventBroker{subscribers: make(map[string]map[chan displayBrokerEvent]struct{}), previews: make(map[string][]*displayPreview)}
}

type displayDeltaEvent struct {
	BeforeInputID string                         `json:"before_input_id,omitempty"`
	ItemID        string                         `json:"item_id"`
	Role          string                         `json:"role"`
	PartType      string                         `json:"part_type"`
	Delta         string                         `json:"delta"`
	ToolCallID    string                         `json:"tool_call_id,omitempty"`
	ToolName      string                         `json:"tool_name,omitempty"`
	Subagent      *zotigosession.DisplaySubagent `json:"subagent,omitempty"`
}

// Only unfinished blocks are retained. Completed content belongs to the durable log.
type displayPreview struct {
	delta displayDeltaEvent
	text  strings.Builder
}

type displayPreviewRetirement struct {
	ItemID      string   `json:"item_id"`
	ToolCallIDs []string `json:"tool_call_ids,omitempty"`
	Terminal    bool     `json:"terminal,omitempty"`
}

func previewRetirement(item zotigosession.DisplayItem) displayPreviewRetirement {
	retirement := displayPreviewRetirement{ItemID: item.ID}
	retirement.Terminal = item.Subagent == nil && (item.Type == zotigosession.DisplayItemTurnCompleted || item.Type == zotigosession.DisplayItemTurnFailed || item.Type == zotigosession.DisplayItemTurnInterrupted)
	for _, part := range item.Content {
		if part.ToolResult != nil {
			retirement.ToolCallIDs = append(retirement.ToolCallIDs, part.ToolResult.ToolCallID)
		}
	}
	return retirement
}

func (b *displayEventBroker) Retire(sessionID string, retirement displayPreviewRetirement) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if retirement.Terminal {
		delete(b.previews, sessionID)
		return
	}
	var remaining []*displayPreview
	for _, preview := range b.previews[sessionID] {
		completed := preview.delta.ItemID == retirement.ItemID
		for _, id := range retirement.ToolCallIDs {
			if id != "" && (preview.delta.ToolCallID == id || preview.delta.Subagent != nil && preview.delta.Subagent.ToolCallID == id) {
				completed = true
			}
		}
		if !completed {
			remaining = append(remaining, preview)
		}
	}
	if len(remaining) == 0 {
		delete(b.previews, sessionID)
	} else {
		b.previews[sessionID] = remaining
	}
}

func (b *displayEventBroker) hasPreview(sessionID, itemID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, preview := range b.previews[sessionID] {
		if preview.delta.ItemID == itemID {
			return true
		}
	}
	return false
}

type displayBrokerEvent struct {
	delta *displayDeltaEvent
}

func (b *displayEventBroker) Subscribe(sessionID string) (<-chan displayBrokerEvent, func()) {
	events, _, unsubscribe := b.subscribeWithPreviews(sessionID)
	return events, unsubscribe
}

// Capture the prefix and register for later deltas under the same lock, so
// switching sessions cannot leave a gap or duplicate the subscription boundary.
func (b *displayEventBroker) subscribeWithPreviews(sessionID string) (<-chan displayBrokerEvent, []displayDeltaEvent, func()) {
	events := make(chan displayBrokerEvent, displayEventSubscriberQueue)
	b.mu.Lock()
	var previews []displayDeltaEvent
	for _, preview := range b.previews[sessionID] {
		delta := preview.delta
		delta.Delta = preview.text.String()
		previews = append(previews, delta)
	}
	if b.subscribers[sessionID] == nil {
		b.subscribers[sessionID] = make(map[chan displayBrokerEvent]struct{})
	}
	b.subscribers[sessionID][events] = struct{}{}
	b.mu.Unlock()
	return events, previews, func() {
		b.mu.Lock()
		delete(b.subscribers[sessionID], events)
		if len(b.subscribers[sessionID]) == 0 {
			delete(b.subscribers, sessionID)
		}
		b.mu.Unlock()
	}
}

func (b *displayEventBroker) Wake(sessionID string) {
	b.publish(sessionID, displayBrokerEvent{})
}

func (b *displayEventBroker) WakeBarrier(sessionID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for subscriber := range b.subscribers[sessionID] {
	drainSubscriber:
		for {
			select {
			case <-subscriber:
			default:
				break drainSubscriber
			}
		}
		subscriber <- displayBrokerEvent{}
	}
}

func (b *displayEventBroker) PublishDelta(sessionID string, delta displayDeltaEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var preview *displayPreview
	for _, candidate := range b.previews[sessionID] {
		if candidate.delta.ItemID == delta.ItemID {
			preview = candidate
			break
		}
	}
	if preview == nil && delta.Delta != "" && delta.PartType != "tool_progress" {
		preview = &displayPreview{delta: delta}
		preview.delta.Delta = ""
		b.previews[sessionID] = append(b.previews[sessionID], preview)
	}
	if preview != nil {
		preview.text.WriteString(delta.Delta)
		if delta.BeforeInputID != "" {
			preview.delta.BeforeInputID = delta.BeforeInputID
		}
	}
	b.publishLocked(sessionID, displayBrokerEvent{delta: &delta})
}

func (b *displayEventBroker) publish(sessionID string, event displayBrokerEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.publishLocked(sessionID, event)
}

func (b *displayEventBroker) publishLocked(sessionID string, event displayBrokerEvent) {
	for subscriber := range b.subscribers[sessionID] {
		select {
		case subscriber <- event:
		default:
		}
	}
}

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

	events, previews, unsubscribe := h.events.subscribeWithPreviews(id)
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
		if !h.events.hasPreview(id, preview.ItemID) {
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
			if event.delta != nil {
				if err := writeDisplayEventDelta(w, flusher, *event.delta); err != nil {
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

func displayEventSessionExists(ctx context.Context, source displayItemSource, sessionID string) (bool, error) {
	if source, ok := source.(offsetDisplayItemSource); ok {
		_, exists, _, err := source.LoadItemsFromOffset(ctx, sessionID, 0, 1)
		return exists, err
	}
	_, exists, err := source.LoadItems(ctx, sessionID)
	return exists, err
}

func catchUpDisplayEvents(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, source displayItemSource, broker *displayEventBroker, sessionID string, cursor *displayEventCursor, runtimeUpdates ...bool) (bool, error) {
	if source, ok := source.(offsetDisplayItemSource); ok {
		for {
			items, exists, nextOffset, err := source.LoadItemsFromOffset(ctx, sessionID, cursor.offset, displayEventBatchSize)
			if err != nil || !exists {
				return exists, err
			}
			for _, item := range items {
				retirement := previewRetirement(item)
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
		retirement := previewRetirement(item)
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

func writeDisplayEventDelta(w http.ResponseWriter, flusher http.Flusher, delta displayDeltaEvent) error {
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
