// Package observability defines the agent's hooks for capturing
// turn / generation / tool spans into an external backend (Langfuse).
//
// The Observer interface is the only thing core/agent depends on; the
// concrete Langfuse implementation lives in subpackage langfuse and
// can be swapped or stubbed without touching the agent. A Noop
// observer is the default — when no backend is configured, every hook
// is a zero-cost no-op so observability never affects the hot path.
package observability

import (
	"context"

	"github.com/jayyao97/zotigo/core/protocol"
)

// GenerationKind tags what triggered an LLM call so the trace UI can
// show where token cost actually goes (visible main-turn answers vs
// hidden classifier / compactor calls). Without this distinction a
// single user turn can fan out into 5+ generations with no clue which
// did what.
type GenerationKind string

const (
	GenerationMain       GenerationKind = "main"
	GenerationClassifier GenerationKind = "classifier"
	GenerationCompactor  GenerationKind = "compactor"
)

// GenerationOutput is what the model produced over a streaming call.
// Assembled by the agent from the protocol.Event stream — observers
// see the same normalized shape regardless of upstream provider, so
// each provider needs zero observability code.
type GenerationOutput struct {
	Text         string              // visible response text
	Reasoning    string              // assembled thinking/reasoning content
	ToolCalls    []protocol.ToolCall // tool calls the model decided to make
	FinishReason protocol.FinishReason
}

// Observer captures lifecycle events for one user turn:
//
//	StartTurn → StartGeneration{*,Tool,*} ... → EndGeneration → EndTool → EndTurn
//
// Methods that return context.Context inject the new span's ID into
// ctx; downstream callers must use the returned ctx so nested spans
// can find their parent. The matching End* call takes that returned
// ctx — pairing is positional, not name-based.
//
// Implementations MUST tolerate Close-after-error and Close-without-Open
// (best-effort flush). Close is the hard cut-over: anything still in
// the buffer after its timeout is dropped, never blocks process exit.
type Observer interface {
	StartTurn(ctx context.Context, userMsg protocol.Message) context.Context
	EndTurn(ctx context.Context, err error)

	StartGeneration(ctx context.Context, kind GenerationKind, model string, msgs []protocol.Message) context.Context
	EndGeneration(ctx context.Context, output GenerationOutput, usage *protocol.Usage, err error)

	StartTool(ctx context.Context, name, arguments string) context.Context
	EndTool(ctx context.Context, output any, err error)

	// Close flushes any buffered events and shuts down background
	// goroutines. Idempotent.
	Close(ctx context.Context) error
}

// Noop is the default observer used when no backend is configured.
// All methods are zero-cost; they return the unchanged ctx and never
// allocate.
type Noop struct{}

func (Noop) StartTurn(ctx context.Context, _ protocol.Message) context.Context { return ctx }
func (Noop) EndTurn(_ context.Context, _ error)                                {}
func (Noop) StartGeneration(ctx context.Context, _ GenerationKind, _ string, _ []protocol.Message) context.Context {
	return ctx
}
func (Noop) EndGeneration(_ context.Context, _ GenerationOutput, _ *protocol.Usage, _ error) {}
func (Noop) StartTool(ctx context.Context, _, _ string) context.Context                      { return ctx }
func (Noop) EndTool(_ context.Context, _ any, _ error)                                       {}
func (Noop) Close(_ context.Context) error                                                   { return nil }
