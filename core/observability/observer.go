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
	"github.com/jayyao97/zotigo/core/tools"
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
//
// Structured wins when set, replacing the prose-shaped fields below.
// Use it for generations whose output is logically a typed value
// (classifier decisions, structured tool args) — backends will
// render the original JSON shape rather than a stringified summary.
type GenerationOutput struct {
	Text         string              // visible response text
	Reasoning    string              // assembled thinking/reasoning content
	ToolCalls    []protocol.ToolCall // tool calls the model decided to make
	FinishReason protocol.FinishReason

	// Structured, when non-nil, takes precedence over Text/Reasoning/
	// ToolCalls in the rendered output. Observers should marshal it
	// as the generation's output verbatim.
	Structured any
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
	// StartTurn opens a trace span. metadata is sticky environment
	// info that helps filter/group traces in the backend (working
	// directory, platform, provider/model, approval policy).
	// Pass nil when no metadata is meaningful.
	StartTurn(ctx context.Context, userMsg protocol.Message, metadata map[string]any) context.Context
	EndTurn(ctx context.Context, err error)

	// StartGeneration records the inputs to one LLM call. tools is the
	// list available to the model on this call; nil for calls that
	// pass no tools (e.g. the compactor's summarizer). metadata is
	// per-call detail (classifier attempt number, risk level, etc.) —
	// merged with the kind tag the observer always sets.
	StartGeneration(ctx context.Context, kind GenerationKind, model string, msgs []protocol.Message, tools []tools.Tool, metadata map[string]any) context.Context
	EndGeneration(ctx context.Context, output GenerationOutput, usage *protocol.Usage, err error)

	StartTool(ctx context.Context, name, arguments string) context.Context
	EndTool(ctx context.Context, output any, err error)

	// ResumeTrace copies the trace identity (and only the trace
	// identity) from saved onto target, returning a ctx that carries
	// target's cancellation and deadline plus saved's trace span.
	// Used to thread an in-flight turn's trace across an external
	// pause boundary (manual approval) without coupling the resumed
	// work to the original Run's cancellation.
	ResumeTrace(target, saved context.Context) context.Context

	// Close flushes any buffered events and shuts down background
	// goroutines. Idempotent.
	Close(ctx context.Context) error
}

// Noop is the default observer used when no backend is configured.
// All methods are zero-cost; they return the unchanged ctx and never
// allocate.
type Noop struct{}

func (Noop) StartTurn(ctx context.Context, _ protocol.Message, _ map[string]any) context.Context {
	return ctx
}
func (Noop) EndTurn(_ context.Context, _ error) {}
func (Noop) StartGeneration(ctx context.Context, _ GenerationKind, _ string, _ []protocol.Message, _ []tools.Tool, _ map[string]any) context.Context {
	return ctx
}
func (Noop) EndGeneration(_ context.Context, _ GenerationOutput, _ *protocol.Usage, _ error) {}
func (Noop) StartTool(ctx context.Context, _, _ string) context.Context                      { return ctx }
func (Noop) EndTool(_ context.Context, _ any, _ error)                                       {}
func (Noop) ResumeTrace(target, _ context.Context) context.Context                           { return target }
func (Noop) Close(_ context.Context) error                                                   { return nil }
