package agent

import (
	"context"

	"github.com/jayyao97/zotigo/core/executor"
	"github.com/jayyao97/zotigo/core/tools"
)

// ToolCall captures everything a hook might want to read or rewrite
// about a single tool invocation. Before-side hook code may mutate
// Arguments (e.g. to inject defaults); the Executor field is the
// same value passed to the tool's Execute.
type ToolCall struct {
	Tool      tools.Tool
	Name      string
	Arguments string
	Executor  executor.Executor
}

// Next is the next link in the hook chain. The innermost Next invokes
// the tool itself; every hook between wraps its outer neighbor.
type Next func(ctx context.Context, call *ToolCall) (any, error)

// Hook is a middleware in the HTTP-handler-middleware style: given the
// next link, it returns a new link that does whatever wrapping work it
// likes. To short-circuit (rate limit, cache hit, dry-run, tracker
// refusal), simply don't call next. The returned (result, err) tuple
// is what outer hooks and ultimately the agent see.
type Hook func(next Next) Next

// WithHook registers a tool-call middleware. Hooks run in registration
// order on the way in (first registered wraps outermost) and reverse
// order on the way out.
func WithHook(h Hook) AgentOption {
	return func(a *Agent) {
		if h == nil {
			return
		}
		a.hooks = append(a.hooks, h)
	}
}

// buildHookChain composes all registered hooks around final. Final is
// typically the raw tool.Execute call. Callers build the chain freshly
// per dispatch — hooks slice is small and closure allocation is cheap
// relative to the tool call itself; keeping build inline avoids the
// synchronization dance a cached chain would need.
func buildHookChain(hooks []Hook, final Next) Next {
	chain := final
	for i := len(hooks) - 1; i >= 0; i-- {
		if hooks[i] == nil {
			continue
		}
		chain = hooks[i](chain)
	}
	return chain
}
