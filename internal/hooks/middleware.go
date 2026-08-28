package hooks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jayyao97/zotigo/core/agent"
)

const maxResultSummaryBytes = 4 * 1024

func NewToolResult(status string, summary string) *ToolResultPayload {
	return &ToolResultPayload{Status: status, Summary: truncateSummary(summary)}
}

type ToolContext struct {
	SessionID string
	Agent     string
	CWD       string
	TurnID    func() string
}

func ToolMiddleware(dispatcher *Dispatcher, metadata ToolContext) agent.Middleware {
	if dispatcher == nil || (!dispatcher.HasHandlers(PreToolUse) && !dispatcher.HasHandlers(PostToolUse)) {
		return nil
	}
	return func(next agent.Next) agent.Next {
		return func(ctx context.Context, call *agent.ToolCall) (any, error) {
			turnID := ""
			if metadata.TurnID != nil {
				turnID = metadata.TurnID()
			}
			tool := ToolPayload{
				CallID: call.ToolCallID, Name: call.Name, NativeName: call.Name,
				Input: DecodeToolInput(call.Arguments),
			}
			if dispatcher.HasHandlers(PreToolUse) {
				event := newToolEvent(PreToolUse, metadata, turnID, tool)
				decision := dispatcher.Dispatch(ctx, event)
				if decision.Denied {
					tool.Result = NewToolResult("denied", decision.Reason)
					dispatcher.Dispatch(context.WithoutCancel(ctx), newToolEvent(PostToolUse, metadata, turnID, tool))
					return nil, agent.DenyToolExecution(decision.Reason)
				}
			}

			result, err := next(ctx, call)
			if dispatcher.HasHandlers(PostToolUse) {
				tool.Result = NewToolResult(toolResultStatus(err), toolResultSummary(result, err))
				dispatcher.Dispatch(context.WithoutCancel(ctx), newToolEvent(PostToolUse, metadata, turnID, tool))
			}
			return result, err
		}
	}
}

func newToolEvent(eventName EventName, metadata ToolContext, turnID string, tool ToolPayload) Event {
	event := NewEvent(eventName, metadata.SessionID, metadata.Agent, metadata.CWD)
	event.TurnID = turnID
	event.Tool = &tool
	return event
}

func toolResultStatus(err error) string {
	if err == nil {
		return "succeeded"
	}
	if agent.IsToolExecutionDenied(err) {
		return "denied"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "interrupted"
	}
	return "failed"
}

func toolResultSummary(result any, err error) string {
	if err != nil {
		return err.Error()
	}
	switch value := result.(type) {
	case agent.ToolOutputWithMetadata:
		return value.ToolOutputText()
	case string:
		return value
	case []string:
		return strings.Join(value, "\n")
	default:
		return fmt.Sprint(value)
	}
}

func truncateSummary(value string) string {
	value = strings.TrimSpace(strings.ToValidUTF8(value, "�"))
	if len(value) <= maxResultSummaryBytes {
		return value
	}
	value = value[:maxResultSummaryBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + "..."
}
