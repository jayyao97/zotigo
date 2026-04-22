# Extension Points

Zotigo exposes two extension mechanisms that live at different layers of
the stack. They both used to be called "Hook", which was confusing
because they do very different things. Now they have distinct names —
and this doc is the short explanation of which one to reach for.

| | `agent.Middleware` | `runner.Listeners` |
|---|---|---|
| **Package** | `core/agent` | `core/runner` |
| **Layer** | Tool-call middleware | Runner event loop |
| **Granularity** | Every tool call | Per-turn milestones (before/after/pause/error) |
| **Signature** | `func(next Next) Next` — chain wrapper | `func(payload)` — one-shot callback |
| **Control flow** | Can short-circuit, rewrite args, change results | Observational only; return values ignored, panics swallowed |
| **Registration** | `agent.WithMiddleware(mw)` on the agent | `runner.WithListeners(ls)` on the runner |
| **Typical use** | read-before-edit check, safety gates, tool-arg rewriting, caching | session persistence, TUI updates, logging, metrics |

## When to use Middleware

You need to **change what a tool does** — or whether it runs at all.
Middleware wraps every `ToolCall` the agent dispatches, in the classic
HTTP-handler style (`Next func(ctx, *ToolCall) (any, error)`). Returning
without calling `next` short-circuits the tool; returning an error
surfaces to the agent exactly as if the tool itself had failed.

Example — the read-before-edit check in `core/middleware/tracker.go`:
it intercepts every `edit` / `write_file` call, verifies the agent
`read_file`'d the target first and that the file hasn't changed on
disk since, and returns an error (skipping execution entirely) if not.

```go
ag, _ := agent.New(profile, exec,
    agent.WithMiddleware(middleware.ReadTracker(readTracker)),
)
```

Middleware run in registration order on the way in (first registered
wraps outermost) and reverse order on the way out.

## When to use Listeners

You need to **observe what the runner is doing** without altering it.
Listeners are a flat struct of four optional callbacks fired at
turn-level milestones. They cannot block, redirect, or modify — their
return value is ignored and any panic is caught and forwarded to
`OnError` so a buggy listener can't take the turn down.

```go
r := runner.New(ag, tr, runner.WithListeners(runner.Listeners{
    AfterTurn: func(snap agent.Snapshot) { saveSession(snap) },
    OnPause:   func(snap agent.Snapshot) { flashApprovalBadge() },
    OnError:   func(err error)            { logf("runner error: %v", err) },
}))
```

## Rule of thumb

> If you want to change the outcome, register a Middleware.
> If you want to record that something happened, register a Listener.

If you find yourself trying to "stop a tool from running" via a
Listener, you actually want a Middleware; `panic`-ing in `BeforeTurn`
does not cancel the turn, it just triggers `OnError`. Conversely, if
you're using a Middleware only to log, you're paying for a closure
wrap on every tool call for no reason — a Listener is the right tool.
