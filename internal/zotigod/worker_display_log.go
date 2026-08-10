package zotigod

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

type workerDisplayLog struct {
	sessionID string
	items     displayItemSource
	wake      func(context.Context)
	delta     func(displayDeltaEvent)

	mu          sync.Mutex
	turnID      string
	turnStarted time.Time
	block       *workerDisplayBlock
	toolCalls   map[string]chan struct{}
	toolCallErr map[string]error
}

type workerDisplayBlock struct {
	id       string
	index    int
	partType string
	text     string
}

func newWorkerDisplayLog(sessionID string, items displayItemSource) *workerDisplayLog {
	return &workerDisplayLog{sessionID: sessionID, items: items}
}

func (l *workerDisplayLog) StartTurn(ctx context.Context) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.turnStarted = time.Now()
	l.turnID = fmt.Sprintf("turn_%d", l.turnStarted.UnixNano())
	l.block = nil
	l.toolCalls = make(map[string]chan struct{})
	l.toolCallErr = make(map[string]error)
	_, err := l.appendItem(ctx, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnStarted,
		Turn: &zotigosession.DisplayTurn{ID: l.turnID},
	})
	return l.turnID, err
}

func (l *workerDisplayLog) CurrentTurnID() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.turnID
}

func (l *workerDisplayLog) ProfileChanged(ctx context.Context, commandID string, from string, to string) error {
	_, err := l.appendItem(ctx, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemProfileChanged,
		Profile: &zotigosession.DisplayProfileChange{
			CommandID: commandID,
			From:      from,
			To:        to,
		},
	})
	return err
}

func (l *workerDisplayLog) ProfileFailed(ctx context.Context, commandID string, from string, to string, profileErr error) error {
	_, err := l.appendItem(ctx, zotigosession.DisplayItem{
		Type:  zotigosession.DisplayItemProfileFailed,
		Error: profileErr.Error(),
		Profile: &zotigosession.DisplayProfileChange{
			CommandID: commandID,
			From:      from,
			To:        to,
		},
	})
	return err
}

func (l *workerDisplayLog) ApprovalPolicyChanged(ctx context.Context, commandID string, from string, to string) error {
	_, err := l.appendItem(ctx, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemApprovalPolicyChanged,
		ApprovalPolicy: &zotigosession.DisplayApprovalPolicyChange{
			CommandID: commandID,
			From:      from,
			To:        to,
		},
	})
	return err
}

func (l *workerDisplayLog) InterruptOpenTurn(ctx context.Context, reason string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	items, _, err := l.items.LoadItems(ctx, l.sessionID)
	if err != nil {
		return err
	}
	turnID, started := lastOpenTurn(items)
	if turnID == "" {
		return nil
	}
	duration := int64(0)
	if !started.IsZero() {
		duration = time.Since(started).Milliseconds()
		if duration < 0 {
			duration = 0
		}
	}
	if reason == "" {
		reason = workerRestartedReason
	}
	_, err = l.appendItem(ctx, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnInterrupted,
		Turn: &zotigosession.DisplayTurn{
			ID:         turnID,
			Status:     "interrupted",
			Reason:     reason,
			DurationMS: duration,
		},
	})
	return err
}

func (l *workerDisplayLog) MarkPaused() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.block = nil
}

func (l *workerDisplayLog) Interrupt(ctx context.Context, reason string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.turnID == "" {
		return nil
	}
	l.block = nil
	if reason == "" {
		reason = userPauseReason
	}
	_, err := l.appendItem(ctx, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnInterrupted,
		Turn: &zotigosession.DisplayTurn{
			ID:         l.turnID,
			Status:     "interrupted",
			Reason:     reason,
			DurationMS: time.Since(l.turnStarted).Milliseconds(),
		},
	})
	l.turnID = ""
	return err
}

func (l *workerDisplayLog) Fail(ctx context.Context, err error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.block = nil
	errText := fmt.Sprintf("%v", err)
	if _, appendErr := l.appendItem(ctx, zotigosession.DisplayItem{
		Type:  zotigosession.DisplayItemError,
		Error: errText,
	}); appendErr != nil {
		return appendErr
	}
	_, appendErr := l.appendItem(ctx, zotigosession.DisplayItem{
		Type:  zotigosession.DisplayItemTurnFailed,
		Error: errText,
		Turn: &zotigosession.DisplayTurn{
			ID:         l.turnID,
			Status:     "failed",
			DurationMS: time.Since(l.turnStarted).Milliseconds(),
		},
	})
	l.turnID = ""
	return appendErr
}

