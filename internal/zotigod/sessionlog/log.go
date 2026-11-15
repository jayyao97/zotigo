package sessionlog

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
)

type Log struct {
	sessionID string
	Items     ItemSource
	Persisted func(zotigosession.DisplayItem)
	Wake      func(context.Context)
	WakeSync  func(context.Context) error
	Barrier   func(context.Context) error
	Delta     func(wire.DisplayDelta)

	mu            sync.Mutex
	turnID        string
	turnStarted   time.Time
	turnUsage     protocol.Usage
	runUsage      protocol.Usage
	terminalID    string
	terminal      string
	block         *displayBlock
	subagents     map[string]*subagentBlock
	toolCalls     map[string]chan struct{}
	toolCallErr   map[string]error
	steering      map[string]wire.Command
	steeringOrder []string
	deltaMuted    bool
}

type displayBlock struct {
	id       string
	index    int
	partType string
	text     string
}

type subagentBlock struct {
	block displayBlock
	info  zotigosession.DisplaySubagent
}

func NewLog(sessionID string, items ItemSource) *Log {
	return &Log{sessionID: sessionID, Items: items}
}

func (l *Log) StartTurn(ctx context.Context) (string, error) {
	return l.startTurn(ctx, nil)
}

func (l *Log) StartTurnWithRuntime(ctx context.Context, runtime zotigosession.DisplayRuntime) (string, error) {
	return l.startTurn(ctx, &runtime)
}

func (l *Log) startTurn(ctx context.Context, runtime *zotigosession.DisplayRuntime) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.turnStarted = time.Now()
	l.turnUsage = protocol.Usage{}
	l.runUsage = protocol.Usage{}
	l.turnID = fmt.Sprintf("turn_%d", l.turnStarted.UnixNano())
	l.terminalID = ""
	l.terminal = ""
	l.block = nil
	l.subagents = make(map[string]*subagentBlock)
	l.toolCalls = make(map[string]chan struct{})
	l.toolCallErr = make(map[string]error)
	l.steering = make(map[string]wire.Command)
	l.steeringOrder = nil
	l.deltaMuted = false
	_, err := l.AppendItem(ctx, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnStarted,
		Turn: &zotigosession.DisplayTurn{ID: l.turnID, Runtime: runtime},
	})
	return l.turnID, err
}

func (l *Log) CurrentTurnID() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.turnID
}

func (l *Log) TerminalStatus(turnID string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.terminal, turnID != "" && l.terminalID == turnID
}

func (l *Log) QueueSteering(command wire.Command) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.steering == nil {
		l.steering = make(map[string]wire.Command)
	}
	if _, exists := l.steering[command.ID]; !exists {
		l.steeringOrder = append(l.steeringOrder, command.ID)
	}
	l.steering[command.ID] = command
	// The current block may not emit another delta before it finishes.
	if l.block != nil && l.Delta != nil && !l.deltaMuted {
		l.Delta(wire.DisplayDelta{
			ItemID: l.block.id, Role: string(protocol.RoleAssistant),
			PartType: l.block.partType, BeforeInputID: l.beforeInputIDLocked(),
		})
	}
}

func (l *Log) beforeInputIDLocked() string {
	for len(l.steeringOrder) > 0 {
		id := l.steeringOrder[0]
		if _, exists := l.steering[id]; exists {
			return id
		}
		l.steeringOrder = l.steeringOrder[1:]
	}
	return ""
}

func (l *Log) DiscardSteering(commandID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.steering, commandID)
}

func (l *Log) HasQueuedSteering(commandID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.steering[commandID]
	return ok
}

func (l *Log) ProfileChanged(ctx context.Context, commandID string, from string, to string) error {
	_, err := l.AppendItem(ctx, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemProfileChanged,
		Profile: &zotigosession.DisplayProfileChange{
			CommandID: commandID,
			From:      from,
			To:        to,
		},
	})
	return err
}

