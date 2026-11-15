package sessionlog

import (
	"strings"
	"sync"

	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

type EventBroker struct {
	mu          sync.Mutex
	subscribers map[string]map[chan BrokerEvent]struct{}
	previews    map[string][]*displayPreview
}

const displayEventSubscriberQueue = 32

func NewEventBroker() *EventBroker {
	return &EventBroker{subscribers: make(map[string]map[chan BrokerEvent]struct{}), previews: make(map[string][]*displayPreview)}
}

// Only unfinished blocks are retained. Completed content belongs to the durable log.
type displayPreview struct {
	delta wire.DisplayDelta
	text  strings.Builder
}

func PreviewRetirement(item zotigosession.DisplayItem) wire.PreviewRetirement {
	retirement := wire.PreviewRetirement{ItemID: item.ID}
	retirement.Terminal = item.Subagent == nil && (item.Type == zotigosession.DisplayItemTurnCompleted || item.Type == zotigosession.DisplayItemTurnFailed || item.Type == zotigosession.DisplayItemTurnInterrupted)
	for _, part := range item.Content {
		if part.ToolResult != nil {
			retirement.ToolCallIDs = append(retirement.ToolCallIDs, part.ToolResult.ToolCallID)
		}
	}
	return retirement
}

func (b *EventBroker) Retire(sessionID string, retirement wire.PreviewRetirement) {
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

func (b *EventBroker) HasPreview(sessionID, itemID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, preview := range b.previews[sessionID] {
		if preview.delta.ItemID == itemID {
			return true
		}
	}
	return false
}

type BrokerEvent struct {
	Delta *wire.DisplayDelta
}

func (b *EventBroker) Subscribe(sessionID string) (<-chan BrokerEvent, func()) {
	events, _, unsubscribe := b.SubscribeWithPreviews(sessionID)
	return events, unsubscribe
}

// Capture the prefix and register for later deltas under the same lock, so
// switching sessions cannot leave a gap or duplicate the subscription boundary.
func (b *EventBroker) SubscribeWithPreviews(sessionID string) (<-chan BrokerEvent, []wire.DisplayDelta, func()) {
	events := make(chan BrokerEvent, displayEventSubscriberQueue)
	b.mu.Lock()
	var previews []wire.DisplayDelta
	for _, preview := range b.previews[sessionID] {
		delta := preview.delta
		delta.Delta = preview.text.String()
		previews = append(previews, delta)
	}
	if b.subscribers[sessionID] == nil {
		b.subscribers[sessionID] = make(map[chan BrokerEvent]struct{})
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

func (b *EventBroker) Wake(sessionID string) {
	b.publish(sessionID, BrokerEvent{})
}

func (b *EventBroker) WakeBarrier(sessionID string) {
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
		subscriber <- BrokerEvent{}
	}
}

func (b *EventBroker) PublishDelta(sessionID string, delta wire.DisplayDelta) {
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
	b.publishLocked(sessionID, BrokerEvent{Delta: &delta})
}

func (b *EventBroker) publish(sessionID string, event BrokerEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.publishLocked(sessionID, event)
}

func (b *EventBroker) publishLocked(sessionID string, event BrokerEvent) {
	for subscriber := range b.subscribers[sessionID] {
		select {
		case subscriber <- event:
		default:
		}
	}
}

// SubscriberCount reports how many subscriptions are open for a session.
func (b *EventBroker) SubscriberCount(sessionID string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subscribers[sessionID])
}