func (l *workerDisplayLog) HandleEvent(ctx context.Context, event protocol.Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.turnID == "" {
		return nil
	}

	switch event.Type {
	case protocol.EventTypeContentDelta:
		if event.ContentPartDelta == nil || event.ContentPartDelta.Text == "" {
			return nil
		}
		partType := string(event.ContentPartDelta.Type)
		if partType == "" {
			partType = string(protocol.ContentTypeText)
		}
		if l.block != nil && (l.block.index != event.Index || l.block.partType != partType) {
			if err := l.flushBlockLocked(ctx); err != nil {
				return err
			}
		}
		if l.block == nil {
			l.block = &workerDisplayBlock{id: "item_" + uuid.NewString(), index: event.Index, partType: partType}
		}
		l.block.text += event.ContentPartDelta.Text
		if l.delta != nil {
			l.delta(displayDeltaEvent{
				ItemID:   l.block.id,
				Role:     string(protocol.RoleAssistant),
				PartType: partType,
				Delta:    event.ContentPartDelta.Text,
			})
		}
	case protocol.EventTypeContentEnd:
		if l.block == nil && event.ContentPart != nil && event.ContentPart.Text != "" {
			partType := string(event.ContentPart.Type)
			if partType == "" {
				partType = string(protocol.ContentTypeText)
			}
			l.block = &workerDisplayBlock{
				id:       "item_" + uuid.NewString(),
				index:    event.Index,
				partType: partType,
				text:     event.ContentPart.Text,
			}
		} else if l.block != nil && event.ContentPart != nil && event.ContentPart.Text != "" {
			l.block.text = event.ContentPart.Text
		}
		return l.flushBlockLocked(ctx)
	case protocol.EventTypeToolCallEnd:
		if event.ToolCall != nil {
			if err := l.flushBlockLocked(ctx); err != nil {
				return err
			}
			_, err := l.appendItem(ctx, zotigosession.DisplayItem{
				Type: zotigosession.DisplayItemAssistantMessage,
				Role: string(protocol.RoleAssistant),
				Content: []zotigosession.DisplayContentPart{{
					Type: string(protocol.ContentTypeToolCall),
					ToolCall: &zotigosession.DisplayToolCall{
						ID:        event.ToolCall.ID,
						Name:      event.ToolCall.Name,
						Arguments: event.ToolCall.Arguments,
					},
				}},
			})
			l.completeToolCallLocked(event.ToolCall.ID, err)
			return err
		}
	case protocol.EventTypeToolResultDone:
		if event.ToolResult != nil {
			if err := l.flushBlockLocked(ctx); err != nil {
				return err
			}
			_, err := l.appendItem(ctx, zotigosession.DisplayItem{
				Type: zotigosession.DisplayItemAssistantMessage,
				Role: string(protocol.RoleAssistant),
				Content: []zotigosession.DisplayContentPart{{
					Type:       string(protocol.ContentTypeToolResult),
					ToolResult: displayToolResultFromProtocol(event.ToolResult),
				}},
			})
			return err
		}
	case protocol.EventTypeFinish:
		if event.FinishReason == "need_approval" {
			return l.flushBlockLocked(ctx)
		}
		if err := l.flushBlockLocked(ctx); err != nil {
			return err
		}
		_, err := l.appendItem(ctx, zotigosession.DisplayItem{
			Type: zotigosession.DisplayItemTurnCompleted,
			Turn: &zotigosession.DisplayTurn{
				ID:                   l.turnID,
				Status:               "completed",
				ProviderFinishReason: string(event.FinishReason),
				DurationMS:           time.Since(l.turnStarted).Milliseconds(),
			},
		})
		l.turnID = ""
		return err
	case protocol.EventTypeError:
		if event.Error != nil {
			return l.failLocked(ctx, event.Error)
		}
	}
	return nil
}

