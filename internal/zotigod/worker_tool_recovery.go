package zotigod

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/protocol"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

const recoveryToolArgumentsLimit = 2048

func workerToolExecutionMiddleware(display *workerDisplayLog) agent.Middleware {
	return func(next agent.Next) agent.Next {
		return func(ctx context.Context, call *agent.ToolCall) (any, error) {
			if err := display.ToolExecutionStarted(ctx, call.ToolCallID, call.Name); err != nil {
				return nil, fmt.Errorf("record tool execution start: %w", err)
			}
			return next(ctx, call)
		}
	}
}

func recoverInterruptedToolExecutions(ctx context.Context, store zotigosession.Store, sess *zotigosession.Session) error {
	items, exists, err := store.ListDisplayItems(ctx, sess.ID)
	if err != nil || !exists {
		return err
	}
	recovery := latestUnknownToolExecutions(items)
	if len(recovery.unknown) == 0 {
		return nil
	}

	newUnknown := recovery.unknown[:0]
	for _, execution := range recovery.unknown {
		if !historyHasToolRecovery(sess.AgentSnapshot.History, execution.execution.TurnID, execution.call.ID) {
			newUnknown = append(newUnknown, execution)
		}
	}
	if len(newUnknown) == 0 {
		return nil
	}

	recovery.unknown = newUnknown
	recoveryMessage := toolRecoveryMessage(sess.AgentSnapshot, recovery)
	sess.AgentSnapshot.History = append(sess.AgentSnapshot.History, recoveryMessage)
	sess.AgentSnapshot.State = agent.StateIdle
	sess.AgentSnapshot.PendingActions = nil
	sess.AgentSnapshot.DeferredActions = nil
	return store.Put(ctx, sess)
}

type unknownToolExecution struct {
	execution zotigosession.DisplayToolExecution
	call      zotigosession.DisplayToolCall
}

type toolExecutionRecovery struct {
	turnID  string
	unknown []unknownToolExecution
	results map[string]protocol.ToolResult
}

func latestUnknownToolExecutions(items []zotigosession.DisplayItem) toolExecutionRecovery {
	type turnExecutions struct {
		id      string
		calls   map[string]zotigosession.DisplayToolCall
		started []zotigosession.DisplayToolExecution
		results map[string]protocol.ToolResult
	}
	newTurn := func(id string) *turnExecutions {
		return &turnExecutions{
			id:      id,
			calls:   make(map[string]zotigosession.DisplayToolCall),
			results: make(map[string]protocol.ToolResult),
		}
	}
	finish := func(turn *turnExecutions) toolExecutionRecovery {
		if turn == nil {
			return toolExecutionRecovery{}
		}
		unknown := make([]unknownToolExecution, 0, len(turn.started))
		seen := make(map[string]struct{})
		for _, execution := range turn.started {
			if _, ok := turn.results[execution.ToolCallID]; ok {
				continue
			}
			if _, ok := seen[execution.ToolCallID]; ok {
				continue
			}
			seen[execution.ToolCallID] = struct{}{}
			call := turn.calls[execution.ToolCallID]
			if call.ID == "" {
				call = zotigosession.DisplayToolCall{ID: execution.ToolCallID, Name: execution.ToolName}
			}
			unknown = append(unknown, unknownToolExecution{execution: execution, call: call})
		}
		if len(unknown) == 0 {
			return toolExecutionRecovery{}
		}
		return toolExecutionRecovery{turnID: turn.id, unknown: unknown, results: turn.results}
	}

	var current *turnExecutions
	var latest toolExecutionRecovery
	for _, item := range items {
		if item.Type == zotigosession.DisplayItemTurnStarted && item.Turn != nil {
			if current != nil {
				latest = finish(current)
			}
			current = newTurn(item.Turn.ID)
			continue
		}
		if current == nil {
			continue
		}
		for _, part := range item.Content {
			if part.ToolCall != nil {
				current.calls[part.ToolCall.ID] = *part.ToolCall
			}
			if part.ToolResult != nil {
				current.results[part.ToolResult.ToolCallID] = protocolToolResult(part.ToolResult)
			}
		}
		if item.Type == zotigosession.DisplayItemToolExecutionStarted && item.ToolExecution != nil && item.ToolExecution.TurnID == current.id {
			current.started = append(current.started, *item.ToolExecution)
		}
		if item.Turn != nil && item.Turn.ID == current.id {
			switch item.Type {
			case zotigosession.DisplayItemTurnCompleted, zotigosession.DisplayItemTurnFailed, zotigosession.DisplayItemTurnInterrupted:
				latest = finish(current)
				current = nil
			}
		}
	}
	if current != nil {
		latest = finish(current)
	}
	return latest
}

