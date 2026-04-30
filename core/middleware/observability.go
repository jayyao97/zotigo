package middleware

import (
	"context"

	"github.com/jayyao97/zotigo/core/agent"
	"github.com/jayyao97/zotigo/core/observability"
)

// ToolSpan returns an agent middleware that opens a Langfuse span
// around every tool execution. The span sits as a sibling of the
// turn's generations under the trace, recording the tool's name,
// arguments, output, and any execution error.
//
// The middleware is a no-op when observer is nil or Noop — install it
// unconditionally and let the observer choice dictate whether spans
// actually get emitted.
func ToolSpan(observer observability.Observer) agent.Middleware {
	if observer == nil {
		return nil
	}
	if _, isNoop := observer.(observability.Noop); isNoop {
		return nil
	}
	return func(next agent.Next) agent.Next {
		return func(ctx context.Context, call *agent.ToolCall) (any, error) {
			ctx = observer.StartTool(ctx, call.Name, call.Arguments)
			result, err := next(ctx, call)
			observer.EndTool(ctx, result, err)
			return result, err
		}
	}
}