func (l *workerDisplayLog) flushBlockLocked(ctx context.Context) error {
	if l.block == nil || l.block.text == "" {
		l.block = nil
		return nil
	}
	if _, err := l.appendItem(ctx, zotigosession.DisplayItem{
		ID:      l.block.id,
		Type:    zotigosession.DisplayItemAssistantMessage,
		Role:    string(protocol.RoleAssistant),
		Content: []zotigosession.DisplayContentPart{{Type: l.block.partType, Text: l.block.text}},
	}); err != nil {
		return err
	}
	l.block = nil
	return nil
}

func (l *workerDisplayLog) failLocked(ctx context.Context, err error) error {
	l.block = nil
	errText := fmt.Sprintf("%v", err)
	if _, appendErr := l.appendItem(ctx, zotigosession.DisplayItem{
		Type:  zotigosession.DisplayItemError,
		Error: errText,
	}); appendErr != nil {
		return appendErr
	}
	_, appendErr := l.appendItem(ctx, zotigosession.DisplayItem{
		Type:  zotigosession.DisplayItemTurnFailed,
		Error: errText,
		Turn: &zotigosession.DisplayTurn{
			ID:         l.turnID,
			Status:     "failed",
			DurationMS: time.Since(l.turnStarted).Milliseconds(),
		},
	})
	l.turnID = ""
	return appendErr
}

func (l *workerDisplayLog) ToolExecutionStarted(ctx context.Context, toolCallID string, toolName string) error {
	if err := l.waitForToolCall(ctx, toolCallID); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.turnID == "" {
		return fmt.Errorf("tool execution started outside an active turn")
	}
	_, err := l.appendItem(ctx, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemToolExecutionStarted,
		ToolExecution: &zotigosession.DisplayToolExecution{
			TurnID:     l.turnID,
			ToolCallID: toolCallID,
			ToolName:   toolName,
		},
	})
	return err
}

func (l *workerDisplayLog) waitForToolCall(ctx context.Context, toolCallID string) error {
	l.mu.Lock()
	ready := l.toolCalls[toolCallID]
	if ready == nil {
		ready = make(chan struct{})
		l.toolCalls[toolCallID] = ready
	}
	select {
	case <-ready:
		err := l.toolCallErr[toolCallID]
		l.mu.Unlock()
		return err
	default:
	}
	l.mu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ready:
		l.mu.Lock()
		err := l.toolCallErr[toolCallID]
		l.mu.Unlock()
		return err
	}
}

func (l *workerDisplayLog) completeToolCallLocked(toolCallID string, err error) {
	ready := l.toolCalls[toolCallID]
	if ready == nil {
		ready = make(chan struct{})
		l.toolCalls[toolCallID] = ready
	}
	l.toolCallErr[toolCallID] = err
	select {
	case <-ready:
	default:
		close(ready)
	}
}

func (l *workerDisplayLog) appendItem(ctx context.Context, item zotigosession.DisplayItem) (zotigosession.DisplayItem, error) {
	stored, err := l.items.AppendItem(ctx, l.sessionID, item)
	if err == nil && l.wake != nil {
		l.wake(ctx)
	}
	return stored, err
}

func displayToolResultFromProtocol(result *protocol.ToolResult) *zotigosession.DisplayToolResult {
	if result == nil {
		return nil
	}
	return &zotigosession.DisplayToolResult{
		ToolCallID: result.ToolCallID,
		ToolName:   result.ToolName,
		ResultType: string(result.Type),
		Text:       result.Text,
		JSON:       result.JSON,
		Reason:     result.Reason,
		Content:    displayToolResultContentFromProtocol(result.Content),
		IsError:    result.IsError,
	}
}

func displayToolResultContentFromProtocol(content []protocol.ToolResultContentPart) []zotigosession.DisplayToolResultContentPart {
	if len(content) == 0 {
		return nil
	}
	parts := make([]zotigosession.DisplayToolResultContentPart, 0, len(content))
	for _, part := range content {
		parts = append(parts, zotigosession.DisplayToolResultContentPart{
			Type:  string(part.Type),
			Text:  part.Text,
			Image: displayMediaPartFromProtocol(part.Image),
		})
	}
	return parts
}

func displayMediaPartFromProtocol(media *protocol.MediaPart) *zotigosession.DisplayMediaPart {
	if media == nil {
		return nil
	}
	return &zotigosession.DisplayMediaPart{
		Data:      media.Data,
		URL:       media.URL,
		FileID:    media.FileID,
		MediaType: media.MediaType,
	}
}