func toolRecoveryHint(executions []unknownToolExecution) string {
	var text strings.Builder
	text.WriteString("<system-reminder>\n")
	text.WriteString("A previous Zotigo worker stopped after starting the following tool calls but before recording durable results. Their outcomes are unknown: the operation may have completed and produced side effects. Do not repeat these calls automatically. First verify the current state with a read-only operation when possible, or ask the user before retrying.\n")
	for _, execution := range executions {
		fmt.Fprintf(&text, "- %s tool=%s tool_call_id=%s", toolRecoveryMarker(execution.execution.TurnID, execution.call.ID), execution.call.Name, execution.call.ID)
		if arguments := strings.TrimSpace(execution.call.Arguments); arguments != "" {
			if len(arguments) > recoveryToolArgumentsLimit {
				arguments = arguments[:recoveryToolArgumentsLimit] + "..."
			}
			fmt.Fprintf(&text, " arguments=%s", arguments)
		}
		text.WriteByte('\n')
	}
	text.WriteString("</system-reminder>")
	return text.String()
}

func toolRecoveryMessage(snapshot agent.Snapshot, recovery toolExecutionRecovery) protocol.Message {
	unknownByID := make(map[string]unknownToolExecution, len(recovery.unknown))
	for _, execution := range recovery.unknown {
		unknownByID[execution.call.ID] = execution
	}
	if snapshot.State != agent.StatePaused || len(snapshot.History) == 0 {
		return protocol.NewUserMessage(toolRecoveryHint(recovery.unknown))
	}
	message := snapshot.History[len(snapshot.History)-1]
	if message.Role != protocol.RoleAssistant {
		return protocol.NewUserMessage(toolRecoveryHint(recovery.unknown))
	}
	calls := make([]protocol.ToolCall, 0)
	containsUnknown := false
	for _, part := range message.Content {
		if part.ToolCall == nil {
			continue
		}
		calls = append(calls, *part.ToolCall)
		if _, ok := unknownByID[part.ToolCall.ID]; ok {
			containsUnknown = true
		}
	}
	if !containsUnknown {
		return protocol.NewUserMessage(toolRecoveryHint(recovery.unknown))
	}

	results := make([]protocol.ToolResult, 0, len(calls))
	for _, call := range calls {
		if result, ok := recovery.results[call.ID]; ok {
			results = append(results, result)
			continue
		}
		if _, ok := unknownByID[call.ID]; ok {
			results = append(results, protocol.NewTextToolResult(call.ID, unknownToolResultText(unknownByID[call.ID]), true))
			results[len(results)-1].ToolName = call.Name
			continue
		}
		result := protocol.NewTextToolResult(call.ID, "The previous worker stopped before this tool execution began. The tool was not executed.", true)
		result.ToolName = call.Name
		results = append(results, result)
	}
	return protocol.NewToolMessage(results)
}

func protocolToolResult(result *zotigosession.DisplayToolResult) protocol.ToolResult {
	if result == nil {
		return protocol.ToolResult{}
	}
	return protocol.ToolResult{
		ToolCallID: result.ToolCallID,
		ToolName:   result.ToolName,
		Type:       protocol.ToolResultType(result.ResultType),
		Text:       result.Text,
		JSON:       result.JSON,
		Reason:     result.Reason,
		Content:    protocolToolResultContent(result.Content),
		IsError:    result.IsError,
	}
}

func protocolToolResultContent(content []zotigosession.DisplayToolResultContentPart) []protocol.ToolResultContentPart {
	if len(content) == 0 {
		return nil
	}
	parts := make([]protocol.ToolResultContentPart, 0, len(content))
	for _, part := range content {
		converted := protocol.ToolResultContentPart{Type: protocol.ContentType(part.Type), Text: part.Text}
		if part.Image != nil {
			converted.Image = &protocol.MediaPart{
				Data:      part.Image.Data,
				URL:       part.Image.URL,
				FileID:    part.Image.FileID,
				MediaType: part.Image.MediaType,
			}
		}
		parts = append(parts, converted)
	}
	return parts
}

func unknownToolResultText(execution unknownToolExecution) string {
	return fmt.Sprintf(
		"%s The previous worker stopped after this tool started but before a durable result was recorded. The outcome is unknown and the operation may have produced side effects. Do not repeat it automatically; verify current state with a read-only operation or ask the user before retrying.",
		toolRecoveryMarker(execution.execution.TurnID, execution.call.ID),
	)
}

func historyHasToolRecovery(history []protocol.Message, turnID string, toolCallID string) bool {
	marker := toolRecoveryMarker(turnID, toolCallID)
	for _, message := range history {
		if strings.Contains(message.String(), marker) {
			return true
		}
	}
	return false
}

func toolRecoveryMarker(turnID string, toolCallID string) string {
	key := turnID + "\x00" + toolCallID
	return "[zotigo-tool-recovery:" + base64.RawURLEncoding.EncodeToString([]byte(key)) + "]"
}
