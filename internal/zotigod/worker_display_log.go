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
	wakeSync  func(context.Context) error
	barrier   func(context.Context) error
	delta     func(displayDeltaEvent)

	mu          sync.Mutex
	turnID      string
	turnStarted time.Time
	terminalID  string
	terminal    string
	block       *workerDisplayBlock
	subagents   map[string]*workerSubagentBlock
	toolCalls   map[string]chan struct{}
	toolCallErr map[string]error
	steering    map[string]commandResponse
	deltaMuted  bool
}

type workerDisplayBlock struct {
	id       string
	index    int
	partType string
	text     string
}

type workerSubagentBlock struct {
	block workerDisplayBlock
	info  zotigosession.DisplaySubagent
}

func newWorkerDisplayLog(sessionID string, items displayItemSource) *workerDisplayLog {
	return &workerDisplayLog{sessionID: sessionID, items: items}
}

func (l *workerDisplayLog) StartTurn(ctx context.Context) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.turnStarted = time.Now()
	l.turnID = fmt.Sprintf("turn_%d", l.turnStarted.UnixNano())
	l.terminalID = ""
	l.terminal = ""
	l.block = nil
	l.subagents = make(map[string]*workerSubagentBlock)
	l.toolCalls = make(map[string]chan struct{})
	l.toolCallErr = make(map[string]error)
	l.steering = make(map[string]commandResponse)
	l.deltaMuted = false
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

func (l *workerDisplayLog) TerminalStatus(turnID string) (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.terminal, turnID != "" && l.terminalID == turnID
}

func (l *workerDisplayLog) QueueSteering(command commandResponse) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.steering == nil {
		l.steering = make(map[string]commandResponse)
	}
	l.steering[command.ID] = command
}

func (l *workerDisplayLog) DiscardSteering(commandID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.steering, commandID)
}

func (l *workerDisplayLog) HasQueuedSteering(commandID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.steering[commandID]
	return ok
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

func (l *workerDisplayLog) ApprovalRequested(ctx context.Context, approval approvalRequest) (approvalRequest, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	item, err := l.appendItem(ctx, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemApprovalRequest,
		Approval: &zotigosession.DisplayApproval{
			ID:      approval.ID,
			TurnID:  approval.TurnID,
			Pending: copyPendingApprovals(approval.Pending),
		},
	})
	if err != nil {
		return approvalRequest{}, err
	}
	approval.CreatedAt = item.CreatedAt
	_, _ = l.appendItem(ctx, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnPaused,
		Turn: &zotigosession.DisplayTurn{
			ID:     approval.TurnID,
			Reason: "need_approval",
		},
	})
	l.block = nil
	return approval, nil
}

func (l *workerDisplayLog) ApprovalResolved(ctx context.Context, approval approvalRequest, decisions []zotigosession.DisplayApprovalDecision) (approvalRequest, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	item, err := l.appendItem(ctx, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemApprovalDecision,
		Approval: &zotigosession.DisplayApproval{
			ID:        approval.ID,
			TurnID:    approval.TurnID,
			Decisions: copyApprovalDecisions(decisions),
		},
	})
	if err != nil {
		return approvalRequest{}, err
	}
	return resolvedApprovalFromDecision(approval, decisions, item.CreatedAt), nil
}

