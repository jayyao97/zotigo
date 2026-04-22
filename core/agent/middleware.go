package agent

import (
	"context"

	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/tools"
)

// This file defines agent-level tool-call Middleware — the HTTP-handler
// style extension point that wraps every tool invocation. It is
// deliberately distinct from runner.Listeners:
//
//   - agent.Middleware runs INSIDE a tool call. Middleware can rewrite
//     arguments, short-circuit with a synthetic result or error, and
//     observe both the request and the response. One registration =
//     "every tool call goes through me".
//   - runner.Listeners fire AROUND turn-level milestones (BeforeTurn,
//     AfterTurn, OnPause, OnError). They can observe but cannot alter
//     control flow — return values are ignored and panics are swallowed.
//     One registration = "tell me when these things happen".
//
// Rule of thumb: need to change what a tool does (or whether it runs at
// all) → Middleware. Need to record that a turn happened → Listeners.

// ToolCall captures everything middleware might want to read or rewrite
// about a single tool invocation. Before-side middleware may mutate
// Arguments (e.g. to inject defaults); the Executor field is the same
// value passed to the tool's Execute.
type ToolCall struct {
	Tool      tools.Tool
	Name      string
	Arguments string
	Executor  executor.Executor
}

// Next is the next link in the middleware chain. The innermost Next
// invokes the tool itself; every middleware between wraps its outer
// neighbor.
type Next func(ctx context.Context, call *ToolCall) (any, error)

// Middleware is a tool-call wrapper in the HTTP-handler-middleware
// style: given the next link, it returns a new link that does whatever
// wrapping work it likes. To short-circuit (rate limit, cache hit,
// dry-run, tracker refusal), simply don't call next. The returned
// (result, err) tuple is what outer middleware and ultimately the
// agent see.
type Middleware func(next Next) Next

// WithMiddleware registers a tool-call middleware. Middleware run in
// registration order on the way in (first registered wraps outermost)
// and reverse order on the way out.
func WithMiddleware(m Middleware) AgentOption {
	return func(a *Agent) {
		if m == nil {
			return
		}
		a.middlewares = append(a.middlewares, m)
	}
}

// buildMiddlewareChain composes all registered middleware around final.
// Final is typically the raw tool.Execute call. Callers build the chain
// freshly per dispatch — the middleware slice is small and closure
// allocation is cheap relative to the tool call itself; keeping build
// inline avoids the synchronization dance a cached chain would need.
func buildMiddlewareChain(middlewares []Middleware, final Next) Next {
	chain := final
	for i := len(middlewares) - 1; i >= 0; i-- {
		if middlewares[i] == nil {
			continue
		}
		chain = middlewares[i](chain)
	}
	return chain
}
