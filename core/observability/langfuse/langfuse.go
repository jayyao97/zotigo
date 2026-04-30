package langfuse

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/jayyao97/zotigo/core/observability"
	"github.com/jayyao97/zotigo/core/protocol"
)

// observer implements observability.Observer by emitting Langfuse
// trace / generation / span events.
//
// Span ordering is tracked via context.Context: each Start* method
// stashes its newly minted observation ID under a typed key, and the
// matching End* call recovers it from the ctx the caller threads
// through. This makes the parent/child shape of nested spans
// (turn → generation → tool) implicit in the call graph rather than
// requiring observers to track stacks.
type observer struct {
	c *client

	// Span start times keyed by observation ID. Langfuse expects an
	// endTime relative to a startTime, but its update events carry
	// only endTime — we compute the duration locally so the API
	// doesn't have to re-derive it. Lock contention here is bounded
	// because spans are short-lived and per-turn.
	mu     sync.Mutex
	starts map[string]time.Time
}

// New constructs a Langfuse-backed observer. Returns Noop when
// credentials are absent so callers don't need to special-case
// "observability disabled".
func New(cfg Config) observability.Observer {
	c := NewClient(cfg)
	if c == nil {
		return observability.Noop{}
	}
	return &observer{c: c, starts: map[string]time.Time{}}
}

// --- Context-keyed IDs ---
//
// In our agent the LLM call (generation) and tool executions are
// sequential phases of one turn, NOT nested — generations produce
// tool_calls, tools run, then a new generation processes the tool
// results. Both phases sit as siblings under the trace in Langfuse,
// not in a parent-child relationship.
//
// We therefore use distinct ctx keys per span type so each Start*
// can return a ctx that carries its own ID forward to the matching
// End* without overwriting another span's ID. Trace ID stays the
// stable parent reference for both generations and tool spans.

type ctxKey int

const (
	keyTraceID ctxKey = iota
	keyGenID
	keyToolID
)

func withTraceID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyTraceID, id)
}

func traceIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(keyTraceID).(string)
	return v
}

func withGenID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyGenID, id)
}

func genIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(keyGenID).(string)
	return v
}

func withToolID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyToolID, id)
}

func toolIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(keyToolID).(string)
	return v
}

// --- Lifecycle ---

func (o *observer) StartTurn(ctx context.Context, userMsg protocol.Message) context.Context {
	traceID := newID()
	o.recordStart(traceID)

	o.c.emit("trace-create", map[string]any{
		"id":        traceID,
		"timestamp": nowISO(),
		"name":      "turn",
		"input":     userMsg.String(),
	})
	return withTraceID(ctx, traceID)
}

func (o *observer) EndTurn(ctx context.Context, err error) {
	traceID := traceIDFrom(ctx)
	if traceID == "" {
		return
	}
	body := map[string]any{
		"id": traceID,
	}
	if err != nil {
		body["statusMessage"] = err.Error()
		body["level"] = "ERROR"
	}
	o.c.emit("trace-create", body) // Langfuse merges by id; reusing trace-create is the documented update path
	o.consumeStart(traceID)
}

func (o *observer) StartGeneration(ctx context.Context, kind observability.GenerationKind, model string, msgs []protocol.Message) context.Context {
	traceID := traceIDFrom(ctx)
	if traceID == "" {
		return ctx
	}
	genID := newID()
	o.recordStart(genID)

	body := map[string]any{
		"id":        genID,
		"traceId":   traceID,
		"name":      string(kind),
		"startTime": nowISO(),
		"model":     model,
		"input":     summarizeMessages(msgs),
		"metadata": map[string]string{
			"kind": string(kind),
		},
	}
	o.c.emit("generation-create", body)
	return withGenID(ctx, genID)
}

func (o *observer) EndGeneration(ctx context.Context, output observability.GenerationOutput, usage *protocol.Usage, err error) {
	genID := genIDFrom(ctx)
	if genID == "" {
		return
	}
	body := map[string]any{
		"id":      genID,
		"endTime": nowISO(),
		"output": map[string]any{
			"text":          output.Text,
			"reasoning":     output.Reasoning,
			"tool_calls":    output.ToolCalls,
			"finish_reason": string(output.FinishReason),
		},
	}
	if usage != nil {
		u := usage.Normalized()
		body["usage"] = map[string]any{
			"input":        u.InputTokens,
			"output":       u.OutputTokens,
			"total":        u.TotalTokens,
			"cache_read":   u.CacheReadInputTokens,
			"cache_create": u.CacheCreationInputTokens,
			"unit":         "TOKENS",
		}
	}
	if err != nil {
		body["level"] = "ERROR"
		body["statusMessage"] = err.Error()
	}
	o.c.emit("generation-update", body)
	o.consumeStart(genID)
}

func (o *observer) StartTool(ctx context.Context, name, arguments string) context.Context {
	traceID := traceIDFrom(ctx)
	if traceID == "" {
		return ctx
	}
	spanID := newID()
	o.recordStart(spanID)

	body := map[string]any{
		"id":        spanID,
		"traceId":   traceID,
		"name":      name,
		"startTime": nowISO(),
		"input":     arguments,
	}
	o.c.emit("span-create", body)
	return withToolID(ctx, spanID)
}

func (o *observer) EndTool(ctx context.Context, output any, err error) {
	spanID := toolIDFrom(ctx)
	if spanID == "" {
		return
	}
	// Tool outputs vary wildly (text, JSON, byte blobs); marshal best-effort.
	outRendered := output
	if b, mErr := json.Marshal(output); mErr == nil && len(b) < 200_000 {
		outRendered = json.RawMessage(b)
	}
	body := map[string]any{
		"id":      spanID,
		"endTime": nowISO(),
		"output":  outRendered,
	}
	if err != nil {
		body["level"] = "ERROR"
		body["statusMessage"] = err.Error()
	}
	o.c.emit("span-update", body)
	o.consumeStart(spanID)
}

func (o *observer) Close(ctx context.Context) error {
	return o.c.Close(ctx)
}

// --- Helpers ---

func (o *observer) recordStart(id string) {
	o.mu.Lock()
	o.starts[id] = time.Now()
	o.mu.Unlock()
}

func (o *observer) consumeStart(id string) {
	o.mu.Lock()
	delete(o.starts, id)
	o.mu.Unlock()
}

func nowISO() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// summarizeMessages produces the JSON shape Langfuse renders nicely
// in its UI: a list of {role, content} objects. Tool calls and rich
// content are flattened into a stringified content field; the goal
// is human readability in the trace viewer, not perfect round-trip
// fidelity.
func summarizeMessages(msgs []protocol.Message) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, map[string]any{
			"role":    string(m.Role),
			"content": m.String(),
		})
	}
	return out
}
