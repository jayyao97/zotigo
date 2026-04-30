package langfuse

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jayyao97/zotigo/core/observability"
	"github.com/jayyao97/zotigo/core/protocol"
	"github.com/jayyao97/zotigo/core/tools"
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
}

// New constructs a Langfuse-backed observer. Returns Noop when
// credentials are absent so callers don't need to special-case
// "observability disabled".
func New(cfg Config) observability.Observer {
	c := NewClient(cfg)
	if c == nil {
		return observability.Noop{}
	}
	return &observer{c: c}
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

func (o *observer) StartTurn(ctx context.Context, userMsg protocol.Message, metadata map[string]any) context.Context {
	traceID := newID()

	body := map[string]any{
		"id":        traceID,
		"timestamp": nowISO(),
		"name":      "turn",
		"input":     userMsg.String(),
	}
	if o.c.sessionID != "" {
		body["sessionId"] = o.c.sessionID
	}
	if len(metadata) > 0 {
		body["metadata"] = metadata
	}
	o.c.emit("trace-create", body)
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
	// Re-stamp sessionId on the update so a partial earlier emit
	// (e.g., trace-create dropped on a full buffer) still ends up
	// associated with the right session.
	if o.c.sessionID != "" {
		body["sessionId"] = o.c.sessionID
	}
	if err != nil {
		body["statusMessage"] = errCategoryLine(err)
		body["level"] = "ERROR"
	}
	o.c.emit("trace-create", body) // Langfuse merges by id; reusing trace-create is the documented update path
}

func (o *observer) StartGeneration(ctx context.Context, kind observability.GenerationKind, model string, msgs []protocol.Message, toolList []tools.Tool, metadata map[string]any) context.Context {
	traceID := traceIDFrom(ctx)
	if traceID == "" {
		return ctx
	}
	genID := newID()

	// input = {messages, tools} — the full prompt surface the model
	// saw on this call, including tool schemas (debugging "did the
	// model see the right shape?" needs the schema, not just names).
	input := map[string]any{
		"messages": summarizeMessages(msgs),
	}
	if len(toolList) > 0 {
		input["tools"] = summarizeTools(toolList)
	}

	// kind is sticky per-call; merge with caller metadata. caller
	// values win on collision because per-call metadata is more
	// specific than the bookkeeping tag.
	merged := map[string]any{"kind": string(kind)}
	for k, v := range metadata {
		merged[k] = v
	}

	body := map[string]any{
		"id":        genID,
		"traceId":   traceID,
		"name":      string(kind),
		"startTime": nowISO(),
		"model":     model,
		"input":     input,
		"metadata":  merged,
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
		"output":  renderOutput(output),
	}
	if usage != nil {
		u := usage.Normalized()
		// usage_details is Langfuse's modern free-form usage shape;
		// it surfaces cache breakdown in the UI AND lets the server
		// price cache reads at the discounted rate. The legacy
		// `usage` block is kept as a redundant fallback for
		// dashboards that don't yet read usage_details.
		body["usage_details"] = map[string]any{
			"input":                       u.InputTokens,
			"output":                      u.OutputTokens,
			"cache_read_input_tokens":     u.CacheReadInputTokens,
			"cache_creation_input_tokens": u.CacheCreationInputTokens,
			"total":                       u.TotalTokens,
		}
		body["usage"] = map[string]any{
			"input":  u.TotalInput(), // full prompt size, including cached
			"output": u.OutputTokens,
			"total":  u.TotalTokens,
			"unit":   "TOKENS",
		}
	}
	if err != nil {
		body["level"] = "ERROR"
		body["statusMessage"] = errCategoryLine(err)
	}
	o.c.emit("generation-update", body)
}

// renderOutput picks the display shape for a generation's output.
// Structured wins when set (classifier decisions, structured tool
// args); otherwise we collapse the streaming-assembled fields into
// a record so prose generations show up with text+reasoning+tool
// calls all visible.
func renderOutput(o observability.GenerationOutput) any {
	if o.Structured != nil {
		return o.Structured
	}
	return map[string]any{
		"text":          o.Text,
		"reasoning":     o.Reasoning,
		"tool_calls":    o.ToolCalls,
		"finish_reason": string(o.FinishReason),
	}
}

func (o *observer) StartTool(ctx context.Context, name, arguments string) context.Context {
	traceID := traceIDFrom(ctx)
	if traceID == "" {
		return ctx
	}
	spanID := newID()

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
	body := map[string]any{
		"id":      spanID,
		"endTime": nowISO(),
		"output":  toolOutputBody(output, err),
	}
	if err != nil {
		body["level"] = "ERROR"
		body["statusMessage"] = errCategoryLine(err)
	}
	o.c.emit("span-update", body)
}

// toolOutputBody picks what to render in the span's `output` slot.
// On success the tool's raw return value flows through. On error we
// surface the error message in the same place — without this, the
// most visible UI region would render as "undefined" while the
// actual diagnosis (build failure stderr, missing file, etc.) hides
// in the small statusMessage banner. Partial results are preserved
// when both an err and a non-nil result come back together.
//
// Outputs > 200KB collapse to a placeholder so a giant grep result
// or file dump doesn't blow the trace payload.
func toolOutputBody(output any, err error) any {
	if err != nil {
		body := map[string]any{"error": err.Error()}
		if output != nil {
			body["partial_result"] = output
		}
		return body
	}
	if b, mErr := json.Marshal(output); mErr == nil && len(b) < 200_000 {
		return json.RawMessage(b)
	}
	return output
}

func (o *observer) Close(ctx context.Context) error {
	return o.c.Close(ctx)
}

// --- Helpers ---

func nowISO() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// statusMessageCap is the maximum length of the short status string
// we put in Langfuse's `statusMessage` field. The UI renders that
// field in a fixed banner above the trace detail and in trace list
// rows — it's meant for a one-line categorization (HTTP status
// reason, error type), not the full error payload. Tool errors
// pipe entire stderr dumps through err.Error(); without this
// constraint, a single failed `go test` would push thousands of
// output lines into the banner and break scrolling. Full error
// text is preserved in `output` (see toolOutputBody), which is
// scrollable.
const statusMessageCap = 120

// errCategoryLine extracts a short categorical status from err for
// the Langfuse `statusMessage` slot. Takes the first line of
// err.Error() and caps it at statusMessageCap runes, on the
// assumption that error implementations put the most informative
// summary up front (Go's idiom is `fmt.Errorf("doing X: %w", err)`,
// and detail follows after newlines). Truncation respects rune
// boundaries so multi-byte characters don't get cut mid-codepoint.
func errCategoryLine(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) <= statusMessageCap {
		return s
	}
	cut := statusMessageCap
	for cut > 0 && cut < len(s) && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + " ..."
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

// summarizeTools projects each tool to {name, description, parameters}.
// Schemas roughly double the trace payload but they're necessary for
// debugging "did the model see the right tool surface" — a missing
// required field or wrong type often shows up first as a tool call
// with weird arguments, and only the schema explains why.
func summarizeTools(toolList []tools.Tool) []map[string]any {
	out := make([]map[string]any, 0, len(toolList))
	for _, t := range toolList {
		out = append(out, map[string]any{
			"name":        t.Name(),
			"description": t.Description(),
			"parameters":  t.Schema(),
		})
	}
	return out
}
