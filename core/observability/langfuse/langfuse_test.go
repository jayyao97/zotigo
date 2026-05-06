package langfuse

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jayyao97/zotigo/core/observability"
	"github.com/jayyao97/zotigo/core/protocol"
)

// captureServer is a minimal stand-in for Langfuse's ingestion endpoint.
// It records every batch posted so tests can assert event shape, count,
// and ordering without hitting the network. Handler is concurrency-safe
// because the client's flush goroutine may post during test teardown.
type captureServer struct {
	mu      sync.Mutex
	batches [][]event
	auth    string
}

func newCaptureServer(t *testing.T) (*httptest.Server, *captureServer) {
	t.Helper()
	cs := &captureServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Batch []event `json:"batch"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("server: decode body: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		cs.mu.Lock()
		cs.batches = append(cs.batches, payload.Batch)
		cs.auth = r.Header.Get("Authorization")
		cs.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	return srv, cs
}

func (c *captureServer) allEvents() []event {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []event
	for _, b := range c.batches {
		out = append(out, b...)
	}
	return out
}

func TestObserver_FullTurnLifecycle_EmitsExpectedEvents(t *testing.T) {
	srv, cs := newCaptureServer(t)
	defer srv.Close()

	obs := New(Config{
		Host:          srv.URL,
		PublicKey:     "pk-test",
		SecretKey:     "sk-test",
		FlushInterval: 50 * time.Millisecond,
	})

	ctx := context.Background()
	user := protocol.NewUserMessage("hello")

	ctx = obs.StartTurn(ctx, user, nil)
	genCtx := obs.StartGeneration(ctx, observability.GenerationMain, "claude-test", []protocol.Message{user}, nil, nil)
	obs.EndGeneration(genCtx, observability.GenerationOutput{
		Text:         "hi back",
		FinishReason: protocol.FinishReasonStop,
	}, &protocol.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}, nil)

	toolCtx := obs.StartTool(ctx, "read_file", `{"path":"foo"}`)
	obs.EndTool(toolCtx, "file contents", nil)

	obs.EndTurn(ctx, nil)

	// Force a flush by closing — Close drains the queue synchronously.
	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := obs.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	events := cs.allEvents()

	wantTypes := []string{
		"trace-create",      // StartTurn
		"generation-create", // StartGeneration
		"generation-update", // EndGeneration
		"span-create",       // StartTool
		"span-update",       // EndTool
		"trace-create",      // EndTurn (Langfuse merges by id)
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("got %d events, want %d: %+v", len(events), len(wantTypes), events)
	}
	for i, want := range wantTypes {
		if events[i].Type != want {
			t.Errorf("event[%d].Type = %q, want %q", i, events[i].Type, want)
		}
	}

	// Auth header should be HTTP Basic with our test creds, base64-encoded.
	if !strings.HasPrefix(cs.auth, "Basic ") {
		t.Errorf("Authorization = %q, want Basic ...", cs.auth)
	}
}

func TestObserver_GenerationAndToolAreSiblings(t *testing.T) {
	// Generations and tool spans must hang off the trace, not nest under
	// each other — our agent runs them sequentially, not nested. If the
	// observer accidentally pushed parentObservationId to "the previous
	// span", every tool span would appear as a child of the prior
	// generation in the trace UI.
	srv, cs := newCaptureServer(t)
	defer srv.Close()

	obs := New(Config{
		Host:          srv.URL,
		PublicKey:     "pk",
		SecretKey:     "sk",
		FlushInterval: 50 * time.Millisecond,
	})

	ctx := context.Background()
	ctx = obs.StartTurn(ctx, protocol.NewUserMessage("u"), nil)

	genCtx := obs.StartGeneration(ctx, observability.GenerationMain, "m", nil, nil, nil)
	obs.EndGeneration(genCtx, observability.GenerationOutput{}, nil, nil)

	// Critical: pass the trace ctx (not genCtx) to StartTool so the tool
	// span sits beside the generation, not inside it.
	toolCtx := obs.StartTool(ctx, "t", "{}")
	obs.EndTool(toolCtx, nil, nil)

	obs.EndTurn(ctx, nil)

	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := obs.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for _, ev := range cs.allEvents() {
		body, ok := ev.Body.(map[string]any)
		if !ok {
			continue
		}
		// Neither generation-create nor span-create should set
		// parentObservationId — both are direct children of the trace.
		if _, hasParent := body["parentObservationId"]; hasParent {
			t.Errorf("event %s set parentObservationId; expected siblings under trace", ev.Type)
		}
	}
}

func TestObserver_EndGeneration_UsesLangfuseUsageDetails(t *testing.T) {
	srv, cs := newCaptureServer(t)
	defer srv.Close()

	obs := New(Config{
		Host:          srv.URL,
		PublicKey:     "pk",
		SecretKey:     "sk",
		FlushInterval: 50 * time.Millisecond,
	})

	ctx := context.Background()
	ctx = obs.StartTurn(ctx, protocol.NewUserMessage("u"), nil)
	genCtx := obs.StartGeneration(ctx, observability.GenerationMain, "m", nil, nil, nil)
	obs.EndGeneration(genCtx, observability.GenerationOutput{}, &protocol.Usage{
		InputTokens:              10,
		OutputTokens:             5,
		CacheCreationInputTokens: 30,
		CacheReadInputTokens:     40,
	}, nil)

	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := obs.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var updateBody map[string]any
	for _, ev := range cs.allEvents() {
		if ev.Type != "generation-update" {
			continue
		}
		var ok bool
		updateBody, ok = ev.Body.(map[string]any)
		if !ok {
			t.Fatalf("generation-update body has type %T", ev.Body)
		}
		break
	}
	if updateBody == nil {
		t.Fatal("missing generation-update event")
	}
	if _, ok := updateBody["usage_details"]; ok {
		t.Fatal("generation-update used snake_case usage_details; Langfuse ingestion expects usageDetails")
	}
	if _, ok := updateBody["usage"]; ok {
		t.Fatal("generation-update should not include deprecated usage alongside usageDetails")
	}
	usageDetails, ok := updateBody["usageDetails"].(map[string]any)
	if !ok {
		t.Fatalf("usageDetails missing or wrong type: %#v", updateBody["usageDetails"])
	}
	assertNumber(t, usageDetails, "input", 10)
	assertNumber(t, usageDetails, "output", 5)
	assertNumber(t, usageDetails, "cache_creation_input_tokens", 30)
	assertNumber(t, usageDetails, "cache_read_input_tokens", 40)
	assertNumber(t, usageDetails, "total", 85)
}

func TestObserver_EndGeneration_ReportsZeroCacheUsageDetails(t *testing.T) {
	srv, cs := newCaptureServer(t)
	defer srv.Close()

	obs := New(Config{
		Host:          srv.URL,
		PublicKey:     "pk",
		SecretKey:     "sk",
		FlushInterval: 50 * time.Millisecond,
	})

	ctx := context.Background()
	ctx = obs.StartTurn(ctx, protocol.NewUserMessage("u"), nil)
	genCtx := obs.StartGeneration(ctx, observability.GenerationMain, "m", nil, nil, nil)
	obs.EndGeneration(genCtx, observability.GenerationOutput{}, &protocol.Usage{
		InputTokens:  10,
		OutputTokens: 5,
	}, nil)

	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := obs.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var usageDetails map[string]any
	for _, ev := range cs.allEvents() {
		if ev.Type != "generation-update" {
			continue
		}
		body, ok := ev.Body.(map[string]any)
		if !ok {
			t.Fatalf("generation-update body has type %T", ev.Body)
		}
		usageDetails, ok = body["usageDetails"].(map[string]any)
		if !ok {
			t.Fatalf("usageDetails missing or wrong type: %#v", body["usageDetails"])
		}
		break
	}
	if usageDetails == nil {
		t.Fatal("missing generation-update event")
	}
	assertNumber(t, usageDetails, "input", 10)
	assertNumber(t, usageDetails, "output", 5)
	assertNumber(t, usageDetails, "cache_creation_input_tokens", 0)
	assertNumber(t, usageDetails, "cache_read_input_tokens", 0)
	assertNumber(t, usageDetails, "total", 15)
}

func assertNumber(t *testing.T, m map[string]any, key string, want float64) {
	t.Helper()
	var got float64
	switch v := m[key].(type) {
	case int:
		got = float64(v)
	case float64:
		got = v
	default:
		t.Fatalf("%s has type %T, want number", key, m[key])
	}
	if got != want {
		t.Fatalf("%s = %.0f, want %.0f", key, got, want)
	}
}

func TestNewClient_NilOnMissingCredentials(t *testing.T) {
	if c := NewClient(Config{PublicKey: ""}); c != nil {
		t.Error("NewClient should return nil when PublicKey is empty")
	}
	if c := NewClient(Config{PublicKey: "pk", SecretKey: ""}); c != nil {
		t.Error("NewClient should return nil when SecretKey is empty")
	}
	if got := New(Config{PublicKey: ""}); got == nil {
		t.Error("New must always return a non-nil Observer (Noop fallback)")
	}
}

func TestObserver_DropsOnFullBuffer(t *testing.T) {
	// We don't want telemetry to back-pressure the agent. With a 1-slot
	// buffer and a server that never returns, the second emit must drop
	// instead of blocking.
	blockedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer blockedSrv.Close()

	obs := New(Config{
		Host:          blockedSrv.URL,
		PublicKey:     "pk",
		SecretKey:     "sk",
		BufferSize:    1,
		FlushInterval: 24 * time.Hour, // never tick on its own
	})

	done := make(chan struct{})
	go func() {
		ctx := context.Background()
		for i := 0; i < 100; i++ {
			obs.StartTool(ctx, "t", "{}")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("emit blocked when buffer was full; should drop instead")
	}
}

// TestStatusMessage_StaysShort_EvenForBigToolErrors guards the UX
// regression where a `go test` failure piped through err.Error()
// dumped thousands of stdout lines into Langfuse's statusMessage
// field, overflowing the trace banner and blocking scroll. The full
// error must remain accessible in `output`; only the banner string
// is bounded.
func TestStatusMessage_StaysShort_EvenForBigToolErrors(t *testing.T) {
	srv, cs := newCaptureServer(t)
	defer srv.Close()

	obs := New(Config{
		Host:          srv.URL,
		PublicKey:     "pk",
		SecretKey:     "sk",
		FlushInterval: 50 * time.Millisecond,
	})

	// Mimic a `go test` failure: header line followed by thousands
	// of result tokens. >5KB total in the error itself.
	bigErr := errorString("command exited with code 1: === RUN TestSort\n" +
		strings.Repeat("8 18 23 38 40 60 76 98 99 115 117 ", 200))

	ctx := context.Background()
	ctx = obs.StartTurn(ctx, protocol.NewUserMessage("u"), nil)
	tCtx := obs.StartTool(ctx, "shell", `{"command":"go test ./..."}`)
	obs.EndTool(tCtx, nil, bigErr)
	obs.EndTurn(ctx, nil)

	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := obs.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var spanUpdate map[string]any
	for _, ev := range cs.allEvents() {
		if ev.Type == "span-update" {
			spanUpdate = ev.Body.(map[string]any)
			break
		}
	}
	if spanUpdate == nil {
		t.Fatal("expected a span-update event")
	}

	status, _ := spanUpdate["statusMessage"].(string)
	if status == "" {
		t.Fatal("statusMessage missing on errored tool span")
	}
	if len(status) > statusMessageCap+10 {
		t.Errorf("statusMessage = %d bytes, want <= %d (+ ellipsis); banner would overflow", len(status), statusMessageCap)
	}
	if strings.Contains(status, "\n") {
		t.Errorf("statusMessage contains newline; banner is single-line")
	}

	// Full error must remain in output so the user can still see it.
	output, _ := spanUpdate["output"].(map[string]any)
	if output == nil {
		t.Fatal("output missing on errored tool span")
	}
	fullErr, _ := output["error"].(string)
	if !strings.Contains(fullErr, "98 99 115") {
		t.Error("full error text should be preserved in output.error, not just banner")
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }

// TestStaticTraceMetadata_AppearsOnEveryTraceCreate guards the
// cross-startup grouping: app.go stamps zotigo_session / process_start
// / resumed on every Langfuse session via StaticTraceMetadata so users
// can filter Sessions by metadata.zotigo_session to aggregate one
// logical thread across multiple --resume runs.
func TestStaticTraceMetadata_AppearsOnEveryTraceCreate(t *testing.T) {
	srv, cs := newCaptureServer(t)
	defer srv.Close()

	obs := New(Config{
		Host:          srv.URL,
		PublicKey:     "pk",
		SecretKey:     "sk",
		FlushInterval: 50 * time.Millisecond,
		StaticTraceMetadata: map[string]any{
			"zotigo_session": "sess_abc",
			"process_start":  "2026-04-30T14:30:22Z",
			"resumed":        false,
		},
	})

	ctx := context.Background()
	// Per-turn metadata should win on key collision.
	ctx = obs.StartTurn(ctx, protocol.NewUserMessage("hi"), map[string]any{
		"working_directory": "/tmp/test",
		"resumed":           true, // override static
	})
	obs.EndTurn(ctx, nil)

	closeCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := obs.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var traceCreates []map[string]any
	for _, ev := range cs.allEvents() {
		if ev.Type == "trace-create" {
			traceCreates = append(traceCreates, ev.Body.(map[string]any))
		}
	}
	if len(traceCreates) == 0 {
		t.Fatal("expected at least one trace-create event")
	}

	// Both StartTurn and EndTurn emit trace-create; both must carry
	// the static fields (Langfuse merges by trace id, but missing
	// fields on the update event would clear them in some backends).
	for i, body := range traceCreates {
		md, _ := body["metadata"].(map[string]any)
		if md == nil {
			t.Errorf("trace-create #%d has no metadata", i)
			continue
		}
		if got, _ := md["zotigo_session"].(string); got != "sess_abc" {
			t.Errorf("trace-create #%d zotigo_session = %q, want sess_abc", i, got)
		}
		if got, _ := md["process_start"].(string); got != "2026-04-30T14:30:22Z" {
			t.Errorf("trace-create #%d process_start = %q, want 2026-04-30T14:30:22Z", i, got)
		}
	}

	// Per-turn override: the StartTurn event's `resumed` should be true
	// (overriding the static false). EndTurn doesn't take per-turn
	// metadata so it carries only the static value (false).
	startMeta := traceCreates[0]["metadata"].(map[string]any)
	if got, _ := startMeta["resumed"].(bool); got != true {
		t.Errorf("StartTurn metadata.resumed = %v, want true (per-turn override)", got)
	}
	if got, _ := startMeta["working_directory"].(string); got != "/tmp/test" {
		t.Errorf("StartTurn metadata.working_directory missing per-turn field: %q", got)
	}
}
