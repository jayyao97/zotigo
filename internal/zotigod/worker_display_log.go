package zotigod

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

type workerDisplayLog struct {
	sessionID string
	items     displayItemSource

	mu          sync.Mutex
	turnID      string
	turnStarted time.Time
	content     []zotigosession.DisplayContentPart
}

func newWorkerDisplayLog(sessionID string, items displayItemSource) *workerDisplayLog {
	return &workerDisplayLog{sessionID: sessionID, items: items}
}

func (l *workerDisplayLog) StartTurn(ctx context.Context) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.turnStarted = time.Now()
	l.turnID = fmt.Sprintf("turn_%d", l.turnStarted.UnixNano())
	l.content = nil
	_, err := l.items.AppendItem(ctx, l.sessionID, zotigosession.DisplayItem{
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
	_, err := l.items.AppendItem(ctx, l.sessionID, zotigosession.DisplayItem{
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
	_, err := l.items.AppendItem(ctx, l.sessionID, zotigosession.DisplayItem{
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
	_, err = l.items.AppendItem(ctx, l.sessionID, zotigosession.DisplayItem{
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
	l.flushAssistantLocked(context.Background())
}

func (l *workerDisplayLog) Interrupt(ctx context.Context, reason string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.turnID == "" {
		return nil
	}
	l.flushAssistantLocked(ctx)
	if reason == "" {
		reason = userPauseReason
	}
	_, err := l.items.AppendItem(ctx, l.sessionID, zotigosession.DisplayItem{
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
	l.flushAssistantLocked(ctx)
	errText := fmt.Sprintf("%v", err)
	if _, appendErr := l.items.AppendItem(ctx, l.sessionID, zotigosession.DisplayItem{
		Type:  zotigosession.DisplayItemError,
		Error: errText,
	}); appendErr != nil {
		return appendErr
	}
	_, appendErr := l.items.AppendItem(ctx, l.sessionID, zotigosession.DisplayItem{
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
		l.appendContentLocked(partType, event.ContentPartDelta.Text)
	case protocol.EventTypeContentEnd:
		if event.ContentPart != nil && event.ContentPart.Type == protocol.ContentTypeReasoning && event.ContentPart.Text != "" {
			l.appendContentLocked(string(protocol.ContentTypeReasoning), event.ContentPart.Text)
		}
	case protocol.EventTypeToolCallEnd:
		if event.ToolCall != nil {
			l.content = append(l.content, zotigosession.DisplayContentPart{
				Type: string(protocol.ContentTypeToolCall),
				ToolCall: &zotigosession.DisplayToolCall{
					ID:        event.ToolCall.ID,
					Name:      event.ToolCall.Name,
					Arguments: event.ToolCall.Arguments,
				},
			})
		}
	case protocol.EventTypeToolResultDone:
		if event.ToolResult != nil {
			l.content = append(l.content, zotigosession.DisplayContentPart{
				Type:       string(protocol.ContentTypeToolResult),
				ToolResult: displayToolResultFromProtocol(event.ToolResult),
			})
		}
	case protocol.EventTypeFinish:
		if event.FinishReason == "need_approval" {
			l.flushAssistantLocked(ctx)
			return nil
		}
		l.flushAssistantLocked(ctx)
		_, err := l.items.AppendItem(ctx, l.sessionID, zotigosession.DisplayItem{
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

func (l *workerDisplayLog) appendContentLocked(partType string, text string) {
	if text == "" {
		return
	}
	last := len(l.content) - 1
	if last >= 0 && l.content[last].Type == partType && l.content[last].ToolCall == nil && l.content[last].ToolResult == nil {
		l.content[last].Text += text
		return
	}
	l.content = append(l.content, zotigosession.DisplayContentPart{Type: partType, Text: text})
}

func (l *workerDisplayLog) flushAssistantLocked(ctx context.Context) {
	if len(l.content) == 0 {
		return
	}
	content := make([]zotigosession.DisplayContentPart, len(l.content))
	copy(content, l.content)
	_, _ = l.items.AppendItem(ctx, l.sessionID, zotigosession.DisplayItem{
		Type:    zotigosession.DisplayItemAssistantMessage,
		Role:    string(protocol.RoleAssistant),
		Content: content,
	})
	l.content = nil
}

func (l *workerDisplayLog) failLocked(ctx context.Context, err error) error {
	l.flushAssistantLocked(ctx)
	errText := fmt.Sprintf("%v", err)
	if _, appendErr := l.items.AppendItem(ctx, l.sessionID, zotigosession.DisplayItem{
		Type:  zotigosession.DisplayItemError,
		Error: errText,
	}); appendErr != nil {
		return appendErr
	}
	_, appendErr := l.items.AppendItem(ctx, l.sessionID, zotigosession.DisplayItem{
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