func (l *Log) ProfileFailed(ctx context.Context, commandID string, from string, to string, profileErr error) error {
	_, err := l.AppendItem(ctx, zotigosession.DisplayItem{
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

func (l *Log) ApprovalPolicyChanged(ctx context.Context, commandID string, from string, to string) error {
	_, err := l.AppendItem(ctx, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemApprovalPolicyChanged,
		ApprovalPolicy: &zotigosession.DisplayApprovalPolicyChange{
			CommandID: commandID,
			From:      from,
			To:        to,
		},
	})
	return err
}

func (l *Log) InterruptOpenTurn(ctx context.Context, reason string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	items, _, err := l.Items.LoadItems(ctx, l.sessionID)
	if err != nil {
		return err
	}
	turnID, started := LastOpenTurn(items)
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
		reason = WorkerRestartedReason
	}
	_, err = l.AppendItem(ctx, zotigosession.DisplayItem{
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

func (l *Log) MarkPaused() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.block = nil
}

func (l *Log) ApprovalRequested(ctx context.Context, approval ApprovalRequest) (ApprovalRequest, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	item, err := l.AppendItem(ctx, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemApprovalRequest,
		Approval: &zotigosession.DisplayApproval{
			ID:      approval.ID,
			TurnID:  approval.TurnID,
			Pending: CopyPendingApprovals(approval.Pending),
		},
	})
	if err != nil {
		return ApprovalRequest{}, err
	}
	approval.CreatedAt = item.CreatedAt
	_, _ = l.AppendItem(ctx, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnPaused,
		Turn: &zotigosession.DisplayTurn{
			ID:     approval.TurnID,
			Reason: "need_approval",
		},
	})
	l.block = nil
	return approval, nil
}

func (l *Log) ApprovalResolved(ctx context.Context, approval ApprovalRequest, decisions []zotigosession.DisplayApprovalDecision) (ApprovalRequest, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	item, err := l.AppendItem(ctx, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemApprovalDecision,
		Approval: &zotigosession.DisplayApproval{
			ID:        approval.ID,
			TurnID:    approval.TurnID,
			Decisions: CopyApprovalDecisions(decisions),
		},
	})
	if err != nil {
		return ApprovalRequest{}, err
	}
	return ResolvedApprovalFromDecision(approval, decisions, item.CreatedAt), nil
}

func (l *Log) ResolvePendingApprovalsForOpenTurn(ctx context.Context, reason string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	items, _, err := l.Items.LoadItems(ctx, l.sessionID)
	if err != nil {
		return false, err
	}
	turnID := LastOpenTurnID(items)
	if turnID == "" {
		return false, nil
	}
	resolved := make(map[string]struct{})
	for _, item := range items {
		if item.Type == zotigosession.DisplayItemApprovalDecision && item.Approval != nil && item.Approval.TurnID == turnID {
			resolved[item.Approval.ID] = struct{}{}
		}
	}
	recovered := false
	for _, item := range items {
		if item.Type != zotigosession.DisplayItemApprovalRequest || item.Approval == nil || item.Approval.TurnID != turnID {
			continue
		}
		if _, ok := resolved[item.Approval.ID]; ok {
			continue
		}
		decisions := make([]zotigosession.DisplayApprovalDecision, 0, len(item.Approval.Pending))
		for _, pending := range item.Approval.Pending {
			decisions = append(decisions, zotigosession.DisplayApprovalDecision{
				ToolCallID: pending.ToolCallID,
				Approved:   false,
				Reason:     reason,
			})
		}
		if _, err := l.AppendItem(ctx, zotigosession.DisplayItem{
			Type: zotigosession.DisplayItemApprovalDecision,
			Approval: &zotigosession.DisplayApproval{
				ID:        item.Approval.ID,
				TurnID:    turnID,
				Decisions: decisions,
			},
		}); err != nil {
			return false, err
		}
		resolved[item.Approval.ID] = struct{}{}
		recovered = true
	}
	return recovered, nil
}

func (l *Log) Interrupt(ctx context.Context, reason string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.turnID == "" {
		return nil
	}
	l.block = nil
	if reason == "" {
		reason = wire.UserPauseReason
	}
	turnID := l.turnID
	usage := l.turnUsage.Add(l.runUsage).Normalized()
	_, err := l.AppendItem(ctx, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnInterrupted,
		Turn: &zotigosession.DisplayTurn{
			ID:         l.turnID,
			Status:     "interrupted",
			Reason:     reason,
			DurationMS: time.Since(l.turnStarted).Milliseconds(),
			Usage:      &usage,
		},
	})
	if err == nil {
		l.terminalID = turnID
		l.terminal = "interrupted"
	}
	l.turnID = ""
	return err
}

