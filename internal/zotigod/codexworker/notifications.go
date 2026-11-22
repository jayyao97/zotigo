package codexworker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
	"github.com/jayyao97/zotigo/internal/codexapp"
	"github.com/jayyao97/zotigo/internal/hooks"
	"github.com/jayyao97/zotigo/internal/zotigod/sessionlog"
	"github.com/jayyao97/zotigo/internal/zotigod/wire"
	"github.com/jayyao97/zotigo/internal/zotigod/workerbase"
)

func (r *Runtime) HandleNotification(ctx context.Context, message codexapp.Message) error {
	switch message.Method {
	case "serverRequest/resolved":
		var resolved struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		if err := sonic.Unmarshal(message.Params, &resolved); err != nil {
			return err
		}
		for id, pending := range r.interactions {
			if string(pending.rpcID) != string(resolved.RequestID) {
				continue
			}
			req := pending.request
			if err := r.expireInteraction(req); err != nil {
				return err
			}
			now := time.Now().UTC()
			req.Status = sessionlog.InteractionStatusExpired
			req.ResolvedAt = &now
			delete(r.interactions, id)
			if r.writer != nil {
				r.writer.SendInteractionResult(ctx, wire.InteractionResult{Interaction: &req})
			}
			return nil
		}
		for id, pending := range r.approvals {
			if string(pending.rpcID) != string(resolved.RequestID) {
				continue
			}
			if err := r.expireApproval(pending.request, "Codex request was resolved elsewhere"); err != nil {
				return err
			}
			delete(r.approvals, id)
			if r.writer != nil {
				response := sessionlog.PublicApprovalRequest(sessionlog.ResolvedApprovalFromDecision(pending.request, []zotigosession.DisplayApprovalDecision{{
					ToolCallID: pending.request.Pending[0].ToolCallID, Approved: false, Reason: "Codex request was resolved elsewhere",
				}}, time.Now().UTC()))
				r.writer.SendApprovalResult(ctx, wire.ApprovalResult{Approval: &response})
			}
			return nil
		}
		return nil
	case "thread/tokenUsage/updated":
		var updated struct {
			ThreadID   string `json:"threadId"`
			TurnID     string `json:"turnId"`
			TokenUsage struct {
				Total              codexTokenUsage `json:"total"`
				Last               codexTokenUsage `json:"last"`
				ModelContextWindow int             `json:"modelContextWindow"`
			} `json:"tokenUsage"`
		}
		if err := sonic.Unmarshal(message.Params, &updated); err != nil {
			return err
		}
		if updated.ThreadID != r.threadID {
			return nil
		}
		totalUsage := updated.TokenUsage.Total.protocolUsage()
		if r.activeTurnID != "" && updated.TurnID == r.activeTurnID {
			usage := updated.TokenUsage.Last.protocolUsage()
			if r.hasTotalUsage {
				usage = workerbase.TurnUsageDelta(r.totalUsage, totalUsage)
			}
			r.turnUsage = r.turnUsage.Add(usage)
		}
		r.totalUsage = totalUsage
		r.hasTotalUsage = true
		if updated.TokenUsage.ModelContextWindow <= 0 || updated.TokenUsage.Last.InputTokens <= 0 {
			return nil
		}
		now := time.Now().UTC()
		return r.Append(zotigosession.DisplayItem{
			Type: zotigosession.DisplayItemContextUsageUpdated,
			ContextUsage: &zotigosession.DisplayContextUsage{
				Tokens: updated.TokenUsage.Last.InputTokens,
				Window: updated.TokenUsage.ModelContextWindow,
				Status: "available", Source: "provider", UpdatedAt: now,
			},
		})
	case "item/agentMessage/delta":
		var delta struct {
			TurnID string `json:"turnId"`
			ItemID string `json:"itemId"`
			Delta  string `json:"delta"`
		}
		if err := sonic.Unmarshal(message.Params, &delta); err != nil {
			return err
		}
		if delta.TurnID != r.activeTurnID || delta.ItemID == "" {
			return nil
		}
		if _, exists := r.messages[delta.ItemID]; !exists {
			r.messageOrder = append(r.messageOrder, delta.ItemID)
		}
		r.trackDisplayItem(delta.ItemID, "text")
		r.messages[delta.ItemID] += delta.Delta
		r.writer.SendDelta(wire.DisplayDelta{ItemID: delta.ItemID, Role: string(protocol.RoleAssistant), PartType: string(protocol.ContentTypeText), Delta: delta.Delta, BeforeInputID: r.displayItems[delta.ItemID].beforeInputID})
	case "item/started":
		var started codexItemNotification
		if err := sonic.Unmarshal(message.Params, &started); err != nil {
			return err
		}
		if !r.matchesActiveItem(started.ThreadID, started.TurnID, started.Item.ID) {
			return nil
		}
		switch started.Item.Type {
		case "agentMessage":
			r.trackDisplayItem(started.Item.ID, "text")
		case "reasoning":
			r.trackDisplayItem(started.Item.ID, "reasoning")
		}
		name, arguments, ok, err := codexToolCall(started.Item)
		if err != nil || !ok {
			return err
		}
		r.trackDisplayItem(started.Item.ID, "tool_progress")
		if r.toolNames == nil {
			r.toolNames = make(map[string]string)
		}
		r.toolNames[started.Item.ID] = name
		if r.toolArguments == nil {
			r.toolArguments = make(map[string]string)
		}
		if r.toolNativeNames == nil {
			r.toolNativeNames = make(map[string]string)
		}
		r.toolArguments[started.Item.ID] = arguments
		r.toolNativeNames[started.Item.ID] = started.Item.Type
		r.toolOrder = append(r.toolOrder, started.Item.ID)
		if err := r.Append(zotigosession.DisplayItem{
			ID: started.Item.ID, Type: zotigosession.DisplayItemAssistantMessage, Role: string(protocol.RoleAssistant),
			Content: []zotigosession.DisplayContentPart{{Type: "tool_call", ToolCall: &zotigosession.DisplayToolCall{
				ID: started.Item.ID, Name: name, Arguments: arguments,
			}}},
		}); err != nil {
			return err
		}
		return r.Append(zotigosession.DisplayItem{
			Type: zotigosession.DisplayItemToolExecutionStarted,
			ToolExecution: &zotigosession.DisplayToolExecution{
				TurnID: r.activeTurnID, ToolCallID: started.Item.ID, ToolName: name,
			},
		})
	case "item/commandExecution/outputDelta":
		var delta struct {
			ThreadID string `json:"threadId"`
			TurnID   string `json:"turnId"`
			ItemID   string `json:"itemId"`
			Delta    string `json:"delta"`
		}
		if err := sonic.Unmarshal(message.Params, &delta); err != nil {
			return err
		}
		if !r.matchesActiveItem(delta.ThreadID, delta.TurnID, delta.ItemID) || r.writer == nil {
			return nil
		}
		r.writer.SendDelta(wire.DisplayDelta{
			ItemID: delta.ItemID, Role: string(protocol.RoleAssistant), PartType: "tool_progress", Delta: delta.Delta,
			BeforeInputID: r.displayItems[delta.ItemID].beforeInputID,
			ToolCallID:    delta.ItemID, ToolName: r.toolNames[delta.ItemID],
		})
	case "item/completed":
		var completed codexItemNotification
		if err := sonic.Unmarshal(message.Params, &completed); err != nil {
			return err
		}
		if !r.matchesActiveItem(completed.ThreadID, completed.TurnID, completed.Item.ID) {
			return nil
		}
		defer delete(r.displayItems, completed.Item.ID)
		switch completed.Item.Type {
		case "agentMessage":
			text := completed.Item.Text
			if text == "" {
				text = r.messages[completed.Item.ID]
			}
			return r.persistAgentMessage(completed.Item.ID, text)
		case "reasoning":
			text := codexReasoningText(completed.Item)
			if text == "" {
				return nil
			}
			return r.Append(zotigosession.DisplayItem{
				ID: completed.Item.ID, Type: zotigosession.DisplayItemAssistantMessage, Role: string(protocol.RoleAssistant),
				Content: []zotigosession.DisplayContentPart{{Type: string(protocol.ContentTypeReasoning), Text: text}},
			})
		case "contextCompaction":
			return r.Append(zotigosession.DisplayItem{
				ID: completed.Item.ID, Type: zotigosession.DisplayItemContextCompacted,
				Turn: &zotigosession.DisplayTurn{ID: completed.TurnID},
			})
		case "imageGeneration":
			if !codexImageGenerationCompleted(completed.Item) {
				return nil
			}
			item, cleanup, err := codexGeneratedImageDisplayItem(context.Background(), r.store, r.cfg.SessionStoreRoot, r.cfg.SessionID, completed.TurnID, completed.Item, time.Now().UTC())
			if err != nil {
				if errors.Is(err, errCodexImageUnavailable) {
					return r.Append(codexUnavailableImageDisplayItem(completed.Item.ID, completed.TurnID, time.Now().UTC()))
				}
				return err
			}
			if err := r.Append(item); err != nil {
				cleanup()
				return err
			}
			return nil
		default:
			result, ok, err := codexToolResult(completed.Item, r.toolNames[completed.Item.ID])
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
			cleanup, err := persistCodexToolResultMedia(context.Background(), r.store, r.cfg.SessionStoreRoot, r.cfg.SessionID, result)
			if err != nil {
				return err
			}
			if err := r.Append(zotigosession.DisplayItem{
				Type: zotigosession.DisplayItemAssistantMessage, Role: string(protocol.RoleAssistant),
				Content: []zotigosession.DisplayContentPart{{Type: "tool_result", ToolResult: result}},
			}); err != nil {
				cleanup()
				return err
			}
			r.dispatchPostToolUse(ctx, completed.Item.ID, result, codexToolResultStatus(completed.Item.Status, result))
			r.forgetTool(completed.Item.ID)
			return nil
		}
	case "turn/completed":
		var completed struct {
			Turn struct {
				ID       string `json:"id"`
				Status   string `json:"status"`
				Duration int64  `json:"durationMs"`
				Error    *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"turn"`
		}
		if err := sonic.Unmarshal(message.Params, &completed); err != nil {
			return err
		}
		if completed.Turn.ID != r.activeTurnID {
			return nil
		}
		for _, itemID := range r.messageOrder {
			if err := r.Append(zotigosession.DisplayItem{
				ID: itemID, Type: zotigosession.DisplayItemAssistantMessage, Role: string(protocol.RoleAssistant),
				Content: []zotigosession.DisplayContentPart{{Type: string(protocol.ContentTypeText), Text: r.messages[itemID]}},
			}); err != nil {
				return err
			}
		}
		status := strings.ToLower(completed.Turn.Status)
		itemType := zotigosession.DisplayItemTurnCompleted
		switch status {
		case "failed":
			itemType = zotigosession.DisplayItemTurnFailed
		case "interrupted":
			itemType = zotigosession.DisplayItemTurnInterrupted
		}
		errorText := ""
		if completed.Turn.Error != nil {
			errorText = completed.Turn.Error.Message
		}
		duration := completed.Turn.Duration
		if duration == 0 && !r.turnStarted.IsZero() {
			duration = time.Since(r.turnStarted).Milliseconds()
		}
		if err := r.finishPendingTools("codex turn completed before tool result"); err != nil {
			return err
		}
		usage := r.turnUsage.Normalized()
		var usagePointer *protocol.Usage
		if usage != (protocol.Usage{}) {
			usagePointer = &usage
		}
		if err := r.Append(zotigosession.DisplayItem{
			Type: itemType, Error: errorText,
			Turn: &zotigosession.DisplayTurn{ID: r.activeTurnID, Status: status, DurationMS: duration, Usage: usagePointer},
		}); err != nil {
			return err
		}
		workerbase.DispatchTurnEndHook(r.hooks, r.cfg.SessionID, "codex", r.cfg.WorkingDirectory, r.activeTurnID, status, r.turnModel, r.turnUsage)
		workerbase.RecordTurnUsage(r.store, r.cfg.SessionID, "codex", r.activeTurnID, status, r.turnModel, r.activeRequestContext, r.turnUsage)
		r.activeTurnID = ""
		r.activeRequestContext = nil
		commandSequence := r.commandSequence
		r.turnStarted = time.Time{}
		r.turnModel = ""
		r.turnUsage = protocol.Usage{}
		r.messages = make(map[string]string)
		r.messageOrder = nil
		r.displayItems = nil
		r.toolNames = make(map[string]string)
		r.toolArguments = make(map[string]string)
		r.toolNativeNames = make(map[string]string)
		r.toolOrder = nil
		if r.writer != nil && !r.messagePending {
			if err := r.writer.SendIdle(ctx, wire.Idle{CommandSequence: commandSequence}); err != nil {
				return err
			}
		}
	}
	return nil
}

type codexTokenUsage struct {
	TotalTokens           int `json:"totalTokens"`
	InputTokens           int `json:"inputTokens"`
	CachedInputTokens     int `json:"cachedInputTokens"`
	CacheWriteInputTokens int `json:"cacheWriteInputTokens"`
	OutputTokens          int `json:"outputTokens"`
}

func (u codexTokenUsage) protocolUsage() protocol.Usage {
	inputTokens := u.InputTokens - u.CachedInputTokens - u.CacheWriteInputTokens
	if inputTokens < 0 {
		inputTokens = 0
	}
	return protocol.Usage{
		InputTokens:              inputTokens,
		OutputTokens:             u.OutputTokens,
		TotalTokens:              u.TotalTokens,
		CacheCreationInputTokens: u.CacheWriteInputTokens,
		CacheReadInputTokens:     u.CachedInputTokens,
	}
}

func codexReasoningText(item ThreadItem) string {
	parts := append([]string(nil), item.Summary...)
	if content, ok := item.Content.([]any); ok {
		for _, part := range content {
			if text, ok := part.(string); ok {
				parts = append(parts, text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func (r *Runtime) matchesActiveItem(threadID, turnID, itemID string) bool {
	return threadID == r.threadID && turnID == r.activeTurnID && itemID != ""
}

func (r *Runtime) persistAgentMessage(itemID, text string) error {
	delete(r.messages, itemID)
	for index, id := range r.messageOrder {
		if id == itemID {
			r.messageOrder = append(r.messageOrder[:index], r.messageOrder[index+1:]...)
			break
		}
	}
	if text == "" {
		return nil
	}
	return r.Append(zotigosession.DisplayItem{
		ID: itemID, Type: zotigosession.DisplayItemAssistantMessage, Role: string(protocol.RoleAssistant),
		Content: []zotigosession.DisplayContentPart{{Type: string(protocol.ContentTypeText), Text: text}},
	})
}

func (r *Runtime) forgetTool(itemID string) {
	delete(r.toolNames, itemID)
	delete(r.toolArguments, itemID)
	delete(r.toolNativeNames, itemID)
	for index, id := range r.toolOrder {
		if id == itemID {
			r.toolOrder = append(r.toolOrder[:index], r.toolOrder[index+1:]...)
			return
		}
	}
}

func (r *Runtime) finishPendingTools(reason string) error {
	for _, itemID := range r.toolOrder {
		name := r.toolNames[itemID]
		if name == "" {
			continue
		}
		if err := r.Append(zotigosession.DisplayItem{
			Type: zotigosession.DisplayItemAssistantMessage, Role: string(protocol.RoleAssistant),
			Content: []zotigosession.DisplayContentPart{{Type: "tool_result", ToolResult: &zotigosession.DisplayToolResult{
				ToolCallID: itemID, ToolName: name, ResultType: "text", Text: reason, Reason: reason, IsError: true,
			}}},
		}); err != nil {
			return err
		}
		r.dispatchPostToolUse(context.Background(), itemID, &zotigosession.DisplayToolResult{
			ToolCallID: itemID, ToolName: name, Text: reason, Reason: reason, IsError: true,
		}, "interrupted")
	}
	r.toolNames = make(map[string]string)
	r.toolArguments = make(map[string]string)
	r.toolNativeNames = make(map[string]string)
	r.toolOrder = nil
	return nil
}

func (r *Runtime) dispatchPostToolUse(ctx context.Context, itemID string, result *zotigosession.DisplayToolResult, status string) {
	if r == nil || r.hooks == nil || result == nil {
		return
	}
	name := result.ToolName
	nativeName := r.toolNativeNames[itemID]
	if nativeName == "" {
		nativeName = name
	}
	event := hooks.NewEvent(hooks.PostToolUse, r.cfg.SessionID, "codex", r.cfg.WorkingDirectory)
	event.TurnID = r.activeTurnID
	event.Tool = &hooks.ToolPayload{
		CallID: itemID, Name: name, NativeName: nativeName,
		Input:  hooks.DecodeToolInput(r.toolArguments[itemID]),
		Result: hooks.NewToolResult(status, result.Text),
	}
	r.hooks.Dispatch(ctx, event)
}

func codexToolResultStatus(itemStatus string, result *zotigosession.DisplayToolResult) string {
	switch strings.ToLower(strings.TrimSpace(itemStatus)) {
	case "declined", "denied":
		return "denied"
	case "interrupted", "cancelled", "canceled":
		return "interrupted"
	case "failed":
		return "failed"
	}
	if result == nil {
		return "failed"
	}
	if result.ResultType == string(protocol.ToolResultTypeExecutionDenied) || strings.EqualFold(result.Text, "declined") {
		return "denied"
	}
	if result.IsError {
		return "failed"
	}
	return "succeeded"
}

func codexToolCall(item ThreadItem) (string, string, bool, error) {
	name := ""
	arguments := any(nil)
	switch item.Type {
	case "commandExecution":
		name, arguments = codexCommandTool(item)
	case "fileChange":
		name, arguments = "apply_patch", map[string]any{"changes": item.Changes}
	case "mcpToolCall", "dynamicToolCall":
		name, arguments = item.Tool, item.Arguments
	case "collabAgentToolCall":
		name = codexCollabToolName(item.Tool)
		arguments = map[string]any{
			"description": item.Prompt, "prompt": item.Prompt, "model": item.Model,
			"reasoning_effort": item.ReasoningEffort, "receiver_thread_ids": item.ReceiverThreadIDs,
		}
	default:
		return "", "", false, nil
	}
	encoded, err := sonic.Marshal(arguments)
	return name, string(encoded), true, err
}

func codexCollabToolName(tool string) string {
	switch tool {
	case "spawnAgent":
		return "spawn_agent"
	case "sendInput":
		return "send_message"
	case "resumeAgent":
		return "resume_agent"
	case "closeAgent":
		return "close_agent"
	default:
		return tool
	}
}

func codexCommandTool(item ThreadItem) (string, any) {
	arguments := map[string]any{"command": item.Command, "cwd": item.CWD}
	if len(item.CommandActions) != 1 {
		return "shell", arguments
	}
	action := item.CommandActions[0]
	switch action.Type {
	case "read":
		arguments["path"] = action.Path
		return "read_file", arguments
	case "listFiles":
		arguments["path"] = action.Path
		return "glob", arguments
	case "search":
		arguments["path"] = action.Path
		arguments["query"] = action.Query
		return "grep", arguments
	default:
		return "shell", arguments
	}
}

func codexToolResult(item ThreadItem, name string) (*zotigosession.DisplayToolResult, bool, error) {
	if name == "" {
		name, _, _, _ = codexToolCall(item)
	}
	if name == "" {
		return nil, false, nil
	}
	result := &zotigosession.DisplayToolResult{ToolCallID: item.ID, ToolName: name, ResultType: "text"}
	switch item.Type {
	case "commandExecution":
		if item.AggregatedOutput != nil {
			result.Text = *item.AggregatedOutput
		}
		result.JSON = map[string]any{"exit_code": item.ExitCode, "duration_ms": item.DurationMS, "status": item.Status}
		result.IsError = codexFailedStatus(item.Status) || item.ExitCode != nil && *item.ExitCode != 0
	case "fileChange":
		result.JSON = map[string]any{"changes": item.Changes, "status": item.Status}
		result.Text = item.Status
		result.IsError = strings.EqualFold(item.Status, "failed") || strings.EqualFold(item.Status, "declined")
	case "mcpToolCall":
		content, err := codexMCPToolResultContent(item.Result)
		result.Content = content
		if len(result.Content) > 0 {
			result.JSON = codexMCPResultWithoutContent(item.Result)
		} else {
			result.JSON = item.Result
		}
		if err != nil {
			result.Reason = err.Error()
		}
		result.IsError = codexFailedStatus(item.Status)
		if item.Error != nil {
			result.Text = item.Error.Message
			result.Reason = item.Error.Message
			result.IsError = true
		}
	case "dynamicToolCall":
		result.Content = codexDynamicToolResultContent(item.ContentItems)
		if len(result.Content) == 0 {
			result.JSON = item.ContentItems
		}
		result.IsError = codexFailedStatus(item.Status) || item.Success != nil && !*item.Success
	case "collabAgentToolCall":
		result.JSON = map[string]any{"receiver_thread_ids": item.ReceiverThreadIDs, "agents_states": item.AgentsStates, "status": item.Status}
		result.IsError = codexFailedStatus(item.Status)
	default:
		return nil, false, nil
	}
	return result, true, nil
}

func codexFailedStatus(status string) bool {
	return strings.EqualFold(status, "failed") || strings.EqualFold(status, "declined")
}