func (l *workerDisplayLog) ResolvePendingApprovalsForOpenTurn(ctx context.Context, reason string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	items, _, err := l.items.LoadItems(ctx, l.sessionID)
	if err != nil {
		return false, err
	}
	turnID := lastOpenTurnID(items)
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
		if _, err := l.appendItem(ctx, zotigosession.DisplayItem{
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
	turnID := l.turnID
	_, err := l.appendItem(ctx, zotigosession.DisplayItem{
		Type: zotigosession.DisplayItemTurnInterrupted,
		Turn: &zotigosession.DisplayTurn{
			ID:         l.turnID,
			Status:     "interrupted",
			Reason:     reason,
			DurationMS: time.Since(l.turnStarted).Milliseconds(),
		},
	})
	if err == nil {
		l.terminalID = turnID
		l.terminal = "interrupted"
	}
	l.turnID = ""
	return err
}

func (l *workerDisplayLog) Fail(ctx context.Context, err error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.failLocked(ctx, err)
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
		if l.delta != nil && !l.deltaMuted {
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
	case protocol.EventTypeToolProgress:
		if event.ToolResult != nil && event.ToolResult.ToolCallID != "" && event.ToolResult.Text != "" && l.delta != nil && !l.deltaMuted {
			l.delta(displayDeltaEvent{
				ItemID:     "tool-progress:" + event.ToolResult.ToolCallID,
				Role:       string(protocol.RoleAssistant),
				PartType:   "tool_progress",
				Delta:      event.ToolResult.Text,
				ToolCallID: event.ToolResult.ToolCallID,
				ToolName:   event.ToolResult.ToolName,
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
		_, err := l.appendItem(ctx, zotigosession.DisplayItem{
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
		if len(event.SteeringIDs) > 0 && l.barrier != nil {
			if err := l.barrier(ctx); err != nil {
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
			images := make([]messageImage, 0, len(command.Steering.Images))
			for _, image := range command.Steering.Images {
				images = append(images, messageImage{
					MimeType:  image.MimeType,
					SizeBytes: image.SizeBytes,
					Width:     image.Width,
					Height:    image.Height,
					BlobPath:  image.BlobPath,
				})
			}
			item := displayMessageItem(zotigosession.DisplayItemSteeringMessage, command.Steering.Text, images)
			item.ID = command.ID
			item.CreatedAt = command.CreatedAt
			item.Turn = &zotigosession.DisplayTurn{ID: l.turnID}
			item.Command = &zotigosession.DisplayCommand{
				Type:   sessionCommandSteering,
				Text:   command.Steering.Text,
				Images: displayCommandImages(images),
				TurnID: l.turnID,
			}
			if _, err := l.items.AppendItem(ctx, l.sessionID, item); err != nil {
				return err
			}
			delete(l.steering, commandID)
		}
		if len(event.SteeringIDs) > 0 {
			if l.wakeSync != nil {
				if err := l.wakeSync(ctx); err != nil {
					l.deltaMuted = true
				}
			} else if l.wake != nil {
				l.wake(ctx)
			}
		}
		return nil
	case protocol.EventTypeFinish:
		if event.FinishReason == "need_approval" {
			return l.flushBlockLocked(ctx)
		}
		if err := l.flushBlockLocked(ctx); err != nil {
			return err
		}
		turnID := l.turnID
		_, err := l.appendItem(ctx, zotigosession.DisplayItem{
			Type: zotigosession.DisplayItemTurnCompleted,
			Turn: &zotigosession.DisplayTurn{
				ID:                   l.turnID,
				Status:               "completed",
				ProviderFinishReason: string(event.FinishReason),
				DurationMS:           time.Since(l.turnStarted).Milliseconds(),
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

func (l *workerDisplayLog) handleSubagentEventLocked(ctx context.Context, subagent *protocol.SubagentEvent) error {
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
			current = &workerSubagentBlock{
				block: workerDisplayBlock{id: "item_" + uuid.NewString(), index: child.Index, partType: partType},
				info:  info,
			}
			l.subagents[subagent.ToolCallID] = current
		}
		current.block.text += child.ContentPartDelta.Text
		if l.delta != nil && !l.deltaMuted {
			l.delta(displayDeltaEvent{
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
			l.subagents[subagent.ToolCallID] = &workerSubagentBlock{
				block: workerDisplayBlock{id: "item_" + uuid.NewString(), index: child.Index, partType: partType, text: child.ContentPart.Text},
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
		if child.ToolResult != nil && child.ToolResult.ToolCallID != "" && child.ToolResult.Text != "" && l.delta != nil && !l.deltaMuted {
			l.delta(displayDeltaEvent{
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

func (l *workerDisplayLog) flushSubagentBlockLocked(ctx context.Context, toolCallID string) error {
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

func (l *workerDisplayLog) appendSubagentItemLocked(ctx context.Context, itemID string, info zotigosession.DisplaySubagent, content []zotigosession.DisplayContentPart, errText string) error {
	_, err := l.appendItem(ctx, zotigosession.DisplayItem{
		ID: itemID, Type: zotigosession.DisplayItemAssistantMessage, Role: string(protocol.RoleAssistant),
		Content: content, Subagent: &info, Error: errText,
	})
	return err
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
	turnID := l.turnID
	_, appendErr := l.appendItem(ctx, zotigosession.DisplayItem{
		Type:  zotigosession.DisplayItemTurnFailed,
		Error: errText,
		Turn: &zotigosession.DisplayTurn{
			ID:         l.turnID,
			Status:     "failed",
			DurationMS: time.Since(l.turnStarted).Milliseconds(),
		},
	})
	if appendErr == nil {
		l.terminalID = turnID
		l.terminal = "failed"
	}
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
