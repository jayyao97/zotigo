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
	displayWakeTimeout          = 500 * time.Millisecond
	displayEventSubscriberQueue = 32
)

type displayEventBroker struct {
	mu          sync.Mutex
	subscribers map[string]map[chan displayBrokerEvent]struct{}
}

func newDisplayEventBroker() *displayEventBroker {
	return &displayEventBroker{subscribers: make(map[string]map[chan displayBrokerEvent]struct{})}
}

type displayDeltaEvent struct {
	ItemID   string `json:"item_id"`
	Role     string `json:"role"`
	PartType string `json:"part_type"`
	Delta    string `json:"delta"`
}

type displayBrokerEvent struct {
	delta *displayDeltaEvent
}

func (b *displayEventBroker) Subscribe(sessionID string) (<-chan displayBrokerEvent, func()) {
	events := make(chan displayBrokerEvent, displayEventSubscriberQueue)
	b.mu.Lock()
	if b.subscribers[sessionID] == nil {
		b.subscribers[sessionID] = make(map[chan displayBrokerEvent]struct{})
	}
	b.subscribers[sessionID][events] = struct{}{}
	b.mu.Unlock()
	return events, func() {
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

func (b *displayEventBroker) PublishDelta(sessionID string, delta displayDeltaEvent) {
	b.publish(sessionID, displayBrokerEvent{delta: &delta})
}

func (b *displayEventBroker) publish(sessionID string, event displayBrokerEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
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

	events, unsubscribe := h.events.Subscribe(id)
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
		exists, err := catchUpDisplayEvents(r.Context(), w, flusher, h.items, id, &cursor)
		return err == nil && exists
	}
	if !catchUp() {
		return
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

func catchUpDisplayEvents(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, source displayItemSource, sessionID string, cursor *displayEventCursor) (bool, error) {
	if source, ok := source.(offsetDisplayItemSource); ok {
		for {
			items, exists, nextOffset, err := source.LoadItemsFromOffset(ctx, sessionID, cursor.offset, displayEventBatchSize)
			if err != nil || !exists {
				return exists, err
			}
			for _, item := range items {
				if item.Sequence <= cursor.sequence {
					continue
				}
				if !isPublicDisplayItem(item) {
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
		if item.Sequence <= cursor.sequence {
			continue
		}
		if !isPublicDisplayItem(item) {
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

type displayWakeRequest struct {
	Generation string `json:"generation"`
}

type displayWakeNotifier struct {
	client     *http.Client
	daemonURL  string
	sessionID  string
	generation string
	wake       chan struct{}
	done       chan struct{}
	closeOnce  sync.Once
	wait       sync.WaitGroup
}

func newDisplayWakeNotifier(client *http.Client, daemonURL string, sessionID string, generation string) *displayWakeNotifier {
	notifier := &displayWakeNotifier{
		client:     client,
		daemonURL:  daemonURL,
		sessionID:  sessionID,
		generation: generation,
		wake:       make(chan struct{}, 1),
		done:       make(chan struct{}),
	}
	notifier.wait.Add(1)
	go notifier.run()
	return notifier
}

func (n *displayWakeNotifier) Wake(context.Context) {
	select {
	case <-n.done:
		return
	default:
	}
	select {
	case n.wake <- struct{}{}:
	case <-n.done:
	default:
	}
}

func (n *displayWakeNotifier) Close() {
	n.closeOnce.Do(func() { close(n.done) })
	n.wait.Wait()
}

func (n *displayWakeNotifier) run() {
	defer n.wait.Done()
	for {
		select {
		case <-n.done:
			return
		case <-n.wake:
			select {
			case <-n.done:
				return
			default:
			}
			ctx, cancel := context.WithTimeout(context.Background(), displayWakeTimeout)
			_ = reportWorkerDisplayWake(ctx, n.client, n.daemonURL, n.sessionID, n.generation)
			cancel()
		}
	}
}

func (h *handler) handleWorkerDisplayWake(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAPIError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var req displayWakeRequest
	if err := readRequiredJSON(r, &req); err != nil {
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("decode request: %v", err))
		return
	}
	req.Generation = strings.TrimSpace(req.Generation)
	if req.Generation == "" {
		writeAPIError(w, http.StatusBadRequest, "worker generation is required")
		return
	}
	if _, ok := h.registry.Get(id); !ok {
		writeAPIError(w, http.StatusNotFound, "session not found")
		return
	}
	if !h.workers.Matches(id, req.Generation) {
		writeAPIError(w, http.StatusConflict, "display wake does not match the active connection")
		return
	}
	h.events.Wake(id)
	w.WriteHeader(http.StatusNoContent)
}