func (l *Log) Fail(ctx context.Context, err error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.failLocked(ctx, err)
}

func (l *Log) HandleEvent(ctx context.Context, event protocol.Event) error {
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
			l.block = &displayBlock{id: "item_" + uuid.NewString(), index: event.Index, partType: partType}
		}
		l.block.text += event.ContentPartDelta.Text
		if l.Delta != nil && !l.deltaMuted {
			l.Delta(wire.DisplayDelta{
				BeforeInputID: l.beforeInputIDLocked(),
				ItemID:        l.block.id,
				Role:          string(protocol.RoleAssistant),
				PartType:      partType,
				Delta:         event.ContentPartDelta.Text,
			})
		}
	case protocol.EventTypeContentEnd:
		if l.block == nil && event.ContentPart != nil && event.ContentPart.Text != "" {
			partType := string(event.ContentPart.Type)
			if partType == "" {
				partType = string(protocol.ContentTypeText)
			}
			l.block = &displayBlock{
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
			_, err := l.AppendItem(ctx, zotigosession.DisplayItem{
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
			_, err := l.AppendItem(ctx, zotigosession.DisplayItem{
				Type: zotigosession.DisplayItemAssistantMessage,
				Role: string(protocol.RoleAssistant),
				Content: []zotigosession.DisplayContentPart{{
					Type:       string(protocol.ContentTypeToolResult),
					ToolResult: displayToolResultFromProtocol(event.ToolResult),
				}},
			})
			return err
		}
	case protocol.EventTypeToolProgress:
		if event.ToolResult != nil && event.ToolResult.ToolCallID != "" && event.ToolResult.Text != "" && l.Delta != nil && !l.deltaMuted {
			l.Delta(wire.DisplayDelta{
				BeforeInputID: l.beforeInputIDLocked(),
				ItemID:        "tool-progress:" + event.ToolResult.ToolCallID,
				Role:          string(protocol.RoleAssistant),
				PartType:      "tool_progress",
				Delta:         event.ToolResult.Text,
				ToolCallID:    event.ToolResult.ToolCallID,
				ToolName:      event.ToolResult.ToolName,
			})
		}
	case protocol.EventTypeSubagent:
		return l.handleSubagentEventLocked(ctx, event.Subagent)
	case protocol.EventTypeContextCompacted:
		if event.ContextCompaction == nil {
			return nil
		}
		if err := l.flushBlockLocked(ctx); err != nil {
			return err
		}
		_, err := l.AppendItem(ctx, zotigosession.DisplayItem{
			Type: zotigosession.DisplayItemContextCompacted,
			ContextCompaction: &zotigosession.DisplayContextCompaction{
				OriginalTokens:   event.ContextCompaction.OriginalTokens,
				CompressedTokens: event.ContextCompaction.CompressedTokens,
				MessagesBefore:   event.ContextCompaction.MessagesBefore,
				MessagesAfter:    event.ContextCompaction.MessagesAfter,
			},
		})
		return err
	case protocol.EventTypeSteeringApplied:
		if err := l.flushBlockLocked(ctx); err != nil {
			return err
		}
		if len(event.SteeringIDs) > 0 && l.Barrier != nil {
			if err := l.Barrier(ctx); err != nil {
				l.deltaMuted = true
				for _, commandID := range event.SteeringIDs {
					delete(l.steering, commandID)
				}
				return fmt.Errorf("establish steering display boundary: %w", err)
			}
		}
		for _, commandID := range event.SteeringIDs {
			command, ok := l.steering[commandID]
			if !ok || command.Steering == nil {
				return fmt.Errorf("applied steering command %q is unavailable", commandID)
			}
			images := make([]MessageImage, 0, len(command.Steering.Images))
			for _, image := range command.Steering.Images {
				images = append(images, MessageImage{
					MimeType:  image.MimeType,
					SizeBytes: image.SizeBytes,
					Width:     image.Width,
					Height:    image.Height,
					BlobPath:  image.BlobPath,
				})
			}
			item := MessageItem(zotigosession.DisplayItemSteeringMessage, command.Steering.Text, images)
			item.ID = command.ID
			item.CreatedAt = command.CreatedAt
			item.Turn = &zotigosession.DisplayTurn{ID: l.turnID}
			item.Command = &zotigosession.DisplayCommand{
				Type:   wire.CommandSteering,
				Text:   command.Steering.Text,
				Images: CommandImages(images),
				TurnID: l.turnID,
			}
			if _, err := l.Items.AppendItem(ctx, l.sessionID, item); err != nil {
				return err
			}
			delete(l.steering, commandID)
		}
		if len(event.SteeringIDs) > 0 {
			if l.WakeSync != nil {
				if err := l.WakeSync(ctx); err != nil {
					l.deltaMuted = true
				}
			} else if l.Wake != nil {
				l.Wake(ctx)
			}
		}
		return nil
	case protocol.EventTypeRequestRetry:
		_, err := l.AppendItem(ctx, zotigosession.DisplayItem{Type: zotigosession.DisplayItemRequestRetry, Retry: event.Retry})
		return err
	case protocol.EventTypeUsageUpdated:
		if event.Usage != nil {
			l.runUsage = event.Usage.Normalized()
		}
		item := zotigosession.DisplayItem{Type: zotigosession.DisplayItemModelUsageUpdated}
		if usage := event.ContextUsage; usage != nil {
			item.ContextUsage = &zotigosession.DisplayContextUsage{Tokens: usage.Tokens, Window: usage.Window, Source: usage.Source, Status: "available", ProfileKey: usage.ProfileKey, UpdatedAt: time.Now().UTC()}
		}
		if event.Usage != nil {
			usage := l.turnUsage.Add(l.runUsage).Normalized()
			item.Turn = &zotigosession.DisplayTurn{ID: l.turnID, Usage: &usage}
		}
		_, err := l.AppendItem(ctx, item)
		return err
	case protocol.EventTypeFinish:
		if event.Usage != nil {
			l.turnUsage = l.turnUsage.Add(event.Usage.Normalized()).Normalized()
		}
		l.runUsage = protocol.Usage{}
		if event.FinishReason == "need_approval" {
			return l.flushBlockLocked(ctx)
		}
		if err := l.flushBlockLocked(ctx); err != nil {
			return err
		}
		turnID := l.turnID
		usage := l.turnUsage
		var usagePointer *protocol.Usage
		if usage != (protocol.Usage{}) {
			usagePointer = &usage
		}
		_, err := l.AppendItem(ctx, zotigosession.DisplayItem{
			Type: zotigosession.DisplayItemTurnCompleted,
			Turn: &zotigosession.DisplayTurn{
				ID:                   l.turnID,
				Status:               "completed",
				ProviderFinishReason: string(event.FinishReason),
				DurationMS:           time.Since(l.turnStarted).Milliseconds(),
				Usage:                usagePointer,
			},
		})
		if err == nil {
			l.terminalID = turnID
			l.terminal = "completed"
		}
		l.turnID = ""
		return err
	case protocol.EventTypeError:
		if event.Error != nil {
			if event.Usage != nil {
				l.turnUsage = l.turnUsage.Add(event.Usage.Normalized()).Normalized()
				l.runUsage = protocol.Usage{}
			}
			return l.failLocked(ctx, event.Error)
		}
	}
	return nil
}

func (l *Log) flushBlockLocked(ctx context.Context) error {
	if l.block == nil || l.block.text == "" {
		l.block = nil
		return nil
	}
	if _, err := l.AppendItem(ctx, zotigosession.DisplayItem{
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

func (l *Log) handleSubagentEventLocked(ctx context.Context, subagent *protocol.SubagentEvent) error {
	if subagent == nil || subagent.ToolCallID == "" {
		return nil
	}
	info := zotigosession.DisplaySubagent{
		ToolCallID: subagent.ToolCallID, Name: subagent.Name, AgentType: subagent.AgentType,
		WorkDir: subagent.WorkDir, Description: subagent.Description, Status: "running",
	}
	if subagent.Status != "" {
		info.Status = subagent.Status
		return l.appendSubagentItemLocked(ctx, "", info, nil, "")
	}
	if subagent.Event == nil {
		return nil
	}
	child := subagent.Event
	switch child.Type {
	case protocol.EventTypeContentDelta:
		if child.ContentPartDelta == nil || child.ContentPartDelta.Text == "" {
			return nil
		}
		partType := string(child.ContentPartDelta.Type)
		if partType == "" {
			partType = string(protocol.ContentTypeText)
		}
		current := l.subagents[subagent.ToolCallID]
		if current != nil && (current.block.index != child.Index || current.block.partType != partType) {
			if err := l.flushSubagentBlockLocked(ctx, subagent.ToolCallID); err != nil {
				return err
			}
			current = nil
		}
		if current == nil {
			current = &subagentBlock{
				block: displayBlock{id: "item_" + uuid.NewString(), index: child.Index, partType: partType},
				info:  info,
			}
			l.subagents[subagent.ToolCallID] = current
		}
		current.block.text += child.ContentPartDelta.Text
		if l.Delta != nil && !l.deltaMuted {
			l.Delta(wire.DisplayDelta{
				ItemID: current.block.id, Role: string(protocol.RoleAssistant), PartType: partType,
				Delta: child.ContentPartDelta.Text, Subagent: &current.info,
			})
		}
		return nil
	case protocol.EventTypeContentEnd:
		current := l.subagents[subagent.ToolCallID]
		if current == nil && child.ContentPart != nil && child.ContentPart.Text != "" {
			partType := string(child.ContentPart.Type)
			if partType == "" {
				partType = string(protocol.ContentTypeText)
			}
			l.subagents[subagent.ToolCallID] = &subagentBlock{
				block: displayBlock{id: "item_" + uuid.NewString(), index: child.Index, partType: partType, text: child.ContentPart.Text},
				info:  info,
			}
		} else if current != nil && child.ContentPart != nil && child.ContentPart.Text != "" {
			current.block.text = child.ContentPart.Text
		}
		return l.flushSubagentBlockLocked(ctx, subagent.ToolCallID)
	case protocol.EventTypeToolCallEnd:
		if err := l.flushSubagentBlockLocked(ctx, subagent.ToolCallID); err != nil {
			return err
		}
		if child.ToolCall == nil {
			return nil
		}
		return l.appendSubagentItemLocked(ctx, "", info, []zotigosession.DisplayContentPart{{
			Type:     string(protocol.ContentTypeToolCall),
			ToolCall: &zotigosession.DisplayToolCall{ID: child.ToolCall.ID, Name: child.ToolCall.Name, Arguments: child.ToolCall.Arguments},
		}}, "")
	case protocol.EventTypeToolResultDone:
		if err := l.flushSubagentBlockLocked(ctx, subagent.ToolCallID); err != nil {
			return err
		}
		if child.ToolResult == nil {
			return nil
		}
		return l.appendSubagentItemLocked(ctx, "", info, []zotigosession.DisplayContentPart{{
			Type: string(protocol.ContentTypeToolResult), ToolResult: displayToolResultFromProtocol(child.ToolResult),
		}}, "")
	case protocol.EventTypeToolProgress:
		if child.ToolResult != nil && child.ToolResult.ToolCallID != "" && child.ToolResult.Text != "" && l.Delta != nil && !l.deltaMuted {
			l.Delta(wire.DisplayDelta{
				ItemID: "subagent-tool-progress:" + subagent.ToolCallID + ":" + child.ToolResult.ToolCallID,
				Role:   string(protocol.RoleAssistant), PartType: "tool_progress", Delta: child.ToolResult.Text,
				ToolCallID: child.ToolResult.ToolCallID, ToolName: child.ToolResult.ToolName, Subagent: &info,
			})
		}
		return nil
	case protocol.EventTypeFinish:
		if err := l.flushSubagentBlockLocked(ctx, subagent.ToolCallID); err != nil {
			return err
		}
		if child.FinishReason == "need_approval" {
			info.Status = "waiting_approval"
		} else {
			info.Status = "completed"
		}
		return l.appendSubagentItemLocked(ctx, "", info, nil, "")
	case protocol.EventTypeError:
		if err := l.flushSubagentBlockLocked(ctx, subagent.ToolCallID); err != nil {
			return err
		}
		info.Status = "failed"
		if child.Error != nil {
			return l.appendSubagentItemLocked(ctx, "", info, nil, child.Error.Error())
		}
	}
	return nil
}

func (l *Log) flushSubagentBlockLocked(ctx context.Context, toolCallID string) error {
	current := l.subagents[toolCallID]
	if current == nil || current.block.text == "" {
		delete(l.subagents, toolCallID)
		return nil
	}
	err := l.appendSubagentItemLocked(ctx, current.block.id, current.info, []zotigosession.DisplayContentPart{{
		Type: current.block.partType, Text: current.block.text,
	}}, "")
	delete(l.subagents, toolCallID)
	return err
}

func (l *Log) appendSubagentItemLocked(ctx context.Context, itemID string, info zotigosession.DisplaySubagent, content []zotigosession.DisplayContentPart, errText string) error {
	_, err := l.AppendItem(ctx, zotigosession.DisplayItem{
		ID: itemID, Type: zotigosession.DisplayItemAssistantMessage, Role: string(protocol.RoleAssistant),
		Content: content, Subagent: &info, Error: errText,
	})
	return err
}

func (l *Log) failLocked(ctx context.Context, err error) error {
	l.block = nil
	usage := l.turnUsage.Add(l.runUsage).Normalized()
	var usagePointer *protocol.Usage
	if usage != (protocol.Usage{}) {
		usagePointer = &usage
	}
	errText := fmt.Sprintf("%v", err)
	if _, appendErr := l.AppendItem(ctx, zotigosession.DisplayItem{
		Type:  zotigosession.DisplayItemError,
		Error: errText,
	}); appendErr != nil {
		return appendErr
	}
	turnID := l.turnID
	_, appendErr := l.AppendItem(ctx, zotigosession.DisplayItem{
		Type:  zotigosession.DisplayItemTurnFailed,
		Error: errText,
		Turn: &zotigosession.DisplayTurn{
			ID:         l.turnID,
			Status:     "failed",
			DurationMS: time.Since(l.turnStarted).Milliseconds(),
			Usage:      usagePointer,
		},
	})
	if appendErr == nil {
		l.terminalID = turnID
		l.terminal = "failed"
	}
	l.turnID = ""
	return appendErr
}

func (l *Log) ToolExecutionStarted(ctx context.Context, toolCallID string, toolName string) error {
	if err := l.waitForToolCall(ctx, toolCallID); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.turnID == "" {
		return fmt.Errorf("tool execution started outside an active turn")
	}
	_, err := l.AppendItem(ctx, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemToolExecutionStarted,
		ToolExecution: &zotigosession.DisplayToolExecution{
			TurnID:     l.turnID,
			ToolCallID: toolCallID,
			ToolName:   toolName,
		},
	})
	return err
}

func (l *Log) waitForToolCall(ctx context.Context, toolCallID string) error {
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

func (l *Log) completeToolCallLocked(toolCallID string, err error) {
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

// AppendItem writes one item to the durable log and notifies the daemon. It is
// exported only because a daemon-side test drives it directly.
func (l *Log) AppendItem(ctx context.Context, item zotigosession.DisplayItem) (zotigosession.DisplayItem, error) {
	if item.Type == zotigosession.DisplayItemAssistantMessage {
		item.BeforeInputID = l.beforeInputIDLocked()
	}
	stored, err := l.Items.AppendItem(ctx, l.sessionID, item)
	if err == nil {
		if l.Persisted != nil {
			l.Persisted(stored)
		} else if l.Wake != nil {
			l.Wake(ctx)
		}
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
		Metadata:   result.Metadata,
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

// HasToolCallWaiter reports whether a tool execution is waiting for the
// durable tool_call item of toolCallID. It exists for tests outside this
// package that need to observe that wait.
func (l *Log) HasToolCallWaiter(toolCallID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.toolCalls[toolCallID]
	return ok
}
